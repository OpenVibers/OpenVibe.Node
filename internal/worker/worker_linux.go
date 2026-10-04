//go:build linux

package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/OpenVibers/OpenVibe.Node/internal/config"
	"github.com/OpenVibers/OpenVibe.Node/internal/protocol"
)

// clkTck is USER_HZ, the unit of /proc/<pid>/stat times: 100 on every architecture Go runs Linux on.
const clkTck = 100

var pageSize = int64(os.Getpagesize())

const capSysAdmin = 21

// isolate turns cmd into the start of a sandboxed job (see sandbox_linux.go): the Node's own executable re-run as the
// sandbox init, in its own process group, in new user, mount, network (a loopback that is down, and under egress public
// or openvibe-only a veth, egress_linux.go), PID, IPC, UTS and cgroup namespaces, inside the job's cgroup v2 (made here), with PR_SET_PDEATHSIG SIGKILL. It runs as worker.run_as
// (no supplementary groups; the Node must be root) or, with allow_same_user, as the Node's own non-root user. cmd's
// path, args, env and dir become the function's, run once the sandbox stands; cmd.ExtraFiles holds at most fd 3. The
// function's artifact directory is opened here and copied by the init (stageArtifact).
func isolate(cmd *exec.Cmd, cfg config.WorkerConfig, p plan) (*sandbox, error) {
	if err := policy(cfg); err != nil {
		return nil, err
	}
	attr, err := jobAttr(cfg)
	if err != nil {
		return nil, err
	}
	if len(cmd.ExtraFiles) > 1 {
		return nil, errors.New("isolate: a job has fd 3 only")
	}
	binds, links, err := rootPlan(cfg.NodeDirs)
	if err != nil {
		return nil, err
	}
	caps := cfg.Caps.WithDefaults()
	sb := &sandbox{}
	fail := func(err error) (*sandbox, error) { sb.close(); return nil, err }
	art, err := openArtifact(cfg.NodeDirs, binds, links, p.fn.Artifact())
	if err != nil {
		return fail(err)
	}
	if art != nil {
		sb.child = append(sb.child, art)
	}
	files := make([]*os.File, 1, 4)
	copy(files, cmd.ExtraFiles)
	sr, sw, err := os.Pipe()
	if err != nil {
		return fail(err)
	}
	sb.status, sb.child = sr, append(sb.child, sw)
	gr, gw, err := os.Pipe()
	if err != nil {
		return fail(err)
	}
	sb.goW, sb.child = gw, append(sb.child, gr)
	files = append(files, sw, gr) // statusFD, goFD
	spec := sandboxSpec{Root: p.root, Work: cmd.Dir, Disk: caps.MaxDiskBytes, Links: links,
		Argv: append([]string{cmd.Path}, cmd.Args[1:]...), Env: cmd.Env}
	if art != nil {
		spec.Artifact = p.fn.Artifact()
	}
	for _, b := range binds {
		spec.Binds = append(spec.Binds, bindSpec{Path: b.path, Dev: b.dev})
	}
	sec := uint64((p.limits.CPUMS + 999) / 1000)
	pids, disk := uint64(caps.MaxPids), uint64(caps.MaxDiskBytes)
	spec.Limits = []rlimit{{rlimitCPU, sec, sec + 1}, {rlimitCORE, 0, 0}, {rlimitNOFILE, 1024, 1024},
		{rlimitNPROC, pids, pids}, {rlimitFSIZE, disk, disk}}
	if caps.MaxVMBytes <= 0 {
		return fail(errors.New("isolate: worker.caps.max_vm_bytes must be set"))
	}
	spec.Limits = append(spec.Limits, rlimit{rlimitAS, uint64(caps.MaxVMBytes), uint64(caps.MaxVMBytes)})
	sf, err := specFile(spec)
	if err != nil {
		return fail(err)
	}
	sb.child = append(sb.child, sf)
	files = append(files, sf) // specFD
	if art != nil {
		files = append(files, art) // artifactFD
	}
	if p.egress != "" {
		if sb.egress, err = newEgress(cfg); err != nil {
			return fail(fmt.Errorf("worker.egress %s: %w", p.egress, err))
		}
	}
	if sb.cg, err = newJobCgroup(caps, p.limits); err != nil {
		return fail(err)
	}
	attr.UseCgroupFD, attr.CgroupFD = true, sb.cg.fd
	cmd.Path, cmd.Args, cmd.Env, cmd.Dir = "/proc/self/exe", []string{sandboxArg0}, []string{}, "/"
	cmd.ExtraFiles, cmd.SysProcAttr = files, attr
	return sb, nil
}

