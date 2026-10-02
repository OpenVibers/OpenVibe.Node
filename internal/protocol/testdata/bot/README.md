# OpenVibe.Bot device-contract fixtures

Pinned to **OpenVibe.Bot `d398445479edefa845f0eb32198537434fa11bbd`**.

Each `.json` file is one frame copied verbatim (whitespace and `…` included) from the JSON examples in Bot's
`docs/protocol.md` at that commit:

| file                                                                       | Bot `docs/protocol.md`                        |
|----------------------------------------------------------------------------|-----------------------------------------------|
| `pair`, `paired`, `hello`, `config`, `command_drive`, `ack`, `nack`,       | section 1 (`/device`), "Examples"             |
| `heartbeat`, `heartbeat_ack`, `telemetry`, `status`, `estop_state`, `estop` |                                               |
| `error`                                                                    | section 2 (`/control`): the same `error` frame shape Bot sends on `/device` |

The REST pairing answer has no example in the doc; tests derive it from `paired.json` the way Bot's
`POST /api/v1/pair` handler (`server/api/v1.js`) builds it: `device_id, credential, publish_key, robot_id`
(`robot_ids[0]`), `profile` (see `protocol.PairBodyFromPaired`).

Used by `internal/protocol` (every frame decodes, with the fields the Node reads) and `internal/link`
(`TestBotFixturesEndToEnd`: pair → hello + config → command → ack → reconnect through `internal/fakebot`). When Bot's
contract changes, copy the new examples here, update the SHA above, and let the tests say what moved.
