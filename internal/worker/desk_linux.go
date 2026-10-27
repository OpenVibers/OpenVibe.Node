//go:build linux

package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/OpenVibers/OpenVibe.Node/internal/config"
	"github.com/OpenVibers/OpenVibe.Node/internal/protocol"
)

// DeskSpec is one desk's program (its browser) as RunDesk runs it in the jobs' sandbox, on a desk host that serves
// people other than its owner (Desks plan D5, second part): each desk in its own user, mount, network, PID and IPC
// namespaces, as worker.run_as, inside its own cgroup with its own memory, CPU and process limits, seeing only the
// system paths, its own /tmp (its profile) and the read-only Binds it names, and reaching only the public internet.
type DeskSpec struct {
	Argv []string
	Env  []string
	// Binds are host paths the program needs beyond the system paths: the browser's install directory, /etc/fonts.
	// Read-only, and only under deskBindRoots (CheckDeskBinds).
	Binds     []string
	MemBytes  int64 // memory.max (swap off): an OOM ends the whole desk
	CPUMillis int64 // cpu.max: thousandths of a core per second, 1000 = one core
	DiskBytes int64 // the size of its /tmp, where the profile lives
	Pids      int64 // pids.max
	// Egress is "" (no network) or config.EgressPublic, which needs the host's worker.egress public too.
	Egress string
	// In and Out are the program's fd 3 and fd 4: a browser's --remote-debugging-pipe reads commands on 3 and writes
	// its answers on 4.
	In, Out *os.File
	// Stderr receives the program's stderr (nil: discarded).
	Stderr *os.File
}

// deskBindRoots are where a desk's extra read-only binds may come from: an install directory (a browser in /opt, a
// /usr/local build) and the fonts' configuration. Never /home, /root, /var, /run, the rest of /etc, nor anything that
// is, holds or lies in a Node directory or hiddenPaths (rootPlan's rule).
var deskBindRoots = []string{"/opt", "/usr/local", "/etc/fonts"}

// CheckDeskBinds refuses a bind outside deskBindRoots, a missing one, and one that would show a Node directory or a
// hidden host path; it answers each bind cleaned.
func CheckDeskBinds(nodeDirs, binds []string) ([]string, error) {
	out := make([]string, 0, len(binds))
	for _, b := range binds {
		p := filepath.Clean(b)
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("desk bind %s: not an absolute path", b)
		}
		if !under(p, deskBindRoots) {
			return nil, fmt.Errorf("desk bind %s: only %s may be bound", b, strings.Join(deskBindRoots, ", "))
		}
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("desk bind %s: %w", b, err)
		}
		for _, r := range resolved(p) {
			if d, ok := overlaps(r, nodeDirs); ok {
				return nil, fmt.Errorf("desk bind %s would expose the Node's directory %s", b, d)
			}
			if d, ok := overlaps(r, hiddenPaths); ok {
				return nil, fmt.Errorf("desk bind %s would expose the host's %s", b, d)
			}
		}
		out = append(out, p)
	}
	return out, nil
}

