//go:build linux

package worker

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/OpenVibers/OpenVibe.Node/internal/config"
	"github.com/OpenVibers/OpenVibe.Node/internal/protocol"
)

// TestHelperProcess is the job's process when this test binary runs as a function (`-- <mode>`); otherwise it does
// nothing.
func TestHelperProcess(t *testing.T) {
	i := slices.Index(os.Args, "--")
	if i < 0 || i+1 >= len(os.Args) {
		return
	}
	result := os.NewFile(3, "result")
	switch os.Args[i+1] {
	case "stream":
		in, _ := io.ReadAll(os.Stdin)
		fmt.Println("line 1")
		time.Sleep(300 * time.Millisecond)
		fmt.Println("line 2 é")
		time.Sleep(2900 * time.Millisecond) // well past the second usage tick, so both are sent even under -race
		fmt.Fprintf(result, `{"got":%s}`, bytes.TrimSpace(in))
	case "sleep":
		fmt.Println("sleeping")
		time.Sleep(time.Hour)
	case "flood":
		line := strings.Repeat("x", 1023) + "\n"
		for {
			os.Stdout.WriteString(line)
		}
	case "badflood": // invalid UTF-8: every lone 0xff is sent as U+FFFD, 3 bytes
		line := strings.Repeat("\xffx", 511) + "\n"
		for {
			os.Stdout.WriteString(line)
		}
	case "env":
		cwd, _ := os.Getwd()
		// A down loopback still lets a socket bind 127.0.0.1; connecting to it is what fails.
		lerr := errors.New("no listener")
		if l, err := net.Listen("tcp", "127.0.0.1:0"); err == nil {
			_, lerr = net.DialTimeout("tcp", l.Addr().String(), time.Second)
		}
		_, derr := net.DialTimeout("tcp", "192.0.2.1:9", time.Second)
		_ = json.NewEncoder(result).Encode(map[string]any{"env": os.Environ(), "cwd": cwd, "pid": os.Getpid(),
			"uid": os.Getuid(), "loopback": fmt.Sprint(lerr), "dial": fmt.Sprint(derr)})
	case "egress": // how each address answers a TCP connect: connected, refused, unreachable or the error
		got := map[string]string{}
		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, a := range []string{"10.0.0.1:80", "169.254.169.254:80", "169.254.240.1:22", "1.1.1.1:53"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c, err := net.DialTimeout("tcp", a, 3*time.Second)
				r := fmt.Sprint(err)
				switch {
				case err == nil:
					c.Close()
					r = "connected"
				case errors.Is(err, syscall.ECONNREFUSED):
					r = "refused"
				case errors.Is(err, syscall.ENETUNREACH):
					r = "unreachable"
				}
				mu.Lock()
				got[a] = r
				mu.Unlock()
			}()
		}
		wg.Wait()
		_ = json.NewEncoder(result).Encode(got)
	case "escape": // every attempt must fail but the write to /tmp; the result is each attempt's error
		node := os.Getenv("OPENVIBE_TEST_NODE_DIR")
		try := map[string]error{}
		_, try["read credential"] = os.ReadFile(node + "/credential.json")
		_, try["list node dir"] = os.ReadDir(node)
		if c, err := net.DialTimeout("unix", node+"/node.sock", time.Second); err == nil {
			c.Close()
		} else {
			try["dial node socket"] = err
		}
		for _, p := range []string{"/var", "/home", "/root", "/sys", "/etc/shadow", "/etc/ssl/private", "/run"} {
			_, try["stat "+p] = os.Stat(p)
		}
		try["write /"] = os.WriteFile("/x", nil, 0o644)
		try["write /usr"] = os.WriteFile("/usr/x", nil, 0o644)
		try["write artifact"] = os.WriteFile(filepath.Join(filepath.Dir(os.Args[0]), "x"), nil, 0o644)
		try["mount"] = syscall.Mount("tmpfs", "/tmp", "tmpfs", 0, "")
		try["unshare"] = syscall.Unshare(syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET)
		try["chroot"] = syscall.Chroot("/tmp")
		if fd, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_RAW, 0); err == nil {
			syscall.Close(fd)
		} else {
			try["packet socket"] = err
		}
		try["write /tmp"] = os.WriteFile("/tmp/x", []byte("ok"), 0o600)
		st, _ := os.ReadFile("/proc/self/status")
		var keys []string // every file of its /etc that holds a PEM private key (symlinks lead to /usr: not followed)
		_ = filepath.WalkDir("/etc", func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() {
				return nil
			}
			if b, err := os.ReadFile(p); err == nil && bytes.Contains(b, []byte("PRIVATE KEY-----")) {
				keys = append(keys, p)
			}
			return nil
		})
		out := map[string]string{"status": string(st), "private keys": strings.Join(keys, " ")}
		for k, err := range try {
			out[k] = fmt.Sprint(err)
		}
		_ = json.NewEncoder(result).Encode(out)
	case "quick":
		fmt.Fprint(result, `{"ok":true}`)
	case "snapshot": // looks for what TestArtifactSnapshot adds to its artifact directory on the host once it started
		dir := filepath.Dir(os.Args[0])
		for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
			if _, err := os.Lstat(filepath.Join(dir, "late")); err == nil {
				break
			}
		}
		try := map[string]error{}
		_, try["late file"] = os.Lstat(filepath.Join(dir, "late"))
		_, try["read through link"] = os.ReadFile(filepath.Join(dir, "link"))
		_, try["stat unreadable"] = os.Lstat(filepath.Join(dir, "unreadable"))
		if c, err := net.DialTimeout("unix", filepath.Join(dir, "s"), time.Second); err == nil {
			c.Close()
		} else {
			try["dial socket"] = err
		}
		if f, err := os.OpenFile(filepath.Join(dir, "f"), os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			f.Close()
		} else {
			try["open FIFO"] = err
		}
		try["write artifact"] = os.WriteFile(filepath.Join(dir, "x"), nil, 0o644)
		out := map[string]string{}
		for k, err := range try {
			out[k] = fmt.Sprint(err)
		}
		_ = json.NewEncoder(result).Encode(out)
	case "stage": // not a job: TestStageArtifactInSystemPath's private root, in a user and mount namespace of its own
		r, sys, dir := os.Args[i+2], os.Args[i+3], os.Args[i+4]
		out := map[string]string{}
		err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, "")
		if err == nil {
			err = syscall.Mount("tmpfs", r, "tmpfs", 0, "size=1m")
		}
		if err == nil {
			err = bind(r, bindSpec{Path: sys})
		}
		var dev uint64
		if err == nil {
			dev, err = stageArtifact(r, dir, 1<<20)
		}
		if err == nil {
			err = os.Symlink(sys, r+"/abs")
		}
		if err != nil {
			fmt.Fprintf(result, "setup: %v\n", err)
			os.Exit(0)
		}
		out["in root"], _ = inRoot(r, "/abs/fn")
		fmt.Fprintln(result, "staged")
		_, _ = io.ReadAll(os.Stdin) // the test adds a socket, a FIFO and a file to the host's directories
		var st syscall.Stat_t
		if err := syscall.Stat(r+dir, &st); err != nil || st.Dev != dev {
			out["copy"] = fmt.Sprintf("the artifact path does not lead to the copy (%v)", err)
		}
		if c, err := net.DialTimeout("unix", r+dir+"/s", time.Second); err == nil {
			c.Close()
			out["dial socket"] = "connected"
		} else {
			out["dial socket"] = err.Error()
		}
		if f, err := os.OpenFile(r+dir+"/f", os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			f.Close()
			out["open FIFO"] = "opened"
		} else {
			out["open FIFO"] = err.Error()
		}
		_, err = os.Lstat(r + dir + "/a")
		out["artifact file"] = fmt.Sprint(err)
		_, err = os.Lstat(r + sys + "/late")
		out["system path file"] = fmt.Sprint(err)
		_ = json.NewEncoder(result).Encode(out)
	case "forkbomb": // starts processes until the kernel refuses one
		n, err := 0, error(nil)
		for ; n < 1000 && err == nil; n++ {
			err = exec.Command("/bin/sleep", "60").Start()
		}
		_ = json.NewEncoder(result).Encode(map[string]any{"started": n - 1, "err": fmt.Sprint(err)})
	}
	os.Exit(0)
}

// newRunWorker is a worker running the helper modes, skipped where the kernel does not let this user create the
// namespaces (CI sets OPENVIBE_WORKER_TESTS=require to fail instead).
func newRunWorker(t *testing.T, caps config.WorkerCaps, modes ...string) (*Worker, *sink) {
	t.Helper()
	if caps.MaxMemBytes == 0 {
		caps.MaxMemBytes = 8 << 30
	}
	return newRunWorkerConfig(t, helperConfig(t, caps, modes...))
}

