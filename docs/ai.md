# Your own models for your OpenVibe work

If you already run a model at home (Ollama, LM Studio or llama.cpp's server on a gaming PC or a spare box with a GPU),
OpenVibe can use it for your own work: Vibe's answers, an agent's thinking, a summary. It costs nothing, and the prompt
never leaves your network. The machine opens no port: its OpenVibe Node already connects out to OpenVibe.Bot, and the
request comes back over that connection.

## How it works

1. OpenVibe.AI, on your behalf, asks OpenVibe.Bot to run one chat completion on your machine (`bot.command@1` kind
   `ai`, contracts 0.219.0).
2. Bot sends it to your Node, but only for your own machine, and only when you turned on **Let my AI use this
   computer's models** on openvibe.bot/machines. One person's request never goes to another person's machine.
3. The Node's `ai` plugin relays it to your model server, and the reply goes back the same way.

The plugin reaches one place only: the model server in its config. It reads nothing else on the machine and changes
nothing there. It runs one request at a time, because a model is heavy; another request is refused as busy, not
queued. The audit on Bot keeps the model and the token counts, never the text.

## Set it up

Add the plugin to the Node's config (`config.json`):

```json
{ "plugins": [{ "name": "ai" }] }
```

With nothing else set, it uses the first of these that answers:

| Server | Address |
|---|---|
| Ollama | `http://127.0.0.1:11434` |
| LM Studio | `http://127.0.0.1:1234` |
| llama.cpp's server | `http://127.0.0.1:8080` |

It offers every model the server lists. Restart the Node (`openvibe-node restart`), then turn on **Let my AI use this
computer's models** for the machine on openvibe.bot/machines.

| key | default | meaning |
|---|---|---|
| `endpoint` | the first that answers above | the model server's OpenAI-compatible base, without `/v1` |
| `models` | everything the server lists | the models you offer (a list) |
| `max_tokens` | `2048` | the most one reply may be (at most 32768) |
| `timeout_s` | `180` | how long one reply may take (at most 900) |

## The command

`ai` `{"model", "messages": [{"role": "system"|"user"|"assistant", "content"}], "max_tokens"?, "temperature"?}` →
`{model, message: {role: "assistant", content}, usage?, finish_reason?, ms}`.

- At most 200 messages of 64 KB each. A model the machine does not offer is refused (`bad_value`), and so is a request
  while another runs (`not_ready`).
- When no model server answers, the command is `not_ready` and `describe` says `ai.ready: false`.
- It never acts on the machine, so it also runs while the machine's stop is latched.