// RunDesk runs d's program confined and waits for it: its exit code, or an error when the sandbox could not be set up
// or checked (the program then never ran). Cancelling ctx ends the desk: everything in its cgroup is killed.
func RunDesk(ctx context.Context, cfg config.WorkerConfig, d DeskSpec) (int, error) {
	if len(d.Argv) == 0 || d.In == nil || d.Out == nil {
		return 0, errors.New("desk: a program and both pipe ends are needed")
	}
	if d.MemBytes <= 0 || d.CPUMillis <= 0 || d.DiskBytes <= 0 || d.Pids <= 0 {
		return 0, errors.New("desk: memory, CPU, disk and process limits must all be set")
	}
	if d.Egress != "" && (d.Egress != config.EgressPublic || cfg.Egress != config.EgressPublic) {
		return 0, fmt.Errorf("desk: egress %q needs egress public here (worker.egress is %q)", d.Egress, cfg.Egress)
	}
	if err := policy(cfg); err != nil {
		return 0, err
	}
	attr, err := jobAttr(cfg)
	if err != nil {
		return 0, err
	}
	extra, err := CheckDeskBinds(cfg.NodeDirs, d.Binds)
	if err != nil {
		return 0, err
	}
	binds, links, err := rootPlan(cfg.NodeDirs)
	if err != nil {
		return 0, err
	}
	// Under /tmp whatever TMPDIR says: a plugin starts desk-sandbox with a desk's own temp folder there, closed to
	// everyone else, and the sandbox mounts its root as worker.run_as, who must be able to reach it.
	root, err := os.MkdirTemp("/tmp", "openvibe-desk-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(root)

	caps := cfg.Caps.WithDefaults()
	caps.MaxPids, caps.MaxCPUMillis, caps.MaxDiskBytes = d.Pids, d.CPUMillis, d.DiskBytes
	sb := &sandbox{}
	defer sb.close()
	sr, sw, err := os.Pipe()
	if err != nil {
		return 0, err
	}
	sb.status, sb.child = sr, append(sb.child, sw)
	gr, gw, err := os.Pipe()
	if err != nil {
		return 0, err
	}
	sb.goW, sb.child = gw, append(sb.child, gr)
	// A browser keeps hundreds of files open and maps tens of GB of address space it never uses: no RLIMIT_AS (the
	// cgroup's memory.max is the limit) and no RLIMIT_CPU (cpu.max is: a desk lives for hours).
	spec := sandboxSpec{Root: root, Work: "/tmp/desk", Disk: d.DiskBytes, Links: links, Argv: d.Argv, Env: d.Env, Pipe: true,
		Limits: []rlimit{{rlimitCORE, 0, 0}, {rlimitNOFILE, 8192, 8192}, {rlimitNPROC, uint64(d.Pids), uint64(d.Pids)},
			{rlimitFSIZE, uint64(d.DiskBytes), uint64(d.DiskBytes)}}}
	if d.Egress != "" {
		spec.Resolvers = cfg.DNS
	}
	for _, b := range binds {
		spec.Binds = append(spec.Binds, bindSpec{Path: b.path, Dev: b.dev})
	}
	for _, p := range extra {
		spec.Binds = append(spec.Binds, bindSpec{Path: p})
	}
	sf, err := specFile(spec)
	if err != nil {
		return 0, err
	}
	sb.child = append(sb.child, sf)
	if d.Egress != "" {
		if sb.egress, err = newEgress(cfg); err != nil {
			return 0, fmt.Errorf("desk egress: %w", err)
		}
	}
	if sb.cg, err = newJobCgroup(caps, protocol.JobLimits{MemBytes: d.MemBytes}); err != nil {
		// Its cgroup is made beside the Node's own leaf, which the Node sets up when its worker is on (the probe);
		// a desk started by a plugin cannot rearrange the Node's cgroup itself.
		return 0, fmt.Errorf("desk cgroup: %w (a desk host runs its Node with worker.enabled, so the cgroup is set up first)", err)
	}
	attr.UseCgroupFD, attr.CgroupFD = true, sb.cg.fd
	cmd := &exec.Cmd{Path: "/proc/self/exe", Args: []string{sandboxArg0}, Env: []string{}, Dir: "/", Stderr: d.Stderr,
		ExtraFiles: []*os.File{d.In, sw, gr, sf, d.Out}, SysProcAttr: attr} // fds 3, 4 status, 5 go, 6 spec, 7 → 4
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("starting the desk's sandbox: %w", err)
	}
	sb.started()
	pid := cmd.Process.Pid
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	fail := func(err error) (int, error) {
		_ = cmd.Process.Kill()
		<-waited
		return 0, err
	}
	if err := sb.ready(pid); err != nil {
		return fail(err)
	}
	if err := sb.run(); err != nil {
		return fail(err)
	}
	select {
	case err := <-waited:
		var ee *exec.ExitError
		switch {
		case err == nil:
			return 0, nil
		case errors.As(err, &ee):
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				return 128 + int(ws.Signal()), nil
			}
			return ee.ExitCode(), nil
		default:
			return 0, err
		}
	case <-ctx.Done():
		_ = cmd.Process.Kill() // the init; close() then kills what is left in the cgroup
		<-waited
		return 128 + int(syscall.SIGKILL), ctx.Err()
	}
}