func newRunWorkerConfig(t *testing.T, cfg config.WorkerConfig) (*Worker, *sink) {
	t.Helper()
	s := &sink{}
	w := New(cfg, s.send, quiet())
	if err := w.Probe(); err != nil {
		if os.Getenv("OPENVIBE_WORKER_TESTS") == "require" {
			t.Fatalf("job isolation unavailable: %v", err)
		}
		t.Skipf("job isolation unavailable here (OPENVIBE_WORKER_TESTS=require fails instead): %v", err)
	}
	t.Cleanup(w.Close)
	return w, s
}

func run(t *testing.T, w *Worker, j protocol.Job) {
	t.Helper()
	if f, r := w.Admit(j); f != "" {
		t.Fatalf("%s refused: %s %s", j.ID, f, r)
	}
	w.Launch(j.ID)
}

// TestRunStreamsAndMeters: a function gets its args on stdin, its stdout arrives as job_stdout in order, one
// job_usage per elapsed wall-clock second anchored on job_started, and job_exit carries code 0, the result it wrote to
// fd 3 and usage covering every second sent. The job_exit is resent until job_exit_ack, then the job is forgotten.
func TestRunStreamsAndMeters(t *testing.T) {
	w, s := newRunWorker(t, config.WorkerCaps{}, "stream")
	j := testJob(1, "stream")
	run(t, w, j)
	ex := s.exit(t, j.ID)
	if ex.Reason != protocol.ExitExited || ex.Code == nil || *ex.Code != 0 || string(ex.Result) != `{"got":{"n":7}}` {
		t.Fatalf("%+v result %s", ex, ex.Result)
	}
	if ex.Usage.StartedMS == nil || ex.Usage.WallMS < 2000 || ex.Usage.CPUMS == nil || ex.Usage.MemPeakBytes == nil || *ex.Usage.MemPeakBytes == 0 {
		t.Fatalf("usage %+v", ex.Usage)
	}
	frames := s.all()
	started, ok := frames[0].(protocol.JobStarted)
	if !ok || started.StartedMS != *ex.Usage.StartedMS {
		t.Fatalf("first frame %+v, want job_started at %d", frames[0], *ex.Usage.StartedMS)
	}
	if _, ok := frames[len(frames)-1].(protocol.JobExit); !ok {
		t.Fatalf("last frame %+v, want job_exit", frames[len(frames)-1])
	}
	var out strings.Builder
	var chunk uint64
	var second int64
	for _, m := range frames {
		switch f := m.(type) {
		case protocol.JobStdout:
			if f.ChunkSeq != chunk+1 {
				t.Fatalf("chunk_seq %d after %d", f.ChunkSeq, chunk)
			}
			chunk = f.ChunkSeq
			out.WriteString(f.Chunk)
		case protocol.JobUsage:
			if f.StartedMS != started.StartedMS || f.Second != second || f.CPUMS == nil {
				t.Fatalf("usage %+v, want second %d at %d", f, second, started.StartedMS)
			}
			second++
		}
	}
	if out.String() != "line 1\nline 2 é\n" {
		t.Fatalf("stdout %q", out.String())
	}
	if second < 2 || second*1000 > ex.Usage.WallMS {
		t.Fatalf("%d usage seconds for wall_ms %d", second, ex.Usage.WallMS)
	}

	if m, ok := w.Known(j.ID); !ok || m.(protocol.JobExit).Reason != protocol.ExitExited {
		t.Fatalf("a resent job is answered %+v, want its job_exit", m)
	}
	w.Resend()
	if again := s.wait(t, protocol.TypeJobExit, j.ID, 2, 5*time.Second).(protocol.JobExit); *again.Usage.StartedMS != *ex.Usage.StartedMS || again.Usage.WallMS != ex.Usage.WallMS {
		t.Fatalf("resent job_exit %+v differs from %+v", again.Usage, ex.Usage)
	}
	w.ExitAck(j.ID)
	if _, ok := w.Known(j.ID); ok {
		t.Fatal("job still held after job_exit_ack")
	}
}

// TestIsolation: the job sees none of the Node's environment, has no network (not even loopback), is PID 1 of its own
// PID namespace, runs as the configured user, and its working directory is fresh and removed afterwards.
func TestIsolation(t *testing.T) {
	t.Setenv("OPENVIBE_NODE_SECRET", "leak")
	w, s := newRunWorker(t, config.WorkerCaps{}, "env")
	j := testJob(1, "env")
	run(t, w, j)
	ex := s.exit(t, j.ID)
	var r struct {
		Env            []string
		Cwd            string
		PID, UID       int
		Loopback, Dial string
	}
	if err := json.Unmarshal(ex.Result, &r); err != nil || ex.Reason != protocol.ExitExited {
		t.Fatalf("%+v %s: %v", ex, ex.Result, err)
	}
	uid := os.Getuid()
	if uid == 0 {
		uid = 65534 // helperConfig's run_as
	}
	if r.PID != 1 || r.UID != uid {
		t.Fatalf("pid %d uid %d, want 1 and %d", r.PID, r.UID, uid)
	}
	if r.Loopback == "<nil>" || r.Dial == "<nil>" {
		t.Fatalf("network reachable: loopback %s, dial %s", r.Loopback, r.Dial)
	}
	for _, kv := range r.Env {
		if strings.HasPrefix(kv, "OPENVIBE_NODE_SECRET=") {
			t.Fatalf("the Node's environment leaked: %v", r.Env)
		}
	}
	if !slices.Contains(r.Env, "OPENVIBE_JOB_ID="+j.ID) || !slices.Contains(r.Env, "HOME="+r.Cwd) {
		t.Fatalf("env %v", r.Env)
	}
	if !strings.HasPrefix(filepath.Base(r.Cwd), "openvibe-job-") {
		t.Fatalf("cwd %s", r.Cwd)
	}
	if _, err := os.Stat(r.Cwd); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("working directory %s not removed: %v", r.Cwd, err)
	}
}

// TestTTLAndLimitKill: ttl_ms (from receipt) and wall_ms (from start) kill the process group, each clamped by the
// local cap when that is stricter; job_exit says ttl or limit with no exit code.
func TestTTLAndLimitKill(t *testing.T) {
	for i, c := range []struct {
		name      string
		caps      config.WorkerCaps
		ttl, wall int64
		reason    string
	}{
		{"ttl", config.WorkerCaps{}, 800, 60000, protocol.ExitTTL},
		{"ttl clamped by the local cap", config.WorkerCaps{MaxTTLMS: 800}, 60000, 60000, protocol.ExitTTL},
		{"wall_ms", config.WorkerCaps{}, 60000, 800, protocol.ExitLimit},
		{"wall_ms clamped by the local cap", config.WorkerCaps{MaxWallMS: 800}, 60000, 60000, protocol.ExitLimit},
	} {
		t.Run(c.name, func(t *testing.T) {
			w, s := newRunWorker(t, c.caps, "sleep")
			j := testJob(i+1, "sleep")
			j.TTLMS, j.Limits.WallMS = c.ttl, c.wall
			begin := time.Now()
			run(t, w, j)
			ex := s.exit(t, j.ID)
			if ex.Reason != c.reason || ex.Code != nil || ex.Usage.StartedMS == nil || s.count(protocol.TypeJobStarted, j.ID) != 1 {
				t.Fatalf("%+v", ex)
			}
			if d := time.Since(begin); d > 5*time.Second {
				t.Fatalf("killed after %s", d)
			}
		})
	}
}

// TestCancelKill: job_cancel kills a running job (cancelled, no code); a cancel after the end resends the unacked
// job_exit and is ignored once acked; a job cancelled before it started never starts.
func TestCancelKill(t *testing.T) {
	w, s := newRunWorker(t, config.WorkerCaps{MaxJobs: 2}, "sleep")
	j := testJob(1, "sleep")
	run(t, w, j)
	s.wait(t, protocol.TypeJobStarted, j.ID, 1, 10*time.Second)
	if !w.Cancel(j.ID) {
		t.Fatal("running job unknown")
	}
	ex := s.exit(t, j.ID)
	if ex.Reason != protocol.ExitCancelled || ex.Code != nil || ex.Usage.StartedMS == nil {
		t.Fatalf("%+v", ex)
	}
	w.Cancel(j.ID)
	s.wait(t, protocol.TypeJobExit, j.ID, 2, 5*time.Second)
	w.ExitAck(j.ID)
	if w.Cancel(j.ID) {
		t.Fatal("cancel of an acked job was not ignored")
	}

	p := testJob(2, "sleep")
	if f, r := w.Admit(p); f != "" {
		t.Fatalf("%s %s", f, r)
	}
	w.Cancel(p.ID)
	w.Launch(p.ID)
	ex = s.exit(t, p.ID)
	if ex.Reason != protocol.ExitCancelled || ex.Usage.StartedMS != nil || ex.Usage.WallMS != 0 || s.count(protocol.TypeJobStarted, p.ID) != 0 {
		t.Fatalf("%+v", ex)
	}
}

