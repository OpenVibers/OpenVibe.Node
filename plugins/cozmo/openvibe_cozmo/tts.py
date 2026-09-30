"""Text to speech for Cozmo.

PyCozmo's ``audio.load_wav`` accepts 16-bit WAV at 22050 Hz or 48000 Hz. ``espeak-ng`` writes 22050 Hz mono 16-bit;
``pico2wave`` writes 16000 Hz mono 16-bit, so every synthesized file goes through ``to_cozmo_wav`` which converts to
22050 Hz mono 16-bit with a small pure-Python linear resampler (``audioop`` is gone in Python 3.13).

A synthesizer is any callable ``synth(text, wav_path)`` that writes a WAV file or raises. ``find_synthesizer`` picks one
from the binaries on PATH; tests inject their own.
"""

import shutil
import subprocess
import sys
import wave
from array import array

COZMO_RATE = 22050
SYNTH_TIMEOUT_S = 30


def clean_text(text, max_chars):
    """Printable, single-spaced, length-limited text. A leading '-' is dropped so no TTS binary reads it as a flag."""
    text = "".join(ch if ch.isprintable() else " " for ch in str(text))
    text = " ".join(text.split())[:max_chars]
    return text.lstrip("-").strip()


def espeak_ng(binary="espeak-ng", voice=None, speed=150):
    def synth(text, wav_path):
        cmd = [binary, "-w", wav_path, "-s", str(int(speed)), "--stdin"]
        if voice:
            cmd[1:1] = ["-v", str(voice)]
        subprocess.run(cmd, input=text.encode("utf-8"), stdout=subprocess.DEVNULL, stderr=subprocess.PIPE,
                       timeout=SYNTH_TIMEOUT_S, check=True)
    synth.engine = "espeak-ng"
    return synth


def pico2wave(binary="pico2wave", lang="en-US"):
    def synth(text, wav_path):
        # pico2wave insists on a .wav suffix, which our paths have.
        subprocess.run([binary, "-l", str(lang), "-w", wav_path, text], stdout=subprocess.DEVNULL,
                       stderr=subprocess.PIPE, timeout=SYNTH_TIMEOUT_S, check=True)
    synth.engine = "pico2wave"
    return synth


def find_synthesizer(config, which=shutil.which):
    """Return a synthesizer for the first TTS binary found (espeak-ng, then pico2wave), or None."""
    path = which("espeak-ng")
    if path:
        return espeak_ng(path, voice=config.get("voice"), speed=config.get("speech_rate", 150))
    path = which("pico2wave")
    if path:
        return pico2wave(path, lang=config.get("pico_lang", "en-US"))
    return None


def resample_linear(samples, src_rate, dst_rate):
    """Linear-interpolation resampler for a sequence of ints. Good enough for speech."""
    n = len(samples)
    if src_rate == dst_rate or n == 0:
        return array("h", samples)
    n_out = max(1, int(n * dst_rate / src_rate))
    step = src_rate / dst_rate
    last = n - 1
    out = array("h", bytes(2 * n_out))
    for i in range(n_out):
        pos = i * step
        j = int(pos)
        if j >= last:
            out[i] = samples[last]
            continue
        f = pos - j
        a = samples[j]
        out[i] = int(round(a + (samples[j + 1] - a) * f))
    return out


def to_cozmo_wav(src_path, dst_path, rate=COZMO_RATE):
    """Convert a 16-bit PCM WAV of any rate and channel count to mono 16-bit at ``rate``."""
    with wave.open(src_path, "rb") as w:
        channels = w.getnchannels()
        width = w.getsampwidth()
        src_rate = w.getframerate()
        data = w.readframes(w.getnframes())
    if width != 2:
        raise ValueError("TTS produced %d-bit audio; only 16-bit PCM is supported" % (8 * width))
    samples = array("h")
    samples.frombytes(data[:len(data) - len(data) % 2])
    if sys.byteorder == "big":
        samples.byteswap()
    if channels > 1:
        samples = array("h", (sum(samples[i:i + channels]) // channels
                              for i in range(0, len(samples) - channels + 1, channels)))
    out = resample_linear(samples, src_rate, rate)
    if sys.byteorder == "big":
        out.byteswap()
    with wave.open(dst_path, "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(rate)
        w.writeframes(out.tobytes())
    return len(out)