// specFile is spec in a memfd, for the sandbox init to read on specFD. Its environment may hold the function's
// secrets: unlike argv (/proc/<pid>/cmdline), a descriptor is open only to who may ptrace the process.
func specFile(spec sandboxSpec) (*os.File, error) {
	b, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	const mfdCloexec = 1
	name := []byte("openvibe-sandbox-spec\x00")
	fd, _, e := syscall.Syscall(sysMemfdCreate, uintptr(unsafe.Pointer(&name[0])), mfdCloexec, 0)
	if e != 0 {
		return nil, fmt.Errorf("memfd_create: %w", e)
	}
	f := os.NewFile(fd, "sandbox spec")
	if _, err := f.Write(b); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// policy refuses what the sandbox cannot enforce: an architecture without a seccomp allowlist. (An egress policy the
// host cannot enforce fails in newEgress.)
func policy(cfg config.WorkerConfig) error {
	_, err := seccompFilter()
	return err
}

// jobAttr is the namespaces and credentials of a job.
func jobAttr(cfg config.WorkerConfig) (*syscall.SysProcAttr, error) {
	uid, gid := os.Geteuid(), os.Getegid()
	attr := &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL, AmbientCaps: []uintptr{capSysAdmin},
		Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS | syscall.CLONE_NEWNET | syscall.CLONE_NEWPID |
			syscall.CLONE_NEWIPC | syscall.CLONE_NEWUTS | syscall.CLONE_NEWCGROUP}
	switch {
	case cfg.RunAs != nil:
		if uid != 0 {
			return nil, errors.New("worker.run_as needs the Node to run as root")
		}
		r := cfg.RunAs
		attr.UidMappings = []syscall.SysProcIDMap{{ContainerID: int(r.UID), HostID: int(r.UID), Size: 1}}
		attr.GidMappings = []syscall.SysProcIDMap{{ContainerID: int(r.GID), HostID: int(r.GID), Size: 1}}
		attr.GidMappingsEnableSetgroups = true
		attr.Credential = &syscall.Credential{Uid: r.UID, Gid: r.GID} // no Groups: setgroups(0) drops root's groups
	case cfg.AllowSameUser:
		if uid == 0 {
			return nil, errors.New("the Node runs as root: set worker.run_as instead of allow_same_user")
		}
		attr.UidMappings = []syscall.SysProcIDMap{{ContainerID: uid, HostID: uid, Size: 1}}
		attr.GidMappings = []syscall.SysProcIDMap{{ContainerID: gid, HostID: gid, Size: 1}}
	default:
		return nil, errors.New("set worker.run_as (or allow_same_user, for development) to run jobs")
	}
	return attr, nil
}

// sandbox is a job's sandbox as the Node holds it: the status pipe of its init, the pipe that lets it execute the
// function, the descriptors the child inherits (closed once it started), its cgroup and its veth (nil under none).
type sandbox struct {
	cg     *jobCgroup
	egress *egressNet
	status *os.File
	goW    *os.File
	child  []*os.File
}

// started closes the Node's copies of what the child inherited.
func (s *sandbox) started() {
	for _, f := range s.child {
		f.Close()
	}
	s.child = nil
}