// TestOutputCap: stdout past max_output_bytes is cut at the cap and the job is killed (limit); chunks are numbered
// from 1, valid UTF-8 and never larger than chunkBytes. Both bounds count the bytes sent, after invalid UTF-8 became
// U+FFFD: ASCII is cut at exactly the cap, a U+FFFD is never split, so it may stop up to 2 bytes short.
func TestOutputCap(t *testing.T) {
	const capBytes = 64 << 10
	w, s := newRunWorker(t, config.WorkerCaps{MaxOutputBytes: capBytes}, "flood", "badflood")
	for i, mode := range []string{"flood", "badflood"} {
		j := testJob(i+1, mode)
		run(t, w, j)
		ex := s.exit(t, j.ID)
		if ex.Reason != protocol.ExitLimit || ex.Code != nil {
			t.Fatalf("%s: %+v", mode, ex)
		}
		total, seq := 0, uint64(0)
		for _, m := range s.all() {
			if f, ok := m.(protocol.JobStdout); ok && f.ID == j.ID {
				if f.ChunkSeq != seq+1 || len(f.Chunk) > chunkBytes || !utf8.ValidString(f.Chunk) {
					t.Fatalf("%s: chunk %d after %d, %d bytes, valid %v", mode, f.ChunkSeq, seq, len(f.Chunk), utf8.ValidString(f.Chunk))
				}
				seq = f.ChunkSeq
				total += len(f.Chunk)
			}
		}
		if total > capBytes || total <= capBytes-utf8.UTFMax || (mode == "flood" && total != capBytes) {
			t.Fatalf("%s: %d stdout bytes sent, cap %d", mode, total, capBytes)
		}
	}
}

// TestLatchKillsAndRefuses: setting the stop latch kills every running job at once (stopped) and refuses new jobs with
// the latch's fault; once cleared, jobs run again.
func TestLatchKillsAndRefuses(t *testing.T) {
	w, s := newRunWorker(t, config.WorkerCaps{MaxJobs: 2}, "sleep")
	a, b := testJob(1, "sleep"), testJob(2, "sleep")
	run(t, w, a)
	run(t, w, b)
	s.wait(t, protocol.TypeJobStarted, a.ID, 1, 10*time.Second)
	s.wait(t, protocol.TypeJobStarted, b.ID, 1, 10*time.Second)
	w.SetStopped(protocol.FaultEstopped)
	for _, j := range []protocol.Job{a, b} {
		if ex := s.exit(t, j.ID); ex.Reason != protocol.ExitStopped || ex.Code != nil {
			t.Fatalf("%+v", ex)
		}
	}
	if f, r := w.Admit(testJob(3, "sleep")); f != protocol.FaultEstopped || r != protocol.JobStopped {
		t.Fatalf("while latched: %q %q", f, r)
	}
	w.SetStopped("")
	c := testJob(4, "sleep")
	run(t, w, c)
	s.wait(t, protocol.TypeJobStarted, c.ID, 1, 10*time.Second)
	w.Cancel(c.ID)
	if ex := s.exit(t, c.ID); ex.Reason != protocol.ExitCancelled {
		t.Fatalf("%+v", ex)
	}
}

