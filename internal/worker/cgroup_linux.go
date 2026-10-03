//go:build linux

package worker

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/OpenVibers/OpenVibe.Node/internal/config"
	"github.com/OpenVibers/OpenVibe.Node/internal/protocol"
)

const (
	cgroupFS    = "/sys/fs/cgroup"
	cgroup2     = 0x63677270 // CGROUP2_SUPER_MAGIC
	nodeLeaf    = "openvibe-node"
	cpuPeriodUS = 100000
)

// cgroupControllers are the cgroup v2 controllers every job is limited with; without any of them no job runs.
var cgroupControllers = []string{"cpu", "memory", "pids", "io"}

var (
	cgroupOnce sync.Once
	cgroupBase string
	cgroupErr  error
	cgroupSeq  atomic.Uint64
)

// jobCgroupBase is the cgroup v2 directory the job cgroups are made in: the Node's own cgroup, set up once. It needs
// that cgroup delegated to the Node (systemd Delegate=yes, or a Node running as root).
func jobCgroupBase() (string, error) {
	cgroupOnce.Do(func() { cgroupBase, cgroupErr = setupCgroupBase() })
	return cgroupBase, cgroupErr
}

// setupCgroupBase enables the controllers for the children of the Node's own cgroup. cgroup v2 lets a cgroup other
// than the root do that only while it holds no process, so the Node's processes move to a leaf, openvibe-node, first
// (a Node already in that leaf uses its parent). Nothing moves unless every controller is available.
func setupCgroupBase() (string, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(cgroupFS, &st); err != nil || st.Type != cgroup2 {
		return "", errors.New("cgroup v2 is not mounted at " + cgroupFS)
	}
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	rel := ""
	for _, l := range strings.Split(string(b), "\n") {
		if r, ok := strings.CutPrefix(l, "0::"); ok {
			rel = r
		}
	}
	if rel == "" {
		return "", errors.New("the Node is not in a cgroup v2 hierarchy")
	}
	base := filepath.Join(cgroupFS, rel)
	if filepath.Base(base) == nodeLeaf {
		base = filepath.Dir(base)
	}
	avail, err := os.ReadFile(filepath.Join(base, "cgroup.controllers"))
	if err != nil {
		return "", err
	}
	enabled, err := os.ReadFile(filepath.Join(base, "cgroup.subtree_control"))
	if err != nil {
		return "", err
	}
	var missing []string
	for _, c := range cgroupControllers {
		if !hasWord(string(avail), c) {
			return "", fmt.Errorf("the cgroup v2 controller %s is not available in %s (delegate the Node's cgroup to it: systemd Delegate=yes)", c, base)
		}
		if !hasWord(string(enabled), c) {
			missing = append(missing, c)
		}
	}
	if len(missing) == 0 {
		return base, nil
	}
	if base != cgroupFS { // the root cgroup may hold processes and enable controllers all the same
		if err := os.Mkdir(filepath.Join(base, nodeLeaf), 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	for try := 0; ; try++ {
		err := moveProcs(base)
		for _, c := range missing {
			if err == nil {
				err = os.WriteFile(filepath.Join(base, "cgroup.subtree_control"), []byte("+"+c), 0)
			}
		}
		if err == nil {
			return base, nil
		}
		if try == 2 || !errors.Is(err, syscall.EBUSY) { // EBUSY: a process appeared in base meanwhile
			return "", fmt.Errorf("enabling the cgroup v2 controllers in %s: %w", base, err)
		}
	}
}

// moveProcs moves every process of base into its openvibe-node leaf (none from the root cgroup).
func moveProcs(base string) error {
	if base == cgroupFS {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(base, "cgroup.procs"))
	if err != nil {
		return err
	}
	for _, pid := range strings.Fields(string(b)) {
		err := os.WriteFile(filepath.Join(base, nodeLeaf, "cgroup.procs"), []byte(pid), 0)
		if err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
	}
	return nil
}

func hasWord(s, w string) bool {
	for _, f := range strings.Fields(s) {
		if f == w {
			return true
		}
	}
	return false
}

// jobCgroup is one job's cgroup: the job starts in it (clone3 CLONE_INTO_CGROUP) and never leaves it.
type jobCgroup struct {
	dir string
	fd  int
	io  string // the io.max limits every block device must have: "rbps=… wbps=… riops=… wiops=…"
}

// newJobCgroup makes a job's cgroup with its limits: memory.max at the job's mem_bytes (swap off, and an OOM kills
// the whole job), pids.max, cpu.max and io.max on every block device.
func newJobCgroup(caps config.WorkerCaps, lim protocol.JobLimits) (*jobCgroup, error) {
	base, err := jobCgroupBase()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(base, fmt.Sprintf("openvibe-job-%d-%d", os.Getpid(), cgroupSeq.Add(1)))
	if err := os.Mkdir(dir, 0o755); err != nil {
		return nil, err
	}
	cg := &jobCgroup{dir: dir, fd: -1}
	if err := cg.limit(caps, lim); err != nil {
		cg.remove()
		return nil, err
	}
	fd, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		cg.remove()
		return nil, err
	}
	cg.fd = fd
	return cg, nil
}

func (c *jobCgroup) limit(caps config.WorkerCaps, lim protocol.JobLimits) error {
	set := func(file, v string, optional bool) error {
		err := os.WriteFile(filepath.Join(c.dir, file), []byte(v), 0)
		if err != nil && !(optional && errors.Is(err, os.ErrNotExist)) {
			return fmt.Errorf("cgroup %s: %w", file, err)
		}
		return nil
	}
	for _, s := range []struct {
		file, v  string
		optional bool // memory.swap.max exists only with swap accounting
	}{
		{"memory.max", strconv.FormatInt(lim.MemBytes, 10), false},
		{"memory.swap.max", "0", true},
		{"memory.oom.group", "1", false},
		{"pids.max", strconv.FormatInt(caps.MaxPids, 10), false},
		{"cpu.max", fmt.Sprintf("%d %d", caps.MaxCPUMillis*cpuPeriodUS/1000, cpuPeriodUS), false},
	} {
		if err := set(s.file, s.v, s.optional); err != nil {
			return err
		}
	}
	c.io = fmt.Sprintf("rbps=%d wbps=%d riops=%d wiops=%d", caps.MaxIOBps, caps.MaxIOBps, caps.MaxIOPS, caps.MaxIOPS)
	devs, err := blockDevices()
	if err != nil {
		return err
	}
	for _, d := range devs {
		if err := set("io.max", d+" "+c.io, false); err != nil {
			// ENODEV for a disk that is still there is a disk the job's I/O would not be limited on: no job runs.
			if _, serr := os.Stat("/sys/dev/block/" + d); errors.Is(err, syscall.ENODEV) && errors.Is(serr, os.ErrNotExist) {
				continue // unplugged meanwhile: nothing to limit
			}
			return fmt.Errorf("limiting the I/O on block device %s: %w", d, err)
		}
	}
	return c.checkIO()
}

// checkIO reads io.max back and checks that every whole block device with a medium has the job's limits: the kernel
// takes a write it does not apply in full (a value of 0 or 1 leaves that limit off) without failing it.
func (c *jobCgroup) checkIO() error {
	devs, err := blockDevices()
	if err != nil {
		return err
	}
	b, err := os.ReadFile(filepath.Join(c.dir, "io.max"))
	if err != nil {
		return fmt.Errorf("cgroup io.max: %w", err)
	}
	got := map[string][]string{} // "8:0 rbps=… wbps=… riops=… wiops=…", one line per limited device
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) > 0 {
			got[f[0]] = f[1:]
		}
	}
	for _, d := range devs {
		for _, kv := range strings.Fields(c.io) {
			if !slices.Contains(got[d], kv) {
				return fmt.Errorf("block device %s is not limited to %s in cgroup io.max (%q)", d, c.io, strings.Join(got[d], " "))
			}
		}
	}
	return nil
}