// ready waits until the sandbox init stands confined, about to execute the function, moves the job's veth into its
// network namespace (under egress public or openvibe-only) and checks from the host that it runs with no_new_privs and the seccomp filter, that nothing but its /tmp, /proc and device nodes is mounted
// writable and that every block device has the job's io.max. The init waits for run meanwhile, so it cannot have
// exited. An error means the function must not run.
func (s *sandbox) ready(pid int) error {
	_ = s.status.SetReadDeadline(time.Now().Add(10 * time.Second))
	var b [1]byte
	if _, err := io.ReadFull(s.status, b[:]); err != nil {
		return fmt.Errorf("the sandbox did not report: %w", err)
	}
	if b[0] != 0 {
		msg, _ := io.ReadAll(s.status)
		return fmt.Errorf("the sandbox could not be set up: %s%s", b[:], msg)
	}
	if s.egress != nil {
		if err := s.egress.attach(pid); err != nil {
			return fmt.Errorf("worker.egress: the job's network: %w", err)
		}
	}
	// The spec, the function's environment included, must not show to every host user in the init's argv.
	if c, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline"); err != nil || string(c) != sandboxArg0+"\x00" {
		return fmt.Errorf("the sandbox init's command line is not its name alone (%q, %v)", c, err)
	}
	st, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return err
	}
	if st := string(st); !strings.Contains(st, "\nNoNewPrivs:\t1\n") || !strings.Contains(st, "\nSeccomp:\t2\n") {
		return errors.New("the job is not confined: no no_new_privs or seccomp filter")
	}
	rw, err := writableMounts(pid)
	if err != nil {
		return err
	}
	if len(rw) > 0 {
		return fmt.Errorf("the job's root has writable mounts: %s", strings.Join(rw, ", "))
	}
	// Again now that the job's root stands: a disk attached since its cgroup was made could back what it mounted.
	return s.cg.checkIO()
}

// run lets the init that ready checked execute the function and waits until it did (its status pipe closed).
func (s *sandbox) run() error {
	_, err := s.goW.Write([]byte{1})
	s.goW.Close()
	if err != nil {
		return fmt.Errorf("starting the function: %w", err)
	}
	_ = s.status.SetReadDeadline(time.Now().Add(10 * time.Second))
	msg, err := io.ReadAll(s.status)
	switch {
	case len(msg) > 0:
		return fmt.Errorf("the function could not start: %s", msg)
	case err != nil:
		return fmt.Errorf("the sandbox did not report: %w", err)
	}
	return nil
}

// oomKilled reports whether memory.max ended the job.
func (s *sandbox) oomKilled() bool { return s.cg != nil && s.cg.oomKilled() }

// close releases the sandbox once its process ended: it kills what is left in its cgroup and removes it.
func (s *sandbox) close() {
	s.started()
	for _, f := range []*os.File{s.status, s.goW} {
		if f != nil {
			f.Close()
		}
	}
	if s.cg != nil {
		s.cg.remove()
	}
	if s.egress != nil {
		s.egress.close()
	}
}

// probe starts /bin/sh the way a job is started and checks every control from the host: the sandbox was set up;
// the process has user, mount, network, PID, IPC, UTS and cgroup namespaces of its own, no_new_privs and the seccomp
// filter; nothing but its /tmp, /proc and device nodes is mounted writable; it is in its job cgroup; its rlimits
// (RLIMIT_AS included: max_vm_bytes must be set) are set; every block device has its io.max; its root holds none of
// the Node's directories nor hiddenPaths (/var, the host's private keys); its CPU limit can be set and its usage read.
// It does so for every policy a job may run under: none (a job whose net is absent, deny or none) and worker.egress, whose veth must
// then stand as the only interface up with the default route through it, the Node's nftables table written. It also
// checks that no declared function's artifact directory is refused. Any failure keeps the class off: there is no
// weaker mode, never none in place of public nor an open network.
func probe(cfg config.WorkerConfig, isolate isolateFunc) error {
	if err := policy(cfg); err != nil {
		return err
	}
	// The host's own policy, the ceiling every job may be offered: a job naming deny or none (or no net at all) runs
	// under no network whatever it says, and one naming public or openvibe-only runs under this, which the probe must be
	// able to enforce (Admit refuses what it cannot).
	egress := cfg.Egress
	if egress == "" {
		egress = config.EgressNone
	}
	if egress != config.EgressNone {
		if _, err := egressReady(); err != nil {
			return fmt.Errorf("worker.egress %s: %w", egress, err)
		}
	}
	binds, links, err := rootPlan(cfg.NodeDirs)
	if err != nil {
		return err
	}
	for _, f := range cfg.Functions {
		err := checkArtifact(cfg.NodeDirs, f.Artifact())
		if err == nil && f.Artifact() != "" {
			err = checkRoute(binds, links, f.Artifact(), trustedUIDs())
		}
		if err == nil && f.Artifact() != "" {
			err = scanArtifact(f.Artifact())
		}
		if err != nil {
			return fmt.Errorf("%s %s@%s: %w", f.EffectiveClass(), f.Name, f.Version, err)
		}
	}
	if err := probeSandbox(cfg, isolate, ""); err != nil {
		return err
	}
	if egress != config.EgressNone {
		return probeSandbox(cfg, isolate, egress)
	}
	return nil
}

