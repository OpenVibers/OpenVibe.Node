# OpenVibe.Bot device-contract fixtures

Pinned to **OpenVibe.Bot `b48da7c13dbe8e4ab2e3c2d7b3cf058c67e5fd80`**, except the heartbeat exchange: the
`heartbeat_ack_echo` fixture is Bot `ace1671`, where `heartbeat_ack` echoes the heartbeat's `t` as both `echo` and
`t` over Bot's own envelope `seq`. The older `heartbeat_ack` fixture (the `seq` shape) stays as a decode case.

Each `.json` file is one frame copied verbatim (whitespace and `…` included) from the JSON examples in Bot's
`docs/protocol.md` at that commit:

| file                                                                       | Bot `docs/protocol.md`                        |
|----------------------------------------------------------------------------|-----------------------------------------------|
| `pair`, `paired`, `hello`, `config`, `command_actuator`, `command_drive`,  | section 1 (`/device`), "Examples"             |
| `ack`, `nack`, `heartbeat`, `heartbeat_ack`, `telemetry`, `status`,        |                                               |
| `estop_state`, `estop`                                                     |                                               |
| `error`                                                                    | section 2 (`/control`): the same `error` frame shape Bot sends on `/device` |
| `heartbeat_ack_echo`                                                       | section 1 (`/device`), "Examples" (Bot `ace1671`)                             |

The REST pairing answer has no example in the doc; tests derive it from `paired.json` the way Bot's
`POST /api/v1/pair` handler (`server/api/v1.js`) builds it: `device_id, credential, publish_key, whip_url, robot_id`
(`robot_ids[0]`), `profile` (see `protocol.PairBodyFromPaired`).

Used by `internal/protocol` (every frame decodes, with the fields the Node reads), `internal/link`
(`TestBotFixturesEndToEnd`: pair → hello + config → command → ack → reconnect through `internal/fakebot`) and
`internal/node` (`TestBotFramesDriveAndAck`, `TestExpiredDriveNeverReachesPlugin`). The command examples carry a
2025 deadline: replayed as recorded they are expired, and the node tests move `ts`/`deadline_ms` to now (keeping the
300 ms window) to drive the robot. When Bot's contract changes, copy the new examples here, update the SHA above, and
let the tests say what moved.
