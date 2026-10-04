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
    {"name": "thumbnail", "version": "1.2.0", "command": ["/opt/fn/thumbnail/bin/run"], "env": {"MODE": "fast"},
     "artifact_dir": "/opt/fn/thumbnail"}
  ],
  "run_as": {"uid": 2001, "gid": 2001},
  "allow_same_user": false,
  "egress": "none",
  "caps": {
    "max_ttl_ms": 600000,
    "max_wall_ms": 300000,
    "max_cpu_ms": 300000,
    "max_mem_bytes": 536870912,
    "max_output_bytes": 1048576,
    "max_jobs": 1,
    "max_cpu_millis": 1000,
    "max_pids": 64,
    "max_disk_bytes": 67108864,
    "max_io_bps": 67108864,
    "max_io_iops": 1000,
    "max_vm_bytes": 8589934592
  }
}
```

- `functions`: `name` and `version` match a job's `artifact` exactly (the contract's patterns; listed once each);
  `command[0]` is an absolute path. `env` is added to the job's environment. A per-entry `class` (`function`, the
  default, or `code`) is planned and not accepted yet: the config loader refuses unknown keys, so leave it out
  until the `code` class ships; only `function` is advertised in `status.capabilities.worker` today. `artifact_dir` (default: the directory
  of `command[0]`) is the one host directory a job gets besides the system paths, as a **copy** made as the job
  starts, read-only, at the same path: nothing the host adds to it or changes in it later reaches the job (a Unix
  socket or a FIFO that appears there, say, through which a job could reach a host process). The Node opens the
  directory and checks where it leads at that moment; the job's sandbox opens it again by its path, as the job's user
  (who must be able to search every directory above it and list it, or the job fails), checks that it is the
  directory the Node checked (or the job fails), and copies it from there, as the job's user, into a tmpfs of at most `max_disk_bytes` (it counts towards the job's memory, and its reads towards its
  `io.max`; it must be done within the 10 s the sandbox has to stand). The copy holds directories, regular files and
  symlinks (copied as symlinks, never followed) with their permission bits and modification times, without owner,
  set-id or sticky bits; what the job's user may not read is left out; a socket, FIFO or device in the tree, or a
  mount nested in it (a bind mount of the same filesystem included), fails the job. `artifact_dir` must not be `/`;
  nor be, hold or lie in the Node's config or state directory, `/proc`, `/sys`, `/dev`, `/run`, `/var/run`,
  `/var/lock`, `/etc`, `/boot` or `/root`; nor be or hold `/tmp`, `/var/tmp` or `/home`; nor hold a socket, FIFO or
  device at boot (the tree is walked then, so the probe says so before any job). The probe refuses the whole worker
  otherwise, and a job of such a function ends `failed`. A directory in `/usr` (or another system path; the default
  for a command in `/usr/local/bin`, say) is copied too, over what the bind of `/usr` shows there, at the path it leads
  to in the job's root, and the job fails unless its own path to it leads to the copy: point `artifact_dir` at the
  function's own directory, since a copy of all of `/usr/bin` does not fit `max_disk_bytes`. Every directory of a
  system path that its path crosses (as the job follows it: through `/lib` to `/usr/lib` or a symlink in `/usr`, say)
  must belong to root (or the Node's own user) and be writable by no other user, nor by a group with any other member
  (listed in `/etc/group` or by primary group in `/etc/passwd`; a group not in `/etc/group` counts as untrusted), or the probe and
  each job refuse it: there a host user could switch a symlink or rename a directory on the path while the job runs,
  and lead the job to a live host directory instead of its copy. A missing `artifact_dir` does not fail the probe; its
  jobs end `failed`.
- `run_as`: the unprivileged uid and gid jobs run as (not 0; no supplementary groups). It needs the Node to run as
  root, which the system install does. Create a dedicated user for it with no login and no groups.
- `allow_same_user`: run jobs as the Node's own (non-root) user instead, with its groups (`gpio`, `dialout`,
  `video`), on what the job's private root holds: **for development only**. With neither `run_as` nor
  `allow_same_user` the worker stays off.
- `caps`: the owner's local caps. A job's `ttl_ms`, `limits.wall_ms`, `limits.cpu_ms` and `limits.mem_bytes` are
  each clamped to them (the stricter value wins, as with `max_command_ms`). `max_output_bytes` caps the stdout bytes
  one job sends (counted on the wire, after invalid UTF-8 became U+FFFD); `max_jobs` how many run at once. The
  kernel enforces the rest on every job: `max_cpu_millis` (thousandths of a core, cgroup `cpu.max`), `max_pids`
  (processes and threads: `pids.max` and `RLIMIT_NPROC`), `max_disk_bytes` (the size of its `/tmp` and of
  its copy of `artifact_dir`, and `RLIMIT_FSIZE`), `max_io_bps` and `max_io_iops` (`io.max` read and write, on
  every whole block device with a medium (a disk whose `dev` or `size` cannot be read in `/sys/block` fails the job
  and the probe) but the hidden paths of an NVMe multipath disk, which only the limited disk
  over them reaches: read back from the kernel when the job's cgroup is made and again once its sandbox stands; a device that
  cannot be limited, or whose `io.max` is not the job's, fails the job and the probe), `max_vm_bytes`
  (`RLIMIT_AS`, always set: there is no "none"; a runtime that reserves a large address space up front, such as
  Node.js, the JVM or race-instrumented Go, needs a large value, e.g. 64 TiB for the last). `mem_bytes` is the job cgroup's `memory.max` (swap off: `memory.swap.max` is set to 0 and read back; without swap accounting, which makes that
  control, the probe and each job are refused unless the host has no swap at all; an OOM kills the whole job).
- `egress`: what a job may reach on the network. `none` (the default) is the only policy enforced: a network
  namespace with nothing but a loopback that is down. `public` (internet but no private, link-local or Node
  addresses) and `openvibe-only` (the platform's endpoints only) need an egress proxy in the job's namespace that is
  not built yet (follow-up): with either, the probe fails and the worker stays off, never a silent `none` nor an
  open network.

The class `function` — advertised as the reserved Fabric capability `worker:function` in
`status.capabilities.worker` (the `platform.resource-offer@1` namespace OpenVibe.Run routes on) — is offered only when `enabled` is true, the OS is Linux
(amd64 or arm64: the architectures with a seccomp allowlist), at least one `functions` entry is declared and a **boot-time probe** passes: it starts `/bin/sh`
the way a job is started and checks every control from the host: the sandbox was set up; the process has user,
mount, network, PID, IPC, UTS and cgroup namespaces of its own, `NoNewPrivs` and a seccomp filter; nothing in its
root but `/tmp`, `/proc` and the device nodes is mounted writable; it is in its own cgroup with every controller
(cpu, memory, pids, io) and every block device holds its `io.max`; its rlimits (`RLIMIT_AS` included) are set; its
root holds none of `/var`, `/etc/shadow`, `/etc/ssl/private`, `/etc/pki/tls/private` and the Node's directories; its CPU limit can be set and its usage read; `egress` is `none`; no function's `artifact_dir`
is refused (above). If any check fails the Node logs why (`worker off: …`) and refuses every job,
exactly as with the worker off: there is no weaker mode. A job whose sandbox fails at run time ends `failed` without
its function having run.

Requirements: Linux 5.14 or later with cgroup v2; the Node's cgroup **delegated** to it (`Delegate=yes` on its
systemd unit, which the service `openvibe-node install` writes sets; running `install.sh` again rewrites an older
install's service, or add it by hand: `sudo systemctl edit openvibe-node`, add `[Service]` and `Delegate=yes`,
restart). The Node moves itself into an `openvibe-node` leaf of that cgroup and makes one `openvibe-job-*` cgroup per
job beside it; a cgroup that holds any process other than the Node and its children (a login session's, say) is not
rearranged, and the worker stays off. On Ubuntu 24.04 and later, unprivileged user namespaces are restricted by
AppArmor; a Node running as root with `run_as` is not affected.

## How a job runs

1. **Admit** (before the `ack`): refused unless the stop latch is clear, the artifact is declared and fewer than
   `max_jobs` jobs run. An id the worker already holds is answered from it (`ack` while it runs, its `job_exit` once
   it ended) and never starts a second process.
2. **Start**: the Node makes the job's cgroup and starts itself again (`/proc/self/exe`) as the **sandbox init**,
   with its name as its only argument and its spec (the function's command and environment included) in a memfd, so
   that no host user reads the environment in its `/proc/<pid>/cmdline` (the Node checks that it shows the name
   alone), straight into that cgroup (`clone3` `CLONE_INTO_CGROUP`), in its own process group and in new user, mount,
   network, PID, IPC, UTS and cgroup namespaces, with `PR_SET_PDEATHSIG` SIGKILL. The init builds a private root (see
   below) and `pivot_root`s into it, detaching the host's; sets `RLIMIT_CPU` (at `cpu_ms`, rounded up to a second),
   `RLIMIT_CORE` 0, `RLIMIT_NOFILE` 1024, `RLIMIT_NPROC`, `RLIMIT_FSIZE` and `RLIMIT_AS`; drops every capability; sets
   `no_new_privs` and the seccomp filter; reports that it is ready and waits for the Node's go; and only then
   executes the function, with only fds 0-3 open. The function
   works in a fresh directory `/tmp/openvibe-job-*` of its own `/tmp`; its environment is built from scratch
   (`PATH=/usr/local/bin:/usr/bin:/bin`, `HOME` and `TMPDIR` that directory, `LANG=C.UTF-8`, `OPENVIBE_JOB_ID`, and
   the function's `env`; nothing inherited from the Node); `args` come as one line of JSON on stdin; stderr is
   discarded. While the init waits (so it is sure to be alive), the Node checks `NoNewPrivs`, the seccomp filter and
   that nothing but `/tmp`, `/proc` and the device nodes is mounted writable, and sets `RLIMIT_CPU` again from
   outside; only then does it let the init execute the function. `job_started` follows once that `execve`
   succeeded: otherwise the process is killed and the job ends `failed` with no `job_started`.
3. **Run**: stdout streams as `job_stdout` chunks of at most 16 KiB each as sent (invalid UTF-8 becomes U+FFFD before
   it is cut, and a UTF-8 sequence is never split); a watchdog samples CPU time and resident memory of every process in the
   job's PID namespace every 200 ms and sends `job_usage` for every second once it fully elapsed.
4. **End**: the function's result is what it writes to **fd 3** (JSON, at most 256 KiB), reported only when it exits
   0. `job_exit` carries the reason, the exit code (null when it was killed) and the usage, and is resent until
   `job_exit_ack`. The job's cgroup is then killed (`cgroup.kill`) and removed.

Every kill is SIGKILL to the whole process group and to the leader, which is PID 1 of the job's PID namespace, so its
death also kills every process the job left behind. The watchdog kills at `ttl_ms` (from receipt), `wall_ms` (from
start), `cpu_ms`, `mem_bytes`, at `max_output_bytes` of stdout, on `job_cancel`, and the moment the e-stop or the local
stop latches (`openvibe-node stop`); while the latch is set every job is refused, and `openvibe-node resume` admits
them again. A Node shutting down kills its jobs (`stopped`); a job acked but not started yet never starts and gets its
`job_exit` (`stopped`) all the same.

`openvibe-node status` lists the jobs not ended yet under `jobs` (`--json`: `id`, `function` as `name@version`,
`state` `admitted`, `running` or `stopping`, and `started_ms` once running); the list is empty when the worker is off.

## Isolation model and its limits

In six lines:

1. Only owner-declared executables run, matched by exact name and version; the server chooses which and with what JSON.
2. Each job is a separate unprivileged process (`run_as`), with no capability, `no_new_privs` and a seccomp
   allowlist, in its own process group and user, mount, network, PID, IPC, UTS and cgroup namespaces.
3. Its filesystem is a private, read-only root holding only the system paths, a copy of the function's
   `artifact_dir` made as it starts and a size-capped `/tmp` of its own: none of the Node's files, its credential or
   its control socket, nor the host's private keys.
4. The network namespace has only a loopback that is down: no IP network, not even to the Node or `localhost`
   (`egress` `none`; the other policies are not enforced yet, so they keep the worker off).
5. Nothing of the Node's environment reaches it.
6. CPU, memory, processes and disk I/O are enforced by its cgroup v2 and rlimits; ttl, wall, CPU and output also by a
   per-job watchdog that SIGKILLs the whole group; `RLIMIT_CPU` backs the CPU cap per process.

**The private root.** A 16 MiB tmpfs, read-only once built, holding: `/usr`, `/bin`, `/sbin`, `/lib*` (recursive
binds made read-only, `nosuid` and `nodev` with every mount nested in them, by `mount_setattr` `AT_RECURSIVE`;
or the same symlinks on merged-`/usr` systems); from `/etc` only `passwd`, `group`, `hosts`, `nsswitch.conf`,
`localtime`, `mime.types`, `ld.so.*` and `alternatives` (a bind that is, holds or leads by a symlink to
`/etc/ssl/private`, `/etc/pki/tls/private`, `/etc/shadow` or `/var` keeps the worker off); a copy of the public CA
certificates of `/etc/ssl/certs`, `/etc/ssl/cert.pem`, `/etc/ssl/ca-bundle.pem`, `/etc/pki/tls/certs`,
`/etc/pki/tls/cacert.pem`, `/etc/pki/ca-trust/extracted` and `/etc/ca-certificates/extracted` (those that exist),
made for each job as its user and never bound: of each file only the PEM `CERTIFICATE` blocks that parse as X.509
certificates, written again without anything else (a private key filed among them, in a subdirectory or a mount
there included, never reaches a job); a file with none (a binary keystore, a `TRUSTED CERTIFICATE` bundle), one the
job's user may not read, or one over 4 MiB is left out, and symlinks are kept as they are (they lead only to what the
root holds); the copy of the
function's `artifact_dir` (read-only); `/dev/null`, `zero`, `full`, `random`, `urandom` and the `fd`/`stdin`/`stdout`/`stderr` links; a fresh
`/proc` of its own PID namespace; and `/tmp` (also `/dev/shm`), a tmpfs of `max_disk_bytes`, the only writable place.
There is no `/home`, `/root`, `/var`, `/run`, `/srv`, `/mnt`, `/media`, `/sys`, rest of `/etc`, nor the Node's
config or state directory (the Node passes both to the worker and the probe checks them).

**The seccomp filter.** A system call of another architecture (or the x32 ABI) kills or fails; `clone` with a
namespace flag fails, `clone3` fails `ENOSYS` (libc falls back to `clone`); `socket` is limited to Unix, IPv4, IPv6
and netlink (all scoped by the job's network namespace); the calls on the allowlist (files, memory, signals, threads,
time, sockets, polling, `execve`) pass; anything else fails `EPERM`: among them `mount`, `umount`, `pivot_root`,
`chroot`, `unshare`, `setns`, `ptrace`, `process_vm_*`, `bpf`, `perf_event_open`, `keyctl`, `add_key`, `io_uring_*`,
`userfaultfd`, `open_by_handle_at`, the kernel module and `kexec` calls, `reboot`, `swapon`, `quotactl`, the new mount
API, `mbind`/`set_mempolicy` and the `pkey_*` calls.

What it does **not** protect against:

- **`allow_same_user`** (development only): the job keeps the Node user's supplementary groups. It still sees only
  the private root, read-only, and none of the Node's directories.
- **The kernel**: the seccomp allowlist still leaves the common surface (files, memory, sockets, `ioctl`,
  `prctl`) and what an unprivileged user namespace allows; keep the kernel patched.
- **`mem_bytes` and CPU time are also sampled** for the usage report and the `limit` reason; the cgroup enforces
  memory in the kernel (an OOM kills the whole job and it ends `limit`).
- **Sockets and FIFOs under the system paths**: the system paths are bound live, not copied; a standard system keeps
  none under `/usr` or `/lib*` (they live in `/run`, `/tmp` and `/var`, which a job never sees).
- **Every certificate in the CA directories is seen by every job**, CA or not (a host's own public certificate filed
  in `/etc/ssl/certs`, say); its key is not. A certificate Go's X.509 parser refuses is left out of the copy.
- **`io.max` limits block devices only**: I/O to a network filesystem mounted under the system paths is not limited.
- **Zombies**: the function is PID 1 of its namespace; orphans it does not reap count towards `max_pids` until it
  exits.
- **The `args` and the result are trusted to the function**: the worker passes the server's JSON unchanged; validating
  it is the function's job.

Not built yet (follow-ups): the `public` and `openvibe-only` egress policies (an egress proxy reached through a veth
or a socket bound into the job's namespace, filtering by destination).

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
| `limit`     | `wall_ms`, `cpu_ms` (sampled, or `RLIMIT_CPU`'s SIGXCPU) or `mem_bytes` (sampled, or the cgroup's OOM kill) exceeded, stdout past `max_output_bytes`, or a result over 256 KiB |
| `stopped`   | the e-stop or local stop latched, or the Node shut down                                                    |
| `failed`    | the job could not run isolated: sandbox, cgroup or namespaces not set up, spawn error, working directory not made, CPU limit not set; never run with less isolation |