// probeSandbox is probe's check of one sandbox, under egress.
func probeSandbox(cfg config.WorkerConfig, isolate isolateFunc, egress string) error {
	root, err := os.MkdirTemp("", "openvibe-probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	if r := cfg.RunAs; r != nil {
		if err := os.Chown(root, int(r.UID), int(r.GID)); err != nil {
			return err
		}
	}
	caps := cfg.Caps.WithDefaults()
	cmd := exec.Command("/bin/sh", "-c", "read x")
	cmd.Env, cmd.Dir = []string{"PATH=/usr/bin:/bin"}, "/tmp"
	sb, err := isolate(cmd, cfg, plan{root: root, limits: protocol.JobLimits{CPUMS: 1000, MemBytes: caps.MaxMemBytes}, egress: egress})
	if err != nil {
		return err
	}
	defer sb.close()
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	err = cmd.Start()
	sb.started()
	if err != nil {
		in.Close()
		return fmt.Errorf("starting a sandboxed process: %w", err)
	}
	defer func() { killGroup(cmd.Process); in.Close(); _ = cmd.Wait() }()
	pid := cmd.Process.Pid
	if err := sb.ready(pid); err != nil {
		return err
	}
	if err := sb.run(); err != nil {
		return err
	}
	proc := "/proc/" + strconv.Itoa(pid)
	for _, ns := range []string{"user", "mnt", "net", "pid", "ipc", "uts", "cgroup"} {
		mine, err1 := os.Readlink("/proc/self/ns/" + ns)
		its, err2 := os.Readlink(proc + "/ns/" + ns)
		if err1 != nil || err2 != nil || mine == its {
			return fmt.Errorf("the probe process did not get a %s namespace of its own (%v, %v)", ns, err1, err2)
		}
	}
	b, err := os.ReadFile(proc + "/cgroup")
	if want := "0::" + strings.TrimPrefix(sb.cg.dir, cgroupFS); err != nil || strings.TrimSpace(string(b)) != want {
		return fmt.Errorf("the probe process is not in its cgroup %s (%q, %v)", want, b, err)
	}
	pids, disk := uint64(caps.MaxPids), uint64(caps.MaxDiskBytes)
	want := map[int]uint64{rlimitNOFILE: 1024, rlimitNPROC: pids, rlimitFSIZE: disk, rlimitAS: uint64(caps.MaxVMBytes)}
	for res, v := range want {
		var got syscall.Rlimit
		if _, _, e := syscall.RawSyscall6(syscall.SYS_PRLIMIT64, uintptr(pid), uintptr(res), 0, uintptr(unsafe.Pointer(&got)), 0, 0); e != 0 || got.Cur != v || got.Max != v {
			return fmt.Errorf("the probe process's rlimit %d is %d/%d, not %d (%v)", res, got.Cur, got.Max, v, e)
		}
	}
	for _, d := range append(append([]string(nil), hiddenPaths...), cfg.NodeDirs...) {
		if _, err := os.Lstat(proc + "/root" + filepath.Clean(d)); !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("the probe process can see %s (%v)", d, err)
		}
	}
	if err := limitCPU(pid, 1000); err != nil {
		return err
	}
	if _, _, err := statOf(pid); err != nil {
		return fmt.Errorf("reading the probe's CPU and memory: %w", err)
	}
	return nil
}

