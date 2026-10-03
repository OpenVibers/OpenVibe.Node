package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/OpenVibers/OpenVibe.Node/internal/config"
	"github.com/OpenVibers/OpenVibe.Node/internal/protocol"
)

// sink records every frame the worker sends.
type sink struct {
	mu     sync.Mutex
	frames []protocol.Message
}

func (s *sink) send(m protocol.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames = append(s.frames, m)
}

func (s *sink) all() []protocol.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]protocol.Message(nil), s.frames...)
}

func frameID(m protocol.Message) string {
	switch f := m.(type) {
	case protocol.JobStarted:
		return f.ID
	case protocol.JobStdout:
		return f.ID
	case protocol.JobUsage:
		return f.ID
	case protocol.JobExit:
		return f.ID
	}
	return ""
}

// count is how many frames of type typ for job id were sent.
func (s *sink) count(typ, id string) int {
	n := 0
	for _, m := range s.all() {
		if m.MessageType() == typ && frameID(m) == id {
			n++
		}
	}
	return n
}

// wait returns the n-th (from 1) frame of type typ for job id, waiting up to timeout for it.
func (s *sink) wait(t *testing.T, typ, id string, n int, timeout time.Duration) protocol.Message {
	t.Helper()
	end := time.Now().Add(timeout)
	for {
		seen := 0
		for _, m := range s.all() {
			if m.MessageType() == typ && frameID(m) == id {
				if seen++; seen == n {
					return m
				}
			}
		}
		if time.Now().After(end) {
			t.Fatalf("no %s #%d for %s within %s", typ, n, id, timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (s *sink) exit(t *testing.T, id string) protocol.JobExit {
	t.Helper()
	return s.wait(t, protocol.TypeJobExit, id, 1, 20*time.Second).(protocol.JobExit)
}

func testJob(n int, name string) protocol.Job {
	return protocol.Job{ID: fmt.Sprintf("job_01JAB2C3D4E5F6G7H8J9K%05d", n), Class: protocol.ClassFunction,
		Artifact: &protocol.Artifact{Name: name, Version: "1.0.0"}, Args: json.RawMessage(`{"n":7}`), TTLMS: 60000,
		Limits: protocol.JobLimits{WallMS: 60000, CPUMS: 60000, MemBytes: 4 << 30}}
}

// helperConfig declares one function per helper mode, each running this test binary's TestHelperProcess.
func helperConfig(t *testing.T, caps config.WorkerCaps, modes ...string) config.WorkerConfig {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.WorkerConfig{Enabled: true, AllowSameUser: true, Caps: caps}
	for _, m := range modes {
		cfg.Functions = append(cfg.Functions, config.FunctionConfig{Name: m, Version: "1.0.0",
			Command: []string{exe, "-test.run=^TestHelperProcess$", "--", m}})
	}
	return cfg
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestAdmitRefusals: only a declared function at its exact version is admitted, never another class; max_jobs bounds
// the jobs held at once; while the stop latch is set every job is refused with its fault; nothing ever starts.
func TestAdmitRefusals(t *testing.T) {
	s := &sink{}
	w := New(helperConfig(t, config.WorkerCaps{MaxJobs: 1}, "sleep"), s.send, quiet())
	w.isolate = func(*exec.Cmd, config.WorkerConfig) error { return errors.New("not in this test") }

	wrongVersion := testJob(2, "sleep")
	wrongVersion.Artifact.Version = "1.0.1"
	code := testJob(3, "sleep")
	code.Class = protocol.ClassCode
	for _, c := range []struct {
		j             protocol.Job
		fault, reason string
	}{
		{testJob(1, "nope"), protocol.FaultUnsupported, protocol.JobUnknownArtifact},
		{wrongVersion, protocol.FaultUnsupported, protocol.JobUnknownArtifact},
		{code, protocol.FaultUnsupported, protocol.JobNotAvailable},
	} {
		if f, r := w.Admit(c.j); f != c.fault || r != c.reason {
			t.Errorf("%s %+v: %q %q, want %q %q", c.j.Class, c.j.Artifact, f, r, c.fault, c.reason)
		}
	}

	held := testJob(4, "sleep")
	if f, r := w.Admit(held); f != "" {
		t.Fatalf("%s %s", f, r)
	}
	if f, r := w.Admit(held); f != "" {
		t.Fatalf("a held id re-admitted: %s %s", f, r)
	}
	if f, r := w.Admit(testJob(5, "sleep")); f != protocol.FaultNotReady || r != protocol.JobBusy {
		t.Fatalf("over max_jobs: %q %q", f, r)
	}
	w.Cancel(held.ID)
	w.Launch(held.ID)
	if ex := s.exit(t, held.ID); ex.Reason != protocol.ExitCancelled {
		t.Fatalf("%+v", ex)
	}

	w.SetStopped(protocol.FaultLocalStop)
	if f, r := w.Admit(testJob(6, "sleep")); f != protocol.FaultLocalStop || r != protocol.JobStopped {
		t.Fatalf("latched: %q %q", f, r)
	}
	w.SetStopped("")
	if f, r := w.Admit(testJob(6, "sleep")); f != "" {
		t.Fatalf("after the latch cleared: %q %q", f, r)
	}
	for _, m := range s.all() {
		if _, ok := m.(protocol.JobStarted); ok {
			t.Fatalf("a refused job started: %+v", m)
		}
	}
}

// TestIsolationUnavailableRefused: when the namespaces cannot be created the probe fails (the class is not
// advertised), and a job whose isolation fails ends `failed` without its process ever starting.
func TestIsolationUnavailableRefused(t *testing.T) {
	s := &sink{}
	w := New(helperConfig(t, config.WorkerCaps{}, "sleep"), s.send, quiet())
	w.isolate = func(*exec.Cmd, config.WorkerConfig) error { return errors.New("no user namespaces") }
	t.Cleanup(w.Close)
	if err := w.Probe(); err == nil {
		t.Fatal("the probe passed without namespaces")
	}
	j := testJob(1, "sleep")
	if f, r := w.Admit(j); f != "" {
		t.Fatalf("%s %s", f, r)
	}
	w.Launch(j.ID)
	ex := s.exit(t, j.ID)
	if ex.Reason != protocol.ExitFailed || ex.Code != nil || ex.Usage.WallMS != 0 || ex.Usage.StartedMS != nil || string(ex.Result) != "null" {
		t.Fatalf("%+v", ex)
	}
	if n := s.count(protocol.TypeJobStarted, j.ID); n != 0 {
		t.Fatalf("job_started sent %d times for a job that could not be isolated", n)
	}
}

// TestCloseEndsUnlaunched: a job admitted but not launched when the worker closes gets its job_exit (stopped, never
// run), is answered with it when resent, and a later Launch starts nothing.
func TestCloseEndsUnlaunched(t *testing.T) {
	s := &sink{}
	w := New(helperConfig(t, config.WorkerCaps{}, "sleep"), s.send, quiet())
	w.isolate = func(*exec.Cmd, config.WorkerConfig) error { return errors.New("not in this test") }
	j := testJob(1, "sleep")
	if f, r := w.Admit(j); f != "" {
		t.Fatalf("%s %s", f, r)
	}
	if jobs := w.Jobs(); len(jobs) != 1 || jobs[0] != (JobInfo{ID: j.ID, Function: "sleep@1.0.0", State: "admitted"}) {
		t.Fatalf("jobs %+v", jobs)
	}
	w.Close()
	ex := s.exit(t, j.ID)
	if ex.Reason != protocol.ExitStopped || ex.Code != nil || ex.Usage.StartedMS != nil || string(ex.Result) != "null" {
		t.Fatalf("%+v", ex)
	}
	if m, ok := w.Known(j.ID); !ok || m.(protocol.JobExit).Reason != protocol.ExitStopped {
		t.Fatalf("a resent job is answered %+v, want its job_exit", m)
	}
	if jobs := w.Jobs(); len(jobs) != 0 {
		t.Fatalf("an ended job listed: %+v", jobs)
	}
	w.Launch(j.ID)
	time.Sleep(100 * time.Millisecond)
	if n, e := s.count(protocol.TypeJobStarted, j.ID), s.count(protocol.TypeJobExit, j.ID); n != 0 || e != 1 {
		t.Fatalf("%d job_started and %d job_exit after Close", n, e)
	}
	if f, _ := w.Admit(testJob(2, "sleep")); f != protocol.FaultShuttingDown {
		t.Fatalf("admitted after Close: %q", f)
	}
}

// TestUsageHelperProcess is the job's process for TestUsageSecondsContiguous when this test binary runs as the `meter`
// function (`-- meter`): it holds for hold_ms from its args and exits 0 without writing a result.
func TestUsageHelperProcess(t *testing.T) {
	i := slices.Index(os.Args, "--")
	if i < 0 || i+1 >= len(os.Args) || os.Args[i+1] != "meter" {
		return
	}
	var args struct {
		HoldMS int64 `json:"hold_ms"`
	}
	_ = json.NewDecoder(os.Stdin).Decode(&args)
	time.Sleep(time.Duration(args.HoldMS) * time.Millisecond)
	os.Exit(0)
}

// TestUsageSecondsContiguous: a function that holds for several whole wall-clock seconds after job_started sends
// exactly one job_usage for each fully elapsed second, `second` 0 then 1 then ... with no gap and no duplicate, and
// its job_exit's usage is never smaller than the seconds already sent (wall_ms >= (n+1)*1000 for every second n).
func TestUsageSecondsContiguous(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("function jobs run on Linux only")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Isolation is not what this test measures: run the helper directly, without the job namespaces.
	s := &sink{}
	w := New(config.WorkerConfig{Enabled: true, AllowSameUser: true,
		Functions: []config.FunctionConfig{{Name: "meter", Version: "1.0.0",
			Command: []string{exe, "-test.run=^TestUsageHelperProcess$", "--", "meter"}}}}, s.send, quiet())
	w.isolate = func(*exec.Cmd, config.WorkerConfig) error { return nil }
	t.Cleanup(w.Close)

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