// blockDevices lists the major:minor of every whole block device with a medium (io.max takes whole devices only).
// A hidden disk (a path of an NVMe multipath disk) is not one: it has no block device of its own (io.max refuses it
// ENODEV) and is reached only through the visible disk over it, which is limited.
func blockDevices() ([]string, error) { return blockDevicesIn("/sys/block") }

// blockDevicesIn is blockDevices on the sysfs directory sys. A disk whose dev or size cannot be read is one whose
// io.max would go unset and unchecked: it is an error, unless the disk is gone meanwhile (unplugged).
func blockDevicesIn(sys string) ([]string, error) {
	ents, err := os.ReadDir(sys)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		attr := func(name string, optional bool) (string, error) {
			b, err := os.ReadFile(filepath.Join(sys, e.Name(), name))
			if errors.Is(err, fs.ErrNotExist) && optional {
				return "", nil
			}
			return strings.TrimSpace(string(b)), err
		}
		dev, err := attr("dev", false)
		size, serr := attr("size", false)
		hidden, herr := attr("hidden", true) // older kernels have no hidden attribute and no hidden disks
		if err = errors.Join(err, serr, herr); err != nil {
			if _, gone := os.Stat(filepath.Join(sys, e.Name())); errors.Is(gone, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("block device %s: %w", e.Name(), err)
		}
		if dev == "" {
			return nil, fmt.Errorf("block device %s has no major:minor", e.Name())
		}
		if size == "0" || hidden == "1" {
			continue
		}
		out = append(out, dev)
	}
	return out, nil
}

// oomKilled reports whether the kernel's OOM killer ended a process of the job (memory.max reached).
func (c *jobCgroup) oomKilled() bool {
	b, err := os.ReadFile(filepath.Join(c.dir, "memory.events"))
	if err != nil {
		return false
	}
	for _, l := range strings.Split(string(b), "\n") {
		if n, ok := strings.CutPrefix(l, "oom_kill "); ok && n != "0" {
			return true
		}
	}
	return false
}

// remove kills whatever is left in the cgroup and removes it (its last processes may take a moment to be reaped).
func (c *jobCgroup) remove() {
	if c.fd >= 0 {
		syscall.Close(c.fd)
		c.fd = -1
	}
	_ = os.WriteFile(filepath.Join(c.dir, "cgroup.kill"), []byte("1"), 0)
	for i := 0; i < 100; i++ {
		if err := syscall.Rmdir(c.dir); err == nil || errors.Is(err, syscall.ENOENT) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}