// killGroup SIGKILLs the job's process group while its leader lives, then the leader; the leader is PID 1 of the
// job's PID namespace, so its death also kills anything that left the group.
func killGroup(p *os.Process) {
	if p.Signal(syscall.Signal(0)) == nil {
		_ = syscall.Kill(-p.Pid, syscall.SIGKILL)
	}
	_ = p.Kill()
}

// limitCPU sets the process's RLIMIT_CPU (SIGXCPU at cpuMS rounded up to a second, SIGKILL a second later; every
// process it starts inherits it) and RLIMIT_CORE 0: the kernel's backstop behind the sampled cpu_ms check.
func limitCPU(pid int, cpuMS int64) error {
	sec := uint64((cpuMS + 999) / 1000)
	limits := []struct {
		res int
		lim syscall.Rlimit
	}{{syscall.RLIMIT_CPU, syscall.Rlimit{Cur: sec, Max: sec + 1}}, {syscall.RLIMIT_CORE, syscall.Rlimit{}}}
	for _, l := range limits {
		lim := l.lim
		if _, _, e := syscall.RawSyscall6(syscall.SYS_PRLIMIT64, uintptr(pid), uintptr(l.res), uintptr(unsafe.Pointer(&lim)), 0, 0, 0); e != 0 {
			return fmt.Errorf("prlimit: %w", e)
		}
	}
	return nil
}

func pidNS(pid int) (string, error) { return os.Readlink("/proc/" + strconv.Itoa(pid) + "/ns/pid") }

// sampleProcs sums the CPU time and resident memory of every process in the job's PID namespace ns (the leader alone
// while ns is unknown), so a process that left the process group is still counted.
func sampleProcs(pid int, ns string) (cpuMS, rssBytes int64, err error) {
	if ns == "" {
		return statOf(pid)
	}
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return 0, 0, err
	}
	for _, e := range ents {
		p, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if l, err := os.Readlink("/proc/" + e.Name() + "/ns/pid"); err != nil || l != ns {
			continue
		}
		if c, r, err := statOf(p); err == nil {
			cpuMS, rssBytes = cpuMS+c, rssBytes+r
		}
	}
	return cpuMS, rssBytes, nil
}

// statOf reads one process's CPU time (its own and its reaped children's) and resident memory from /proc.
func statOf(pid int) (cpuMS, rssBytes int64, err error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, err
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, 0, errors.New("malformed /proc stat")
	}
	f := strings.Fields(s[i+1:]) // f[0] is field 3, state
	if len(f) < 22 {
		return 0, 0, errors.New("short /proc stat")
	}
	var ticks int64
	for _, k := range []int{11, 12, 13, 14} { // utime, stime, cutime, cstime
		v, err := strconv.ParseInt(f[k], 10, 64)
		if err != nil {
			return 0, 0, err
		}
		ticks += v
	}
	rss, err := strconv.ParseInt(f[21], 10, 64)
	if err != nil {
		return 0, 0, err
	}
	return ticks * 1000 / clkTck, rss * pageSize, nil
}

// exitStatus is the process's exit code (nil when a signal ended it) and whether RLIMIT_CPU ended it.
func exitStatus(ps *os.ProcessState) (code *int, cpuLimit bool) {
	ws, ok := ps.Sys().(syscall.WaitStatus)
	if !ok {
		return nil, false
	}
	if ws.Exited() {
		c := ws.ExitStatus()
		return &c, false
	}
	return nil, ws.Signaled() && ws.Signal() == syscall.SIGXCPU
}

// maxRSS is the peak resident memory the kernel reports for the process and the children it waited for.
func maxRSS(ps *os.ProcessState) int64 {
	if r, ok := ps.SysUsage().(*syscall.Rusage); ok {
		return int64(r.Maxrss) * 1024
	}
	return 0
}
