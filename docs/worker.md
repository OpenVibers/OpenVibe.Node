# The job worker (`function` jobs)

The Node can run **function jobs** (OpenVibe.Contracts `platform.job@1`, class `function`) sent over the control link
([protocol.md](protocol.md#jobs)). A job is untrusted input: it names a function, and the Node runs it only if the
owner declared that function, at that exact version, in the local config. Nothing is downloaded and no path comes from
the server. The worker is **off by default**; off, the Node refuses every job `class not available`, as before.

## Config

`config.json`, section `worker` (defaults shown; a zero cap means the default):

```json
"worker": {
  "enabled": false,
  "functions": [
    {"name": "thumbnail", "version": "1.2.0", "command": ["/opt/fn/thumbnail/bin/run"], "env": {"MODE": "fast"}}
  ],
  "run_as": {"uid": 2001, "gid": 2001},
  "allow_same_user": false,
  "caps": {
    "max_ttl_ms": 600000,
    "max_wall_ms": 300000,
    "max_cpu_ms": 300000,
    "max_mem_bytes": 536870912,
    "max_output_bytes": 1048576,
    "max_jobs": 1
  }
}
```

- `functions`: `name` and `version` match a job's `artifact` exactly (the contract's patterns; listed once each);
  `command[0]` is an absolute path. `env` is added to the job's environment.
- `run_as`: the unprivileged uid and gid jobs run as (not 0; no supplementary groups). It needs the Node to run as
  root, which the system install does. Create a dedicated user for it with no login and no groups.
- `allow_same_user`: run jobs as the Node's own (non-root) user instead. A job can then read every file the Node can,
  `credential.json` included, and use its groups (`gpio`, `dialout`, `video`): **for development only**. With neither
  `run_as` nor `allow_same_user` the worker stays off.
- `caps`: the owner's local caps. A job's `ttl_ms`, `limits.wall_ms`, `limits.cpu_ms` and `limits.mem_bytes` are
  each clamped to them (the stricter value wins, as with `max_command_ms`). `max_output_bytes` caps one job's stdout;
  `max_jobs` how many run at once.

The class `function` is advertised in `status.capabilities.worker` only when `enabled` is true, the OS is Linux and a
**boot-time probe** passes: it starts `/bin/sh` the way a job is started and checks that the process got user,
network and PID namespaces of its own, that its CPU limit can be set and that its CPU and memory can be read. If the
probe fails the Node logs why (`worker off: …`) and refuses every job, exactly as with the worker off. On Ubuntu 24.04
and later, unprivileged user namespaces are restricted by AppArmor; a Node running as root with `run_as` is not
affected.

## How a job runs

1. **Admit** (before the `ack`): refused unless the stop latch is clear, the artifact is declared and fewer than
   `max_jobs` jobs run. An id the worker already holds is answered from it (`ack` while it runs, its `job_exit` once
   it ended) and never starts a second process.
2. **Start**: a fresh working directory (`$TMPDIR/openvibe-job-*`, owned by `run_as`, removed afterwards); an
   environment built from scratch (`PATH=/usr/local/bin:/usr/bin:/bin`, `HOME` and `TMPDIR` the working directory,
   `LANG=C.UTF-8`, `OPENVIBE_JOB_ID`, and the function's `env`; nothing inherited from the Node); `args` as one line of
   JSON on stdin; stderr discarded. The process starts in its own process group and in new user, network and PID
   namespaces, with `PR_SET_PDEATHSIG` SIGKILL, then gets `RLIMIT_CPU` (at `cpu_ms`, rounded up to a second) and
   `RLIMIT_CORE` 0. `job_started` follows.
3. **Run**: stdout streams as `job_stdout`; a watchdog samples CPU time and resident memory of every process in the
   job's PID namespace every 200 ms and sends `job_usage` for every second once it fully elapsed.
4. **End**: the function's result is what it writes to **fd 3** (JSON, at most 256 KiB), reported only when it exits
   0. `job_exit` carries the reason, the exit code (null when it was killed) and the usage, and is resent until
   `job_exit_ack`.

Every kill is SIGKILL to the whole process group and to the leader, which is PID 1 of the job's PID namespace, so its
death also kills every process the job left behind. The watchdog kills at `ttl_ms` (from receipt), `wall_ms` (from
start), `cpu_ms`, `mem_bytes`, at `max_output_bytes` of stdout, on `job_cancel`, and the moment the e-stop or the local
stop latches (`openvibe-node stop`); while the latch is set every job is refused. A Node shutting down kills its jobs
(`stopped`).

## Isolation model and its limits

In five lines:

1. Only owner-declared executables run, matched by exact name and version; the server chooses which and with what JSON.
2. Each job is a separate unprivileged process (`run_as`) in its own process group and user, network and PID namespaces.
3. The network namespace has only a loopback that is down: no IP network, not even to the Node or `localhost`.
4. Nothing of the Node's environment reaches it; its working directory is fresh and removed after the job.
5. ttl, wall, CPU, memory and output are enforced by a per-job watchdog that SIGKILLs the whole group; `RLIMIT_CPU` backs the CPU cap in the kernel.

What it does **not** protect against (no seccomp, no cgroups, no mount namespace yet):

- **Files**: there is no filesystem isolation. A job reads and writes whatever its uid may: with `run_as`, every
  world-readable file and world-writable directory; with `allow_same_user`, everything the Node can, including
  `credential.json` and the local control socket (so it could clear the local stop).
- **Unix sockets by path** (Docker's, D-Bus's, the Node's control socket) are reachable: a network namespace does not
  cover filesystem sockets; only their file permissions do.
- **The kernel**: no seccomp filter, so the whole syscall surface is open, including what user namespaces expose to
  unprivileged code (a common source of local privilege escalations). Keep the kernel patched.
- **Memory and CPU are sampled**, not enforced by cgroups: a burst between two 200 ms samples can exceed `mem_bytes`
  (and meet the kernel's OOM killer first); the `RLIMIT_CPU` backstop is set just after start and covers each process
  on its own, not their sum.
- **No limit on processes, threads, disk or I/O**: a fork bomb is bounded only by the uid's `RLIMIT_NPROC`, disk use
  in the working directory or `/tmp` only by free space; `/tmp` and `/dev/shm` are shared with the machine.
- **`/proc` is the host's**: a job can list the machine's processes and their command lines (not signal them: its PID
  namespace has no host PIDs).
- **The `args` and the result are trusted to the function**: the worker passes the server's JSON unchanged; validating
  it is the function's job.

## Refusals and exit reasons

Refused with `nack` (keyed by the job id; the first matching row wins, after the checks every Node does, in
[protocol.md](protocol.md#jobs)):

| `message`                             | `fault_code`               | when                                                    |
|---------------------------------------|----------------------------|---------------------------------------------------------|
| `class not available`                 | `unsupported`              | the worker is off (disabled, not Linux, probe failed), or the class is not `function` |
| `the e-stop or local stop is latched` | `estopped` or `local_stop` | the stop latch is set                                   |
| `class not available`                 | `shutting_down`            | the Node is stopping                                    |
| `unknown artifact`                    | `unsupported`              | no declared function has that name and exact version    |
| `worker busy`                         | `not_ready`                | `max_jobs` jobs are running                             |

Accepted jobs end with `job_exit.reason`:

| `reason`    | when                                                                                                      |
|-------------|-----------------------------------------------------------------------------------------------------------|
| `exited`    | the process ended by itself; `code` is its status (null if a signal the worker did not send ended it)      |
| `cancelled` | `job_cancel`; a job cancelled before it started never starts                                               |
| `ttl`       | `ttl_ms` (clamped) ran out, before or while it ran                                                         |
| `limit`     | `wall_ms`, `cpu_ms` (sampled, or `RLIMIT_CPU`'s SIGXCPU) or `mem_bytes` exceeded, stdout past `max_output_bytes`, or a result over 256 KiB |
| `stopped`   | the e-stop or local stop latched, or the Node shut down                                                    |
| `failed`    | the job could not run isolated: namespaces not created, spawn error, working directory not made, CPU limit not set; never run with less isolation |