// TestEscape: the job cannot see the Node's directories (a secret file, the control socket), nor /var, /home, /root,
// /sys, /run or the rest of /etc (/etc/ssl/private, the host's TLS keys, included); it cannot write / nor its
// artifact; mount, unshare, chroot and packet sockets are refused; it runs with no_new_privs and the seccomp filter,
// and only its own /tmp is writable.
func TestEscape(t *testing.T) {
	node := t.TempDir()
	if err := os.WriteFile(filepath.Join(node, "credential.json"), []byte(`{"secret":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", filepath.Join(node, "node.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	cfg := helperConfig(t, config.WorkerCaps{MaxMemBytes: 8 << 30}, "escape")
	cfg.NodeDirs = []string{node}
	cfg.Functions[0].Env["OPENVIBE_TEST_NODE_DIR"] = node
	w, s := newRunWorkerConfig(t, cfg)
	j := testJob(1, "escape")
	run(t, w, j)
	ex := s.exit(t, j.ID)
	var r map[string]string
	if err := json.Unmarshal(ex.Result, &r); err != nil || ex.Reason != protocol.ExitExited {
		t.Fatalf("%+v %s: %v", ex, ex.Result, err)
	}
	if !strings.Contains(r["status"], "\nNoNewPrivs:\t1\n") || !strings.Contains(r["status"], "\nSeccomp:\t2\n") {
		t.Fatalf("not confined: %s", r["status"])
	}
	if r["write /tmp"] != "<nil>" {
		t.Fatalf("its /tmp is not writable: %s", r["write /tmp"])
	}
	if r["private keys"] != "" {
		t.Errorf("its /etc holds private keys: %s", r["private keys"])
	}
	delete(r, "write /tmp")
	delete(r, "status")
	delete(r, "private keys")
	if len(r) != 17 {
		t.Fatalf("%d attempts reported: %v", len(r), r)
	}
	for k, v := range r {
		if v == "<nil>" {
			t.Errorf("%s succeeded", k)
		}
		if strings.Contains(k, "node") || strings.Contains(k, "credential") || strings.HasPrefix(k, "stat ") {
			if !strings.Contains(v, "no such file or directory") {
				t.Errorf("%s: %s, want no such file or directory", k, v)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(cfg.Functions[0].Command[0]), "x")); err == nil {
		t.Error("the job wrote into its artifact directory")
	}
}

// TestArtifactNestedMount: a mount nested in the artifact directory is not copied, be it another filesystem or a bind
// mount of the artifact's own (a host directory with a secret, here): the job fails without running (as root only:
// the test mounts on the host).
func TestArtifactNestedMount(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("mounting on the host needs root")
	}
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.Mkdir(secret, 0o755); err != nil {
		t.Fatal(err)
	}
	for i, m := range []struct{ src, fstype string }{{"tmpfs", "tmpfs"}, {filepath.Dir(secret), ""}} {
		cfg := helperConfig(t, config.WorkerCaps{MaxMemBytes: 8 << 30}, "quick")
		nested := filepath.Join(cfg.Functions[0].Artifact(), "nested")
		if err := os.Mkdir(nested, 0o755); err != nil {
			t.Fatal(err)
		}
		flags := uintptr(0)
		if m.fstype == "" {
			flags = syscall.MS_BIND
		}
		if err := syscall.Mount(m.src, nested, m.fstype, flags, "mode=0755,size=1m"); err != nil {
			os.Remove(nested)
			t.Fatal(err)
		}
		w, s := newRunWorkerConfig(t, cfg)
		log := &logBuf{}
		w.log = slog.New(slog.NewTextHandler(log, nil))
		j := testJob(i+1, "quick")
		run(t, w, j)
		ex := s.exit(t, j.ID)
		_ = syscall.Unmount(nested, syscall.MNT_DETACH)
		os.Remove(nested)
		if ex.Reason != protocol.ExitFailed || ex.Usage.StartedMS != nil || s.count(protocol.TypeJobStarted, j.ID) != 0 {
			t.Fatalf("%s mounted in the artifact directory: %+v", m.src, ex)
		}
		if !strings.Contains(log.String(), "is a mount point") {
			t.Fatalf("%s mounted in the artifact directory: the job failed for another reason:\n%s", m.src, log.String())
		}
	}
}

// logBuf is a worker's log, written while its jobs run.
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestArtifactSnapshot: a job runs on a copy of its artifact directory made as it started. A file, a Unix socket and a
// FIFO the host adds to the directory once the job started never appear in it, so a socket or FIFO there cannot reach
// a host process however late it appears; the copy is read-only and the host's directory untouched. A symlink is
// copied as a symlink (one to a host file leads nowhere in the job's root) and a file the job's user may not read is
// left out. That holds for an artifact directory in a system path too (as /usr/local/bin, the default for a command
// there): the copy covers what the bind of that path shows of the host's directory.
func TestArtifactSnapshot(t *testing.T) {
	t.Run("own directory", func(t *testing.T) { artifactSnapshot(t, t.TempDir()) })
	t.Run("in a system path", func(t *testing.T) {
		sys := t.TempDir() // bound in every private root as /usr is
		saved := systemPaths
		systemPaths = append(slices.Clip(systemPaths), sys)
		t.Cleanup(func() { systemPaths = saved })
		dir := filepath.Join(sys, "fn")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Dir(sys), 0o755); err != nil {
			t.Fatal(err)
		}
		artifactSnapshot(t, dir)
	})
}

func artifactSnapshot(t *testing.T, dir string) {
	cfg := helperConfig(t, config.WorkerCaps{MaxMemBytes: 8 << 30}, "snapshot")
	for _, d := range []string{filepath.Dir(dir), dir} { // as root, jobs run as nobody and reach their artifact as nobody
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	moveHelper(t, &cfg, dir)
	host := filepath.Join(t.TempDir(), "host-secret")
	if err := os.WriteFile(host, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(host, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "unreadable"), []byte("x"), 0); err != nil {
		t.Fatal(err)
	}
	w, s := newRunWorkerConfig(t, cfg)
	j := testJob(1, "snapshot")
	run(t, w, j)
	s.wait(t, protocol.TypeJobStarted, j.ID, 1, 10*time.Second)
	l, err := net.Listen("unix", filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := syscall.Mkfifo(filepath.Join(dir, "f"), 0o666); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"s", "f"} { // a job that could see them could use them
		if err := os.Chmod(filepath.Join(dir, p), 0o777); err != nil {
			t.Fatal(err)
		}
	}
	fifo, err := os.OpenFile(filepath.Join(dir, "f"), os.O_RDONLY|syscall.O_NONBLOCK, 0) // a reader, so a writer could open it
	if err != nil {
		t.Fatal(err)
	}
	defer fifo.Close()
	if err := os.WriteFile(filepath.Join(dir, "late"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ex := s.exit(t, j.ID)
	var r map[string]string
	if err := json.Unmarshal(ex.Result, &r); err != nil || ex.Reason != protocol.ExitExited {
		t.Fatalf("%+v %s: %v", ex, ex.Result, err)
	}
	for _, k := range []string{"late file", "dial socket", "open FIFO", "read through link", "stat unreadable"} {
		if !strings.Contains(r[k], "no such file or directory") {
			t.Errorf("%s: %s, want no such file or directory", k, r[k])
		}
	}
	if !strings.Contains(r["write artifact"], "read-only file system") {
		t.Errorf("write artifact: %s, want read-only file system", r["write artifact"])
	}
	if _, err := os.Stat(filepath.Join(dir, "x")); err == nil {
		t.Error("the job wrote into its artifact directory")
	}
}

// TestStageArtifactInSystemPath: an artifact directory a bound system path holds is copied over what the bind shows
// there, at the path it leads to in the private root (through an absolute symlink too), so a socket or FIFO the host
// adds to it once the copy is made is out of reach, while the rest of the bound path still shows the host's directory.
// It needs only a user and a mount namespace, no cgroup: it runs where the sandboxed jobs cannot.
func TestStageArtifactInSystemPath(t *testing.T) {
	r, sys := t.TempDir(), t.TempDir()
	dir := filepath.Join(sys, "fn")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	art, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer art.Close()
	rr, rw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer rr.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$", "--", "stage", r, sys, dir)
	cmd.ExtraFiles = []*os.File{rw, nil, nil, nil, art} // fd 3, the result; fd 7, artifactFD
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Geteuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getegid(), Size: 1}}}
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		rw.Close()
		if os.Getenv("OPENVIBE_WORKER_TESTS") == "require" {
			t.Fatal(err)
		}
		t.Skipf("no user and mount namespace here: %v", err)
	}
	rw.Close()
	defer func() { in.Close(); _ = cmd.Wait() }()
	res := bufio.NewReader(rr)
	if line, err := res.ReadString('\n'); line != "staged\n" {
		t.Fatalf("staging: %q %v", line, err)
	}
	l, err := net.Listen("unix", filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := syscall.Mkfifo(filepath.Join(dir, "f"), 0o666); err != nil {
		t.Fatal(err)
	}
	fifo, err := os.OpenFile(filepath.Join(dir, "f"), os.O_RDONLY|syscall.O_NONBLOCK, 0) // a reader, so a writer could open it
	if err != nil {
		t.Fatal(err)
	}
	defer fifo.Close()
	if err := os.WriteFile(filepath.Join(sys, "late"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	in.Close()
	var out map[string]string
	if err := json.NewDecoder(res).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out["copy"] != "" {
		t.Fatal(out["copy"])
	}
	if want := r + dir; out["in root"] != want {
		t.Errorf("/abs/fn, a symlink to %s/fn, leads to %q in the root, want %s", sys, out["in root"], want)
	}
	for _, k := range []string{"dial socket", "open FIFO"} {
		if !strings.Contains(out[k], "no such file or directory") {
			t.Errorf("%s: %s, want no such file or directory", k, out[k])
		}
	}
	if out["artifact file"] != "<nil>" || out["system path file"] != "<nil>" {
		t.Errorf("the copy lacks the artifact's file (%s), or the bound system path does not show the host's (%s)",
			out["artifact file"], out["system path file"])
	}
}

// TestOwnProcess: the Node moves only itself and its descendants out of its cgroup (moveProcs).
func TestOwnProcess(t *testing.T) {
	cmd := exec.Command("sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()
	defer cmd.Process.Kill()
	if !ownProcess(os.Getpid()) || !ownProcess(cmd.Process.Pid) {
		t.Error("the Node or its child is not the Node's")
	}
	if ownProcess(1) || ownProcess(os.Getppid()) {
		t.Error("init or the Node's parent is the Node's")
	}
}

// accounts points trustedGroup at these passwd and group files for the rest of the test.
func accounts(t *testing.T, passwd, group string) {
	dir := t.TempDir()
	p, g := filepath.Join(dir, "passwd"), filepath.Join(dir, "group")
	if err := os.WriteFile(p, []byte(passwd), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(g, []byte(group), 0o644); err != nil {
		t.Fatal(err)
	}
	op, og := passwdFile, groupFile
	passwdFile, groupFile = p, g
	t.Cleanup(func() { passwdFile, groupFile = op, og })
}

// TestTrustedGroup: a group is trusted only when every member, listed or by primary group, is a trusted user.
func TestTrustedGroup(t *testing.T) {
	passwd := "root:x:0:0::/root:/bin/sh\nnode:x:1000:1000::/:/bin/sh\nops:x:1001:0::/:/bin/sh\nbob:x:1002:1002::/:/bin/sh\n"
	accounts(t, passwd, "root:x:0:\nnode:x:1000:\nwheel:x:10:root,bob\nstaff:x:50:node,ghost\nbob:x:1002:\n")
	for _, c := range []struct {
		gid  uint32
		want bool
	}{
		{0, false},    // ops (1001) has root's group as its primary group
		{1000, true},  // the Node's own
		{10, false},   // bob is listed
		{50, false},   // ghost is no known user
		{1002, false}, // bob's primary group
		{77, false},   // not listed
	} {
		if got := trustedGroup(c.gid, []uint32{0, 1000}); got != c.want {
			t.Errorf("trustedGroup(%d) = %v, want %v", c.gid, got, c.want)
		}
	}
	if !trustedGroup(0, []uint32{0, 1000, 1001}) {
		t.Error("root's group with trusted members only: not trusted")
	}
}

// TestArtifactRoute: an artifact directory whose path crosses a directory of a bound system path that a host user
// other than root or the Node's own could change is refused, as the Node opens it for a job and at boot: in such a
// directory a host writer could, after every check, switch a symlink on the path (or rename a directory on it) so that
// the job's path leads to a live host directory holding a socket or FIFO. A path that leaves the bound paths is the
// private root's own, writable by anyone on the host or not.
func TestArtifactRoute(t *testing.T) {
	sys := t.TempDir() // bound in every private root as /usr is
	for _, d := range []string{"real", "pub"} {
		if err := os.Mkdir(filepath.Join(sys, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(sys, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(sys, "fn")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../real", filepath.Join(sys, "pub", "fn")); err != nil {
		t.Fatal(err)
	}
	binds, links := []hostBind{{path: sys}}, []linkSpec{{"/abs", sys}}
	for _, a := range []string{sys + "/real", sys + "/fn", "/abs/fn", "/abs/pub/../real", sys + "/pub/fn"} {
		if err := checkRoute(binds, links, a, trustedUIDs()); err != nil {
			t.Errorf("artifact %s on a path only its owner may change: %v", a, err)
		}
	}
	for _, a := range []string{sys + "/missing", sys + "/real/missing/deeper"} {
		if err := checkRoute(binds, links, a, trustedUIDs()); err != nil {
			t.Errorf("missing artifact %s (the job fails, the probe does not): %v", a, err)
		}
	}
	if err := checkRoute(binds, links, sys+"/real", []uint32{uint32(os.Geteuid()) + 1}); err == nil ||
		!strings.Contains(err.Error(), "crosses "+sys+",") {
		t.Errorf("artifact in a directory another user owns: %v, want refused", err)
	}
	// A group-writable directory: refused while a member of its group is not trusted, root's group included.
	if err := os.Chmod(sys, 0o775); err != nil {
		t.Fatal(err)
	}
	var st syscall.Stat_t
	if err := syscall.Stat(sys, &st); err != nil {
		t.Fatal(err)
	}
	gid, me := int(st.Gid), os.Geteuid()
	accounts(t, fmt.Sprintf("root:x:0:0::/root:/bin/sh\nnode:x:%d:%d::/:/bin/sh\nmallory:x:%d:100::/:/bin/sh\n", me, gid, me+1),
		fmt.Sprintf("root:x:0:\nnodes:x:%d:node,mallory\n", gid))
	if err := checkRoute(binds, links, sys+"/real", trustedUIDs()); err == nil || !strings.Contains(err.Error(), "crosses "+sys+",") {
		t.Errorf("artifact in a directory a group with an untrusted member may write: %v, want refused", err)
	}
	accounts(t, fmt.Sprintf("root:x:0:0::/root:/bin/sh\nnode:x:%d:%d::/:/bin/sh\n", me, gid), fmt.Sprintf("root:x:0:\nnodes:x:%d:node\n", gid))
	if err := checkRoute(binds, links, sys+"/real", trustedUIDs()); err != nil {
		t.Errorf("artifact in a directory only trusted users' group may write: %v", err)
	}
	if err := os.Chmod(sys, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(sys, "pub"), 0o777); err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{sys + "/pub/fn", "/abs/pub/fn", sys + "/pub/fn/sub"} {
		if err := checkRoute(binds, links, a, trustedUIDs()); err == nil || !strings.Contains(err.Error(), "crosses "+sys+"/pub,") {
			t.Errorf("artifact %s through a symlink in a world-writable directory: %v, want refused", a, err)
		}
	}
	if f, err := openArtifact(nil, binds, links, sys+"/pub/fn"); err == nil || !strings.Contains(err.Error(), "could change") {
		if f != nil {
			f.Close()
		}
		t.Errorf("openArtifact %s/pub/fn: %v, want refused", sys, err)
	}
	priv := t.TempDir()
	if err := os.Chmod(priv, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := checkRoute(binds, links, priv+"/fn", trustedUIDs()); err != nil {
		t.Errorf("artifact outside the bound paths: %v", err)
	}
	binds, links, err := rootPlan(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkRoute(binds, links, "/usr/bin", trustedUIDs()); usrTrusted(t) != (err == nil) {
		t.Errorf("/usr/bin: %v", err)
	}
	cfg := helperConfig(t, config.WorkerCaps{}, "env")
	cfg.Functions[0].ArtifactDir = sys + "/pub/fn"
	saved := systemPaths
	systemPaths = append(slices.Clip(systemPaths), sys)
	t.Cleanup(func() { systemPaths = saved })
	w := New(cfg, (&sink{}).send, quiet())
	t.Cleanup(w.Close)
	if err := w.Probe(); err == nil || !strings.Contains(err.Error(), "could change") {
		t.Fatalf("probe with artifact_dir %s/pub/fn: %v, want refused", sys, err)
	}
}

// TestArtifactRouteSwitched: a directory on a job's path to its artifact, in a bound system path, became
// world-writable after boot (the probe passed): the job is refused before it starts, so when the host then switches
// the symlink there to a host directory holding a socket and a FIFO, nothing reaches them. A switch while a job runs
// meets its private copy instead (TestArtifactSnapshot, TestStageArtifactInSystemPath).
func TestArtifactRouteSwitched(t *testing.T) {
	sys := t.TempDir() // bound in every private root as /usr is
	saved := systemPaths
	systemPaths = append(slices.Clip(systemPaths), sys)
	t.Cleanup(func() { systemPaths = saved })
	cfg := helperConfig(t, config.WorkerCaps{MaxMemBytes: 8 << 30}, "snapshot")
	real, other, pub := filepath.Join(sys, "real"), filepath.Join(sys, "other"), filepath.Join(sys, "pub")
	for _, d := range []string{real, other, pub} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{filepath.Dir(sys), sys} {
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	moveHelper(t, &cfg, real)
	if err := os.Link(cfg.Functions[0].Command[0], filepath.Join(other, "worker.test")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(pub, "fn")
	if err := os.Symlink("../real", link); err != nil {
		t.Fatal(err)
	}
	cfg.Functions[0].ArtifactDir, cfg.Functions[0].Command[0] = link, filepath.Join(link, "worker.test")
	w, s := newRunWorkerConfig(t, cfg) // the probe passes: only the Node's user may change the path yet
	if err := os.Chmod(pub, 0o777); err != nil {
		t.Fatal(err)
	}
	log := &logBuf{}
	w.log = slog.New(slog.NewTextHandler(log, nil))
	j := testJob(1, "snapshot")
	run(t, w, j)
	l, err := net.Listen("unix", filepath.Join(other, "s"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := syscall.Mkfifo(filepath.Join(other, "f"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(other, "s"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../other", link+".new"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(link+".new", link); err != nil {
		t.Fatal(err)
	}
	ex := s.exit(t, j.ID)
	if ex.Reason != protocol.ExitFailed || ex.Usage.StartedMS != nil || s.count(protocol.TypeJobStarted, j.ID) != 0 {
		t.Fatalf("a job whose artifact path a host user could switch: %+v %s", ex, ex.Result)
	}
	if !strings.Contains(log.String(), "could change") {
		t.Fatalf("the job failed for another reason:\n%s", log.String())
	}
}

// moveHelper makes the directory dir the artifact directory of cfg's function: the test binary is linked (or copied)
// there and run from there.
func moveHelper(t *testing.T, cfg *config.WorkerConfig, dir string) {
	t.Helper()
	exe := filepath.Join(dir, "worker.test")
	if err := os.Link(cfg.Functions[0].Command[0], exe); err != nil {
		b, err := os.ReadFile(cfg.Functions[0].Command[0])
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(exe, b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg.Functions[0].Command[0] = exe
}

// TestArtifactUnreachable: a job's user must reach its artifact directory by its path: one under a directory it may
// not search fails the job without running, though the Node (root) opens it; once that directory may be searched, the
// job runs (as root only: otherwise jobs run as the Node's own user, who could not open it either).
func TestArtifactUnreachable(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("jobs run as the Node's own user unless it is root")
	}
	cfg := helperConfig(t, config.WorkerCaps{MaxMemBytes: 8 << 30}, "quick")
	top := t.TempDir()
	if err := os.Chmod(filepath.Dir(top), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(top, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(top, "fn")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	moveHelper(t, &cfg, dir)
	w, s := newRunWorkerConfig(t, cfg)
	log := &logBuf{}
	w.log = slog.New(slog.NewTextHandler(log, nil))
	j := testJob(1, "quick")
	run(t, w, j)
	ex := s.exit(t, j.ID)
	if ex.Reason != protocol.ExitFailed || ex.Usage.StartedMS != nil || s.count(protocol.TypeJobStarted, j.ID) != 0 {
		t.Fatalf("an artifact directory its user may not reach: %+v", ex)
	}
	if !strings.Contains(log.String(), "permission denied") {
		t.Fatalf("an artifact directory its user may not reach: the job failed for another reason:\n%s", log.String())
	}
	if err := os.Chmod(top, 0o711); err != nil {
		t.Fatal(err)
	}
	j = testJob(2, "quick")
	run(t, w, j)
	if ex := s.exit(t, j.ID); ex.Reason != protocol.ExitExited || string(ex.Result) != `{"ok":true}` {
		t.Fatalf("an artifact directory its user may reach: %+v %s", ex, ex.Result)
	}
}

// TestIOMaxVerified: a job's cgroup holds io.max for every whole block device with a medium, read back from the
// kernel, and a device whose io.max is not the job's (as when the kernel ignores a value) keeps the job from running.
func TestIOMaxVerified(t *testing.T) {
	newRunWorker(t, config.WorkerCaps{}, "quick") // skips where jobs cannot run sandboxed
	caps := config.WorkerCaps{MaxIOBps: 3 << 20, MaxIOPS: 300}.WithDefaults()
	cg, err := newJobCgroup(caps, protocol.JobLimits{MemBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer cg.remove()
	devs, err := blockDevices()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(cg.dir, "io.max"))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range devs {
		if !strings.Contains(string(b), d+" rbps=3145728 wbps=3145728 riops=300 wiops=300\n") {
			t.Errorf("block device %s is not limited in io.max:\n%s", d, b)
		}
	}
	if len(devs) == 0 {
		t.Skip("no block device to limit")
	}
	cg.io = "rbps=3145728 wbps=3145728 riops=301 wiops=300"
	if err := cg.checkIO(); err == nil || !strings.Contains(err.Error(), devs[0]) {
		t.Fatalf("io.max that is not the job's: %v, want refused", err)
	}
}

// TestBlockDevicesUnreadable: a disk whose dev (or size) cannot be read is an error, never a disk left without io.max;
// an empty disk and a hidden one are not limited; a disk without a hidden attribute (an older kernel) is.
func TestBlockDevicesUnreadable(t *testing.T) {
	sys := t.TempDir()
	disk := func(name string, attrs map[string]string) {
		if err := os.Mkdir(filepath.Join(sys, name), 0o755); err != nil {
			t.Fatal(err)
		}
		for k, v := range attrs {
			if err := os.WriteFile(filepath.Join(sys, name, k), []byte(v+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	disk("sda", map[string]string{"dev": "8:0", "size": "100", "hidden": "0"})
	disk("sr0", map[string]string{"dev": "11:0", "size": "0", "hidden": "0"})
	disk("nvme0c0n1", map[string]string{"dev": "259:1", "size": "100", "hidden": "1"})
	disk("vda", map[string]string{"dev": "252:0", "size": "100"})
	devs, err := blockDevicesIn(sys)
	if err != nil || !slices.Equal(devs, []string{"8:0", "252:0"}) {
		t.Fatalf("block devices %v, %v; want [8:0 252:0]", devs, err)
	}
	// dev a directory: unreadable even by root.
	disk("sdb", map[string]string{"size": "100", "hidden": "0"})
	if err := os.Mkdir(filepath.Join(sys, "sdb", "dev"), 0o755); err != nil {
		t.Fatal(err)
	}
	if devs, err := blockDevicesIn(sys); err == nil || !strings.Contains(err.Error(), "sdb") {
		t.Fatalf("a disk whose dev cannot be read: %v, %v; want an error", devs, err)
	}
	if err := os.Remove(filepath.Join(sys, "sdb", "dev")); err != nil {
		t.Fatal(err)
	}
	if devs, err := blockDevicesIn(sys); err == nil || !strings.Contains(err.Error(), "sdb") {
		t.Fatalf("a disk without dev: %v, %v; want an error", devs, err)
	}
}

// TestCAStaged: of the CA directories, a job's root gets only the X.509 certificates, each file rewritten with its
// CERTIFICATE blocks alone: a private key filed with them (alone, in a bundle, in a subdirectory or in a mount nested
// there, as root) and a file holding no certificate never reach it; symlinks are kept as they are.
func TestCAStaged(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	priv := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kder})
	fake := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: kder}) // a key labelled a certificate
	src := t.TempDir()
	certs := filepath.Join(src, "certs")
	write := func(p string, b []byte) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(certs, "ca.pem"), cert)
	write(filepath.Join(certs, "bundle.crt"), slices.Concat([]byte("# a comment\n"), cert, priv, fake, []byte("MIIB...raw\n"), cert))
	write(filepath.Join(certs, "server.key"), priv)
	write(filepath.Join(certs, "java", "cacerts"), kder)
	write(filepath.Join(certs, "sub", "ca.pem"), cert)
	write(filepath.Join(certs, "sub", "server.key"), slices.Concat(priv, fake))
	if err := os.Symlink("ca.pem", filepath.Join(certs, "1a2b3c4d.0")); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		mnt := filepath.Join(certs, "mnt")
		if err := os.Mkdir(mnt, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mount("tmpfs", mnt, "tmpfs", 0, "mode=0755,size=1m"); err != nil {
			t.Fatal(err)
		}
		defer syscall.Unmount(mnt, syscall.MNT_DETACH)
		write(filepath.Join(mnt, "server.key"), priv)
		write(filepath.Join(mnt, "server.pem"), slices.Concat(cert, priv))
	}
	r := t.TempDir()
	if err := stageCA(r, []string{certs, filepath.Join(src, "absent.pem")}); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	err = filepath.WalkDir(r, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(r+certs, p)
		if d.Type()&fs.ModeSymlink != 0 {
			l, err := os.Readlink(p)
			got[rel] = "-> " + l
			return err
		}
		b, err := os.ReadFile(p)
		got[rel] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"ca.pem": string(cert), "bundle.crt": string(cert) + string(cert),
		"sub/ca.pem": string(cert), "1a2b3c4d.0": "-> ca.pem"}
	if os.Geteuid() == 0 {
		want["mnt/server.pem"] = string(cert)
	}
	if !maps.Equal(got, want) {
		t.Fatalf("staged %v\nwant %v", got, want)
	}
}

// TestShortLivedJob: a function that exits at once still starts, and its result and code arrive: the Node checks its
// confinement and sets RLIMIT_CPU while the sandbox init waits to execute it, never once it may have ended.
func TestShortLivedJob(t *testing.T) {
	w, s := newRunWorker(t, config.WorkerCaps{}, "quick")
	for i := 1; i <= 10; i++ {
		j := testJob(i, "quick")
		run(t, w, j)
		ex := s.exit(t, j.ID)
		if ex.Reason != protocol.ExitExited || ex.Code == nil || *ex.Code != 0 || string(ex.Result) != `{"ok":true}` ||
			s.count(protocol.TypeJobStarted, j.ID) != 1 {
			t.Fatalf("%s: %+v %s", j.ID, ex, ex.Result)
		}
	}
}

// TestForkBomb: pids.max and RLIMIT_NPROC stop a job that starts processes without end; the Node is untouched and the
// job ends when its first process exits, taking the others with it.
func TestForkBomb(t *testing.T) {
	const pids = 32
	w, s := newRunWorker(t, config.WorkerCaps{MaxPids: pids}, "forkbomb")
	j := testJob(1, "forkbomb")
	begin := time.Now()
	run(t, w, j)
	ex := s.exit(t, j.ID)
	var r struct {
		Started int
		Err     string
	}
	if err := json.Unmarshal(ex.Result, &r); err != nil || ex.Reason != protocol.ExitExited {
		t.Fatalf("%+v %s: %v", ex, ex.Result, err)
	}
	if r.Err == "<nil>" || r.Started >= pids {
		t.Fatalf("started %d processes (max_pids %d), last error %s", r.Started, pids, r.Err)
	}
	if d := time.Since(begin); d > 30*time.Second {
		t.Fatalf("the job's processes outlived it: it ended after %s", d)
	}
}

// TestEgressUnenforceable: an egress policy the host cannot enforce (here ip, nsenter and nft are not found, or the
// Node is not root) fails the probe, so the class is not advertised, and a job asking for that network fails without
// running: never a silent none nor an open network. (A job naming no net runs under none and needs no egress.)
func TestEgressUnenforceable(t *testing.T) {
	dirs := egressToolDirs
	egressToolDirs = []string{t.TempDir()}
	t.Cleanup(func() { egressToolDirs = dirs })
	for _, e := range []string{config.EgressPublic, config.EgressOpenVibeOnly} {
		s := &sink{}
		cfg := helperConfig(t, config.WorkerCaps{}, "env")
		cfg.Egress = e
		if e == config.EgressOpenVibeOnly {
			cfg.EgressAllow = []string{"9.9.9.9/32"}
		}
		w := New(cfg, s.send, quiet())
		t.Cleanup(w.Close)
		if err := w.Probe(); err == nil || !strings.Contains(err.Error(), "worker.egress "+e) {
			t.Fatalf("%s: probe %v, want refused", e, err)
		}
		j := testJob(1, "env")
		j.Net = e
		run(t, w, j)
		if ex := s.exit(t, j.ID); ex.Reason != protocol.ExitFailed || ex.Usage.StartedMS != nil || s.count(protocol.TypeJobStarted, j.ID) != 0 {
			t.Fatalf("%s: %+v", e, ex)
		}
	}
}

// TestEgress: under none a job has no route at all; under public it has one, but 10.0.0.1, 169.254.169.254 (cloud
// metadata) and the host's end of its veth are refused by the Node's table (refused, not unreachable nor a timeout);
// under openvibe-only an unlisted public host is refused too; a job naming net "deny" or no net at all under public
// gets none whatever the host allows, and one naming public under openvibe-only gets public. It needs root, ip,
// nsenter, nft and IPv4 forwarding: it skips without the programs even where OPENVIBE_WORKER_TESTS=require.
func TestEgress(t *testing.T) {
	none := map[string]string{"10.0.0.1:80": "unreachable", "169.254.169.254:80": "unreachable",
		"169.254.240.1:22": "unreachable", "1.1.1.1:53": "unreachable"}
	refused := map[string]string{"10.0.0.1:80": "refused", "169.254.169.254:80": "refused", "169.254.240.1:22": "refused"}
	for _, c := range []struct {
		name, egress string
		allow        []string
		net          string
		want         map[string]string
	}{
		{"none", "", nil, "", none},
		{"public", config.EgressPublic, nil, protocol.NetPublic, refused},
		{"openvibe-only", config.EgressOpenVibeOnly, []string{"9.9.9.9/32"}, protocol.NetOpenVibeOnly,
			map[string]string{"10.0.0.1:80": "refused", "169.254.169.254:80": "refused", "1.1.1.1:53": "refused"}},
		{"deny under public", config.EgressPublic, nil, protocol.NetDeny, none},
		{"no net under public", config.EgressPublic, nil, "", none},
		{"none under openvibe-only", config.EgressOpenVibeOnly, []string{"9.9.9.9/32"}, "", none},
		// A job asking for less than the host allows runs under what it asked.
		{"public under openvibe-only", config.EgressOpenVibeOnly, []string{"9.9.9.9/32"}, protocol.NetPublic, refused},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := helperConfig(t, config.WorkerCaps{}, "egress")
			cfg.Egress, cfg.EgressAllow = c.egress, c.allow
			if c.egress != "" {
				if _, err := egressReady(); errors.Is(err, errEgressTool) {
					t.Skip(err)
				}
			}
			w, s := newRunWorkerConfig(t, cfg)
			j := testJob(1, "egress")
			j.Net = c.net
			run(t, w, j)
			ex := s.exit(t, j.ID)
			var got map[string]string
			if err := json.Unmarshal(ex.Result, &got); err != nil || ex.Reason != protocol.ExitExited {
				t.Fatalf("%+v %s: %v", ex, ex.Result, err)
			}
			for a, want := range c.want {
				if got[a] != want {
					t.Errorf("%s: %s, want %s (all: %v)", a, got[a], want, got)
				}
			}
		})
	}
}

// TestEgressRuleset: the table refuses IPv6, other routes than the default one, the private, shared, link-local and
// reserved ranges and egress_deny before anything is accepted; openvibe-only then accepts egress_allow alone.
func TestEgressRuleset(t *testing.T) {
	cfg := config.WorkerConfig{Egress: config.EgressPublic, EgressDeny: []string{"203.0.113.0/24", "198.51.100.0/24"}}
	pub := ruleset(cfg, []string{"eth0", "wlan0"})
	cfg.Egress, cfg.EgressAllow = config.EgressOpenVibeOnly, []string{"9.9.9.9/32"}
	ovo := ruleset(cfg, []string{"eth0"})
	for _, r := range []string{pub, ovo} {
		job := r[strings.Index(r, "chain job {"):]
		job = job[:strings.Index(job, "\n\t}")]
		accept := strings.Index(job, "accept")
		for _, want := range []string{"meta nfproto != ipv4 goto refuse", "ip daddr 10.0.0.0/8 goto refuse",
			"ip daddr 100.64.0.0/10 goto refuse", "ip daddr 169.254.0.0/16 goto refuse",
			"ip daddr 172.16.0.0/12 goto refuse", "ip daddr 192.168.0.0/16 goto refuse",
			"ip daddr 203.0.113.0/24 goto refuse", "ip daddr 198.51.100.0/24 goto refuse"} {
			if i := strings.Index(job, want); i < 0 || i > accept {
				t.Fatalf("%q missing or after the first accept in\n%s", want, job)
			}
		}
		if !strings.Contains(r, "iifname \"ovw*\" goto refuse") || !strings.Contains(r, "delete table inet openvibe_worker") {
			t.Fatalf("the host itself is not refused, or the table is not replaced whole:\n%s", r)
		}
	}
	if !strings.Contains(pub, `oifname != { "eth0", "wlan0" } goto refuse`) || strings.Contains(pub, "9.9.9.9") {
		t.Fatalf("public:\n%s", pub)
	}
	if !strings.Contains(ovo, "ip daddr 9.9.9.9/32 accept\n\t\tgoto refuse\n\t\taccept") {
		t.Fatalf("openvibe-only does not refuse what egress_allow does not list:\n%s", ovo)
	}
}

// TestRootPlan: the private root holds the device nodes; no bind is, holds or lies in /etc/ssl, /etc/pki (whose CA
// certificates are copied, not bound), the Node's directories nor /var; a system path holding a Node directory is
// refused.
func TestRootPlan(t *testing.T) {
	if _, _, err := rootPlan([]string{"/usr/local/openvibe-node"}); err == nil {
		t.Error("a Node directory in /usr was not refused")
	}
	binds, _, err := rootPlan([]string{"/var/lib/openvibe-node", "/etc/openvibe-node"})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range binds {
		if _, ok := overlaps(b.path, []string{"/etc/ssl", "/etc/pki", "/etc/ca-certificates", "/etc/shadow"}); ok ||
			under(b.path, []string{"/var", "/etc/openvibe-node", "/home", "/root", "/run"}) {
			t.Errorf("%s is bound", b.path)
		}
	}
	if !slices.ContainsFunc(binds, func(b hostBind) bool { return b.path == "/dev/null" && b.dev }) {
		t.Error("/dev/null is not bound")
	}
}

// TestArtifactRefused: an artifact directory that is, holds or lies in the host's kernel interfaces, runtime sockets,
// configuration or the Node's directories, or that is or holds a shared directory, is refused; one whose tree holds a
// Unix socket or a FIFO is refused at boot; a function with one keeps the probe from passing; and the directory a job
// opens is checked where its path leads at that moment.
func TestArtifactRefused(t *testing.T) {
	for _, a := range []string{"/", "/run", "/run/user", "/var/run", "/var", "/proc/1/root", "/sys", "/dev", "/etc",
		"/tmp", "/var/tmp", "/home"} {
		if err := checkArtifact(nil, a); err == nil {
			t.Errorf("artifact %s was not refused", a)
		}
	}
	for _, c := range []struct{ node, artifact string }{{"/srv/ov/state", "/srv/ov"}, {"/srv/ov", "/srv/ov/bin"},
		{"/srv/ov", "/srv/ov"}} {
		if err := checkArtifact([]string{c.node}, c.artifact); err == nil {
			t.Errorf("artifact %s with the Node in %s was not refused", c.artifact, c.node)
		}
	}
	dir := t.TempDir()
	if err := checkArtifact(nil, dir); err != nil {
		t.Fatalf("artifact %s: %v", dir, err)
	}
	if err := scanArtifact(dir); err != nil {
		t.Fatalf("artifact %s: %v", dir, err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", filepath.Join(dir, "sub", "s"))
	if err != nil {
		t.Fatal(err)
	}
	if err := scanArtifact(dir); err == nil || !strings.Contains(err.Error(), "socket") {
		t.Errorf("artifact holding a Unix socket: %v", err)
	}
	l.Close()
	if err := syscall.Mkfifo(filepath.Join(dir, "f"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := scanArtifact(dir); err == nil || !strings.Contains(err.Error(), "FIFO") {
		t.Errorf("artifact holding a FIFO: %v", err)
	}
	// A path swapped for a symlink to a host-only directory after boot: the directory opened is refused.
	link := filepath.Join(t.TempDir(), "fn")
	if err := os.Symlink("/run", link); err != nil {
		t.Fatal(err)
	}
	if f, err := openArtifact(nil, nil, nil, link); err == nil || !strings.Contains(err.Error(), "/run") {
		if f != nil {
			f.Close()
		}
		t.Errorf("artifact %s leading to /run: %v, want refused", link, err)
	}
	binds, links, err := rootPlan(nil)
	if err != nil {
		t.Fatal(err)
	}
	if f, err := openArtifact(nil, binds, links, "/usr/bin"); !usrTrusted(t) {
		if err == nil || !strings.Contains(err.Error(), "could change") {
			t.Errorf("artifact in a system path whose owner is not mapped here: %v, want refused", err)
		}
	} else if f == nil || err != nil {
		t.Errorf("artifact in a system path: %v %v, want it copied as any other", f, err)
	} else {
		f.Close()
	}
	cfg := helperConfig(t, config.WorkerCaps{}, "env")
	cfg.Functions[0].ArtifactDir = "/run"
	w := New(cfg, (&sink{}).send, quiet())
	t.Cleanup(w.Close)
	if err := w.Probe(); err == nil || !strings.Contains(err.Error(), "/run") {
		t.Fatalf("probe with artifact_dir /run: %v, want refused", err)
	}
}

// TestUsageSecondsContiguous: a function that holds for several whole wall-clock seconds after job_started sends
// exactly one job_usage for each fully elapsed second, `second` 0 then 1 then ... with no gap and no duplicate, and
// its job_exit's usage is never smaller than the seconds already sent (wall_ms >= (n+1)*1000 for every second n).
func TestUsageSecondsContiguous(t *testing.T) {
	// A job runs only sandboxed (there is no unconfined mode, even for a test): this runs where the sandbox can.
	cfg := helperConfig(t, config.WorkerCaps{MaxMemBytes: 8 << 30})
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Functions = []config.FunctionConfig{{Name: "meter", Version: "1.0.0",
		Command: []string{exe, "-test.run=^TestUsageHelperProcess$", "--", "meter"}, Env: map[string]string{"GOMAXPROCS": "2"}}}
	w, s := newRunWorkerConfig(t, cfg)

	const holdMS = 2500 // two whole seconds, so seconds 0 and 1 are sent before the exit
	j := testJob(1, "meter")
	j.Args = json.RawMessage(fmt.Sprintf(`{"hold_ms":%d}`, holdMS))
	if f, r := w.Admit(j); f != "" {
		t.Fatalf("%s %s", f, r)
	}
	w.Launch(j.ID)
	ex := s.exit(t, j.ID)
	if ex.Reason != protocol.ExitExited || ex.Code == nil || *ex.Code != 0 || ex.Usage.StartedMS == nil {
		t.Fatalf("%+v", ex)
	}
	if n := s.count(protocol.TypeJobStarted, j.ID); n != 1 {
		t.Fatalf("job_started sent %d times, want 1", n)
	}
	started := s.wait(t, protocol.TypeJobStarted, j.ID, 1, 5*time.Second).(protocol.JobStarted)
	if started.StartedMS != *ex.Usage.StartedMS {
		t.Fatalf("job_started at %d, job_exit usage at %d", started.StartedMS, *ex.Usage.StartedMS)
	}
	var seconds []int64
	for _, m := range s.all() {
		u, ok := m.(protocol.JobUsage)
		if !ok || u.ID != j.ID {
			continue
		}
		if u.StartedMS != *ex.Usage.StartedMS || u.CPUMS == nil {
			t.Fatalf("usage %+v", u)
		}
		seconds = append(seconds, u.Second)
	}
	if len(seconds) < 2 {
		t.Fatalf("usage seconds %v, want 0..n-1 for a job that held %d ms", seconds, holdMS)
	}
	for n, second := range seconds {
		if second != int64(n) {
			t.Fatalf("usage seconds %v, want 0..%d with no gap or duplicate", seconds, len(seconds)-1)
		}
		if ex.Usage.WallMS < (second+1)*1000 {
			t.Fatalf("usage second %d sent but wall_ms is %d, want at least %d", second, ex.Usage.WallMS, (second+1)*1000)
		}
	}
}

// TestSeccompFilter: the program fits the kernel's limit and every jump lands inside it.
func TestSeccompFilter(t *testing.T) {
	p, err := seccompFilter()
	if err != nil {
		t.Skip(err)
	}
	if len(p) > 4096 {
		t.Fatalf("%d instructions", len(p))
	}
	for i, ins := range p {
		if ins.Code&0x07 == 0x05 && (i+1+int(ins.Jt) >= len(p) || i+1+int(ins.Jf) >= len(p)) {
			t.Fatalf("instruction %d jumps out of the program: %+v", i, ins)
		}
	}
	if last := p[len(p)-1]; last.Code != 0x06 {
		t.Fatalf("the program does not end in a return: %+v", last)
	}
}

// usrTrusted reports whether /usr is root's (or this user's) as seen here. In a user namespace that does not map
// the host's root, as some test sandboxes run, /usr shows the overflow owner (65534) and checkRoute refuses a path
// through it: it cannot tell root from any other unmapped host user.
func usrTrusted(t *testing.T) bool {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Stat("/usr", &st); err != nil {
		t.Fatal(err)
	}
	return slices.Contains(trustedUIDs(), st.Uid)
}

// TestLimitSwap: memory.swap.max is set to 0 and read back; a missing control (no swap accounting) is refused while
// the host has swap, or when whether it has some is unknown, and never created.
func TestLimitSwap(t *testing.T) {
	dir, cg := t.TempDir(), t.TempDir()
	const header = "Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority\n"
	none, some := filepath.Join(dir, "none"), filepath.Join(dir, "some")
	if err := os.WriteFile(none, []byte(header), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(some, []byte(header+"/swap.img\tfile\t\t2097148\t\t0\t\t-2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := limitSwap(cg, some); err == nil || !strings.Contains(err.Error(), "while the host has swap") {
		t.Errorf("no memory.swap.max, host swap on: %v, want refused", err)
	}
	if err := limitSwap(cg, filepath.Join(dir, "missing")); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("no memory.swap.max, host swap unknown: %v, want refused", err)
	}
	if err := limitSwap(cg, none); err != nil {
		t.Errorf("no memory.swap.max, no host swap: %v", err)
	}
	ctl := filepath.Join(cg, "memory.swap.max")
	if _, err := os.Stat(ctl); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a missing memory.swap.max was created: %v", err)
	}
	if err := os.WriteFile(ctl, []byte("max\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := limitSwap(cg, some); err != nil {
		t.Errorf("memory.swap.max there: %v", err)
	}
	if b, _ := os.ReadFile(ctl); string(b) != "0" {
		t.Errorf("memory.swap.max = %q, want 0", b)
	}
	if os.Geteuid() != 0 {
		if err := os.Chmod(ctl, 0o444); err != nil {
			t.Fatal(err)
		}
		if err := limitSwap(cg, none); err == nil {
			t.Error("memory.swap.max not writable: no error")
		}
	}
}

// TestSpecNotInCmdline: the sandbox init gets its spec, the function's environment included, on specFD and never in
// its argv: while it waits for the Node's go, its /proc/<pid>/cmdline is its name alone. It needs only user, mount and
// PID namespaces, no cgroup: it runs where the sandboxed jobs cannot.
func TestSpecNotInCmdline(t *testing.T) {
	const envMarker = "marker-from-the-environment"
	sf, err := specFile(sandboxSpec{Root: t.TempDir(), Work: "/tmp/w", Disk: 1 << 20, Argv: []string{"/bin/true"},
		Env: []string{"OPENVIBE_MARKER=" + envMarker}})
	if err != nil {
		t.Fatal(err)
	}
	defer sf.Close()
	sr, sw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer sr.Close()
	gr, gw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()
	cmd := &exec.Cmd{Path: "/proc/self/exe", Args: []string{sandboxArg0}, Env: []string{}, Dir: "/", Stderr: os.Stderr,
		ExtraFiles: []*os.File{nil, sw, gr, sf}, // statusFD, goFD, specFD
		SysProcAttr: &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL,
			Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS | syscall.CLONE_NEWPID,
			UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Geteuid(), Size: 1}},
			GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getegid(), Size: 1}}}}
	err = cmd.Start()
	sw.Close()
	gr.Close()
	if err != nil {
		if os.Getenv("OPENVIBE_WORKER_TESTS") == "require" {
			t.Fatal(err)
		}
		t.Skipf("no user, mount and PID namespace here: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	_ = sr.SetReadDeadline(time.Now().Add(10 * time.Second))
	var b [1]byte
	if _, err := io.ReadFull(sr, b[:]); err != nil {
		t.Fatalf("the sandbox init did not report: %v", err)
	}
	if b[0] != 0 {
		msg, _ := io.ReadAll(sr)
		if strings.Contains(string(msg), "spec") || os.Getenv("OPENVIBE_WORKER_TESTS") == "require" {
			t.Fatalf("the sandbox init failed: %s%s", b[:], msg)
		}
		t.Skipf("the sandbox init cannot stand here: %s%s", b[:], msg)
	}
	c, err := os.ReadFile("/proc/" + strconv.Itoa(cmd.Process.Pid) + "/cmdline")
	if err != nil {
		t.Fatal(err)
	}
	if string(c) != sandboxArg0+"\x00" || strings.Contains(string(c), envMarker) {
		t.Errorf("the sandbox init's command line is %q, want its name alone", c)
	}
	gw.Close() // no go: the init exits instead of executing the function
	if msg, _ := io.ReadAll(sr); !strings.Contains(string(msg), "did not let the function start") {
		t.Errorf("the init after no go: %q", msg)
	}
}
