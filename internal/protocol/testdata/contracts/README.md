# OpenVibe.Contracts job-frame fixtures

Pinned to **OpenVibe.Contracts `93e9bb2`** (v0.92.0, unchanged since `4029434`, PR #7, plan T14 Run): each `.json` file is a verbatim copy of
`fixtures/platform.job-frame/valid/<name>.json`, one frame of `contracts/platform/job-frame.v1.json`
(`platform.job-frame@1`, whose `job` field is `platform.job@1`).

Used by `internal/protocol` (`TestJobFrameFixtures`): every frame decodes, carries the fields the Node reads, and
encodes back to the same JSON object, so the Node's field names (`chunk_seq`, not `seq`, on `job_stdout`) are the
contract's. When the contract changes, copy the new fixtures here, update the SHA above, and let the test say what moved.
