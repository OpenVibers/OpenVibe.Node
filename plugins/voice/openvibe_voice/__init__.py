"""OpenVibe Node voice plugin: Vibe by voice on the person's own machine (contracts 0.214.0; consumer plan §8).

The owner lets Vibe answer their voice on this machine and chooses a call word on openvibe.bot/machines; Bot's config
says so (the core passes it on as the ``voice`` op). While it is on, this plugin listens on the machine itself, and
only what the person says *after* the call word leaves it: as a ``voice_ask`` event the core sends to Bot, which asks
Vibe as the owner and sends ``voice_answer`` frames back. The answer is spoken here and shown as a desktop notification,
and kept in the owner's "Voice on <machine>" chat on openvibe.chat.

Nothing is recorded and no audio leaves the machine: recognition runs locally, and the words before the call word are
dropped as they are heard.

Config (the plugin's block in config.json):

* ``engine``: ``vosk`` (the default: a local Vosk model, ``model`` = its directory, audio from ``arecord`` or ``parec``)
  or ``file`` (each new line appended to ``heard_file`` is one thing heard; for tests and for wiring other recognisers).
* ``lang``: the language of the model, BCP 47 (``en-US``).
* ``speak``: ``auto`` (Piper when ``piper_model`` is set, else espeak-ng, else spd-say), ``none``, or an argv list that
  reads the text on stdin (``["sh", "-c", "cat >> said.txt"]``).
* ``notify``: show answers as desktop notifications (notify-send), true by default.
* ``follow_up_s``: after the call word alone, how long the next thing heard counts as the question (8 s).
"""

import json
import os
import shutil
import subprocess
import threading
import time
import unicodedata
import uuid

from openvibe_plugin import Fault, Plugin

VERSION = "0.1.0"
SAMPLE_RATE = 16000
# Speech-to-text writes a spoken greeting several ways (as Live's stream call word, server/ai/vibe-voice.js).
GREETINGS = {"hey", "hi", "hay", "heyy", "ok", "okay", "yo"}


def norm(s):
    """Lower case, accents off, punctuation to spaces, one space between words."""
    s = unicodedata.normalize("NFKD", str(s or "").lower())
    s = "".join(c for c in s if not unicodedata.combining(c))
    return " ".join("".join(c if (c.isalnum() or c in "' ") else " " for c in s).split())


def word_matches(heard, want, first):
    h = heard[:-2] if heard.endswith("'s") else heard
    h, w = h.replace("'", ""), want.replace("'", "")
    if h == w or h == w + "s" or h + "s" == w:
        return True
    return first and w in GREETINGS and h in GREETINGS


def after_call_word(phrase, call_word):
    """The words after the call word (possibly ''), or None when the phrase does not say it (whole words only)."""
    words, want = norm(phrase).split(), norm(call_word).split()
    if not want:
        return None
    for i in range(0, len(words) - len(want) + 1):
        if all(word_matches(words[i + j], want[j], j == 0) for j in range(len(want))):
            return " ".join(words[i + len(want):]).strip(" ,.")
    return None


