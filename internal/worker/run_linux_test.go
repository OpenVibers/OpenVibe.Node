//go:build linux

package worker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
	s := &sink{}
	w := New(helperConfig(t, caps, modes...), s.send, quiet())
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
	if r.PID != 1 || r.UID != os.Getuid() {
		t.Fatalf("pid %d uid %d, want 1 and %d", r.PID, r.UID, os.Getuid())
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
