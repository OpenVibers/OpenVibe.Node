//go:build linux

package worker

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"github.com/OpenVibers/OpenVibe.Node/internal/config"
)

// clkTck is USER_HZ, the unit of /proc/<pid>/stat times: 100 on every architecture Go runs Linux on.
const clkTck = 100

var pageSize = int64(os.Getpagesize())

// isolate starts cmd in its own process group and in new user, network and PID namespaces: no network but a loopback
// that is down, and when the process exits every process it left behind is killed with it. It runs as worker.run_as
// (with no supplementary groups; the Node must be root) or, with allow_same_user, as the Node's own non-root user.
func isolate(cmd *exec.Cmd, cfg config.WorkerConfig) error {
	uid, gid := os.Geteuid(), os.Getegid()
	attr := &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL,
		Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET | syscall.CLONE_NEWPID}
	switch {
	case cfg.RunAs != nil:
		if uid != 0 {
			return errors.New("worker.run_as needs the Node to run as root")
		}
		r := cfg.RunAs
		attr.UidMappings = []syscall.SysProcIDMap{{ContainerID: int(r.UID), HostID: int(r.UID), Size: 1}}
		attr.GidMappings = []syscall.SysProcIDMap{{ContainerID: int(r.GID), HostID: int(r.GID), Size: 1}}
		attr.GidMappingsEnableSetgroups = true
		attr.Credential = &syscall.Credential{Uid: r.UID, Gid: r.GID} // no Groups: setgroups(0) drops root's groups
	case cfg.AllowSameUser:
		if uid == 0 {
			return errors.New("the Node runs as root: set worker.run_as instead of allow_same_user")
		}
		attr.UidMappings = []syscall.SysProcIDMap{{ContainerID: uid, HostID: uid, Size: 1}}
		attr.GidMappings = []syscall.SysProcIDMap{{ContainerID: gid, HostID: gid, Size: 1}}
	default:
		return errors.New("set worker.run_as (or allow_same_user, for development) to run jobs")
	}
	cmd.SysProcAttr = attr
	return nil
}

// probe starts /bin/sh the way a job is started and checks that it got user, network and PID namespaces of its own,
// that its CPU limit can be set and that its usage can be read.
func probe(cfg config.WorkerConfig, isolate func(*exec.Cmd, config.WorkerConfig) error) error {
	cmd := exec.Command("/bin/sh", "-c", "read x")
	cmd.Env, cmd.Dir = []string{"PATH=/usr/bin:/bin"}, "/"
	if err := isolate(cmd, cfg); err != nil {
		return err
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		in.Close()
		return fmt.Errorf("starting a process in new user, network and PID namespaces: %w", err)
	}
	defer func() { killGroup(cmd.Process); in.Close(); _ = cmd.Wait() }()
	pid := cmd.Process.Pid
	for _, ns := range []string{"user", "net", "pid"} {
		mine, err1 := os.Readlink("/proc/self/ns/" + ns)
		its, err2 := os.Readlink("/proc/" + strconv.Itoa(pid) + "/ns/" + ns)
		if err1 != nil || err2 != nil || mine == its {
			return fmt.Errorf("the probe process did not get a %s namespace of its own (%v, %v)", ns, err1, err2)
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