class Voice(Plugin):
    driver = "voice"
    version = VERSION
    motion_kinds = frozenset()   # it moves nothing

    def __init__(self):
        super().__init__()
        self.on = False
        self.call_word = "hey vibe"
        self.armed_until = 0.0
        self.listener = None
        self.stopping = threading.Event()
        self.speaker_lock = threading.Lock()
        self.cfg = {}
        self.engine = "vosk"

    # ---- describe / setup ----
    def describe(self, config):
        engine = str((config or {}).get("engine") or "vosk")
        return {"voice": {"engine": engine, "speak": self._speaker_name(config or {})}}

    def setup(self, config):
        self.cfg = config or {}
        self.engine = str(self.cfg.get("engine") or "vosk")
        if self.engine == "vosk":
            try:
                import vosk  # noqa: F401
            except ImportError:
                raise Fault("unsupported", "the voice plugin needs Vosk: pip install vosk, and a model in its config (model)")
            if not self.cfg.get("model") or not os.path.isdir(str(self.cfg.get("model"))):
                raise Fault("unsupported", "set model to a Vosk model directory (alphacephei.com/vosk/models, the small one is enough)")
            if not (shutil.which("arecord") or shutil.which("parec")):
                raise Fault("unsupported", "the voice plugin reads the microphone with arecord (alsa-utils) or parec (PulseAudio/PipeWire)")
        elif self.engine == "file":
            if not self.cfg.get("heard_file"):
                raise Fault("bad_value", "engine file needs heard_file")
        else:
            raise Fault("bad_value", "engine is vosk or file")
        # Ask for the owner's setting: the core answers with the voice op (on, call word).
        self.emit_event("voice_ready")

    def handle(self, cmd):
        raise Fault("unsupported", "the voice plugin takes no commands")

    def close(self):
        self._stop_listening()

    # ---- the owner's setting ----
    def on_voice(self, on, call_word):
        self.call_word = norm(call_word) or "hey vibe"
        if on and not self.on:
            self.on = True
            self._start_listening()
            self.log("voice on: listening for %r", self.call_word)
        elif not on and self.on:
            self.on = False
            self._stop_listening()
            self.log("voice off")

    # ---- hearing ----
    def heard(self, phrase, now=None):
        """One finished thing heard on the machine. Returns the question sent to Vibe, or None."""
        if not self.on:
            return None
        now = time.monotonic() if now is None else now
        rest = after_call_word(phrase, self.call_word)
        if rest is None:
            if self.armed_until and now <= self.armed_until and norm(phrase):
                self.armed_until = 0.0
                return self._ask(norm(phrase))
            return None   # not for Vibe: dropped, never sent or kept
        if not rest:
            self.armed_until = now + float(self.cfg.get("follow_up_s") or 8)
            return None
        self.armed_until = 0.0
        return self._ask(rest)

    def _ask(self, text):
        text = text[:2000]
        cid = "va-" + uuid.uuid4().hex[:16]
        fields = {"id": cid, "text": text}
        if self.cfg.get("lang"):
            fields["lang"] = str(self.cfg.get("lang"))
        self.emit_event("voice_ask", **fields)
        return text

    def _start_listening(self):
        self.stopping.clear()
        target = self._listen_file if self.engine == "file" else self._listen_vosk
        self.listener = threading.Thread(target=target, name="voice-listen", daemon=True)
        self.listener.start()

    def _stop_listening(self):
        self.stopping.set()
        self.listener = None

    def _listen_file(self):
        path = str(self.cfg.get("heard_file"))
        pos = os.path.getsize(path) if os.path.exists(path) else 0
        while not self.stopping.is_set():
            try:
                size = os.path.getsize(path) if os.path.exists(path) else 0
                if size < pos:
                    pos = 0
                if size > pos:
                    with open(path, "r", encoding="utf-8", errors="replace") as f:
                        f.seek(pos)
                        data = f.read()
                        pos = f.tell()
                    for line in data.splitlines():
                        if line.strip():
                            self.heard(line)
            except OSError as e:
                self.log("heard_file: %s", e)
            self.stopping.wait(0.1)

    def _listen_vosk(self):
        import vosk
        vosk.SetLogLevel(-1)
        model = vosk.Model(str(self.cfg.get("model")))
        rec = vosk.KaldiRecognizer(model, SAMPLE_RATE)
        argv = (["arecord", "-q", "-f", "S16_LE", "-r", str(SAMPLE_RATE), "-c", "1", "-t", "raw"] if shutil.which("arecord")
                else ["parec", "--format=s16le", "--rate=%d" % SAMPLE_RATE, "--channels=1"])
        proc = subprocess.Popen(argv, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        try:
            while not self.stopping.is_set():
                chunk = proc.stdout.read(4000)
                if not chunk:
                    break
                if rec.AcceptWaveform(chunk):
                    text = json.loads(rec.Result()).get("text", "")
                    if text:
                        self.heard(text)
        finally:
            proc.terminate()

    # ---- answering ----
    def on_voice_answer(self, cid, state, text, url):
        if state == "working":
            return
        if text:
            self.log("Vibe %s (%s)", state, cid)
            self.say(text)
            self._notify(text, url)

    def _speaker_name(self, cfg):
        speak = cfg.get("speak", "auto")
        if isinstance(speak, list):
            return "command"
        if speak == "none":
            return None
        if cfg.get("piper_model") and shutil.which("piper"):
            return "piper"
        for name in ("espeak-ng", "spd-say"):
            if shutil.which(name):
                return name
        return None

    def say(self, text):
        speak = self.cfg.get("speak", "auto")
        if speak == "none":
            return
        if isinstance(speak, list) and speak:
            argv, data = [str(a) for a in speak], text
        else:
            name = self._speaker_name(self.cfg)
            if name == "piper":
                argv, data = ["sh", "-c", 'piper --model "$0" --output-raw | aplay -q -r 22050 -f S16_LE -t raw -', str(self.cfg.get("piper_model"))], text
            elif name == "espeak-ng":
                argv, data = ["espeak-ng", "--stdin"], text
            elif name == "spd-say":
                argv, data = ["spd-say", "-w", text], None
            else:
                self.log("nothing to speak with (install espeak-ng, or set piper_model)")
                return
        with self.speaker_lock:
            try:
                subprocess.run(argv, input=data.encode("utf-8") if data is not None else None, timeout=120, check=False,
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            except (OSError, subprocess.SubprocessError) as e:
                self.log("speaking failed: %s", e)

    def _notify(self, text, url):
        if self.cfg.get("notify", True) is False or not shutil.which("notify-send"):
            return
        body = text if not url else "%s\n%s" % (text, url)
        try:
            subprocess.run(["notify-send", "-a", "OpenVibe", "Vibe", body[:500]], timeout=5, check=False,
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        except (OSError, subprocess.SubprocessError):
            pass
