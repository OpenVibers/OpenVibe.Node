# Vibe by voice

Say your machine's call word ("hey vibe" unless you chose another), then ask: Vibe, OpenVibe's assistant, answers out
loud on the machine. The question and the answer are also kept in your "Voice on <machine>" chat on openvibe.chat.

## What leaves the machine

Only what you say **after** the call word. The voice plugin listens on the machine itself and recognises speech
locally. Words before the call word are dropped as they are heard, nothing is recorded, and no audio is sent anywhere.
The text of your question goes to OpenVibe.Bot as a `voice_ask` frame (contracts 0.214.0). Bot asks Vibe as you, the
machine's owner, through OpenVibe.Chat, and sends the answer back as `voice_answer` frames. Answers count against your
daily Vibe allowance like any other.

## Turning it on

1. Add the plugin to the Node's config (`config.json`, `plugins`):

   ```json
   { "name": "voice", "config": { "model": "/home/you/vosk-model-small-en-us-0.15", "lang": "en-US" } }
   ```

2. Install what it uses: `pip install vosk`, a model from https://alphacephei.com/vosk/models (the small one for your
   language is enough), `arecord` (alsa-utils) or `parec` (PulseAudio or PipeWire) for the microphone, and something to
   speak with (`espeak-ng`, or Piper with `piper_model` set).
3. On https://openvibe.bot/machines, press **Let Vibe answer my voice here** on the machine and choose its call word.
   The Node hears about it in Bot's config at once. **Stop Vibe answering my voice here** turns listening off again.

## Plugin settings

| Setting | Default | What |
|---|---|---|
| `engine` | `vosk` | `vosk` (a local model, from the microphone) or `file` (each new line of `heard_file` is one thing heard: tests, or another recogniser feeding it) |
| `model` | | the Vosk model's directory |
| `lang` | | the language you speak, BCP 47 (`en-US`) |
| `speak` | `auto` | `auto` (Piper when `piper_model` is set, else espeak-ng, else spd-say), `none`, or an argv that reads the text on stdin |
| `piper_model` | | a Piper voice (`.onnx`), spoken through `aplay` |
| `notify` | `true` | also show answers as desktop notifications (notify-send) |
| `follow_up_s` | `8` | after the call word alone, how long the next thing you say counts as the question |

The call word counts only when said whole ("hey vibration" is not "hey vibe"). Speech-to-text's other spellings of a
greeting count ("okay vibe", "hi vibe"), as does the plural ("hey vibes").
