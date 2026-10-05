package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
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
	// A race-instrumented binary maps terabytes of shadow memory up front: RLIMIT_AS 64 TiB leaves it room and is
	// still a limit. GOMAXPROCS keeps its threads well under max_pids on a machine with many cores.
	if caps.MaxVMBytes == 0 {
		caps.MaxVMBytes = 1 << 46
	}
	if caps.MaxDiskBytes == 0 { // each job gets a copy of its artifact directory, this test binary, in that much
		caps.MaxDiskBytes = 256 << 20
	}
	cfg := config.WorkerConfig{Enabled: true, AllowSameUser: true, Caps: caps}
	if os.Geteuid() == 0 { // as root, jobs run as nobody: the test binary must then lie in a directory nobody can read
		cfg.AllowSameUser, cfg.RunAs = false, &config.RunAs{UID: 65534, GID: 65534}
	}
	for _, m := range modes {
		cfg.Functions = append(cfg.Functions, config.FunctionConfig{Name: m, Version: "1.0.0",
			Command: []string{exe, "-test.run=^TestHelperProcess$", "--", m}, Env: map[string]string{"GOMAXPROCS": "2"}})
	}
	return cfg
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestAdmitRefusals: only a declared function at its exact version is admitted, never another class; max_jobs bounds
// the jobs held at once; while the stop latch is set every job is refused with its fault; nothing ever starts.
func TestAdmitRefusals(t *testing.T) {
	s := &sink{}
	w := New(helperConfig(t, config.WorkerCaps{MaxJobs: 1}, "sleep"), s.send, quiet())
	w.isolate = func(*exec.Cmd, config.WorkerConfig, plan) (*sandbox, error) {
		return nil, errors.New("not in this test")
	}

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
		{code, protocol.FaultUnsupported, protocol.JobUnknownArtifact},
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

// TestAdmitByClass: a job runs only an entry of its own class (a declared function is no code artifact, nor the
// reverse), a class no entry implements is never available, and Classes lists the declared classes in protocol order.
func TestAdmitByClass(t *testing.T) {
	if c := New(helperConfig(t, config.WorkerCaps{}), (&sink{}).send, quiet()).Classes(); len(c) != 0 {
		t.Fatalf("no entry declared, classes %v", c)
	}
	cfg := helperConfig(t, config.WorkerCaps{MaxJobs: 8}, "sleep")
	script := cfg.Functions[0]
	script.Name, script.Class = "script", config.ClassCode
	cfg.Functions = append([]config.FunctionConfig{script}, cfg.Functions...)
	w := New(cfg, (&sink{}).send, quiet())
	if c := w.Classes(); len(c) != 2 || c[0] != protocol.ClassFunction || c[1] != protocol.ClassCode {
		t.Fatalf("classes %v", c)
	}
	job := func(n int, class, name string) protocol.Job {
		j := testJob(n, name)
		j.Class = class
		return j
	}
	for _, c := range []struct {
		j             protocol.Job
		fault, reason string
	}{
		{job(1, protocol.ClassCode, "script"), "", ""},
		{job(2, protocol.ClassFunction, "sleep"), "", ""},
		{job(3, protocol.ClassFunction, "script"), protocol.FaultUnsupported, protocol.JobUnknownArtifact},
		{job(4, protocol.ClassCode, "sleep"), protocol.FaultUnsupported, protocol.JobUnknownArtifact},
		{job(5, protocol.ClassBrowser, "script"), protocol.FaultUnsupported, protocol.JobNotAvailable},
	} {
		if f, r := w.Admit(c.j); f != c.fault || r != c.reason {
			t.Errorf("%s %+v: %q %q, want %q %q", c.j.Class, c.j.Artifact, f, r, c.fault, c.reason)
		}
	}
	w.Close()
}

// TestAdmitLinux: a declared linux entry is a declared-command class like code — its job is admitted and matched by
// name, version and class — while browser, desktop and gpu jobs stay refused class not available.
func TestAdmitLinux(t *testing.T) {
	cfg := helperConfig(t, config.WorkerCaps{MaxJobs: 8}, "sleep")
	tool := cfg.Functions[0]
	tool.Name, tool.Class = "tool", config.ClassLinux
	cfg.Functions = append([]config.FunctionConfig{tool}, cfg.Functions...)
	w := New(cfg, (&sink{}).send, quiet())
	if c := w.Classes(); !slices.Equal(c, []string{protocol.ClassFunction, protocol.ClassLinux}) {
		t.Fatalf("classes %v", c)
	}
	job := func(n int, class, name string) protocol.Job {
		j := testJob(n, name)
		j.Class = class
		return j
	}
	if f, r := w.Admit(job(1, protocol.ClassLinux, "tool")); f != "" {
		t.Fatalf("a declared linux entry: %s %s", f, r)
	}
	if f, r := w.Admit(job(2, protocol.ClassLinux, "sleep")); f != protocol.FaultUnsupported || r != protocol.JobUnknownArtifact {
		t.Fatalf("a linux job naming a function entry: %q %q", f, r)
	}
	for i, class := range []string{protocol.ClassBrowser, protocol.ClassDesktop, protocol.ClassGPU} {
		if f, r := w.Admit(job(3+i, class, "tool")); f != protocol.FaultUnsupported || r != protocol.JobNotAvailable {
			t.Errorf("%s: %q %q, want %q %q", class, f, r, protocol.FaultUnsupported, protocol.JobNotAvailable)
		}
	}
	w.Close()
}

// TestIsolationUnavailableRefused: when the namespaces cannot be created the probe fails (the class is not
// advertised), and a job whose isolation fails ends `failed` without its process ever starting.
func TestIsolationUnavailableRefused(t *testing.T) {
	s := &sink{}
	w := New(helperConfig(t, config.WorkerCaps{}, "sleep"), s.send, quiet())
	w.isolate = func(*exec.Cmd, config.WorkerConfig, plan) (*sandbox, error) {
		return nil, errors.New("no user namespaces")
	}
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
	w.isolate = func(*exec.Cmd, config.WorkerConfig, plan) (*sandbox, error) {
		return nil, errors.New("not in this test")
	}
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

// TestEgressFor: an absent net (the contract's declared deny) is no network whatever the host allows; net deny and
// none are that too; a named public or openvibe-only runs under the host's policy when that is at least as strict and
// still a network, and is refused when the host is none (or unset) or wider than asked; a net this Node does not know
// is refused, never run weaker.
func TestEgressFor(t *testing.T) {
	for _, c := range []struct {
		host, net string
		want      string
		ok        bool
	}{
		{"", "", "", true}, {config.EgressNone, "", "", true}, {config.EgressPublic, "", "", true},
		{config.EgressOpenVibeOnly, "", "", true},
		{config.EgressPublic, protocol.NetDeny, "", true}, {config.EgressOpenVibeOnly, protocol.NetNone, "", true},
		{config.EgressPublic, protocol.NetPublic, config.EgressPublic, true},
		{config.EgressPublic, protocol.NetOpenVibeOnly, "", false},
		{config.EgressOpenVibeOnly, protocol.NetPublic, config.EgressOpenVibeOnly, true},
		{config.EgressOpenVibeOnly, protocol.NetOpenVibeOnly, config.EgressOpenVibeOnly, true},
		{config.EgressNone, protocol.NetPublic, "", false},
		{config.EgressNone, protocol.NetOpenVibeOnly, "", false},
		{"", protocol.NetPublic, "", false}, {"", protocol.NetOpenVibeOnly, "", false},
		{config.EgressPublic, "allow", "", false}, {config.EgressPublic, "", "", true},
	} {
		got, ok := egressFor(c.host, c.net)
		if got != c.want || ok != c.ok {
			t.Errorf("egressFor(%q, %q) = %q, %v; want %q, %v", c.host, c.net, got, ok, c.want, c.ok)
		}
	}
}

// TestAdmitRefusesInputs: a job carrying inputs is refused, never run with them silently dropped (platform.job@1).
func TestAdmitRefusesInputs(t *testing.T) {
	s := &sink{}
	w := New(helperConfig(t, config.WorkerCaps{}, "sleep"), s.send, quiet())
	j := testJob(1, "sleep")
	j.Inputs = []protocol.JobInput{{Name: "clip.mp4", MediaID: "med_01JAB2C3D4E5F6G7H8J9K0MNPQ",
		SHA256: "9f86d081884c7d659a2feb15b0b4f8f1c0e6ad1d7e6fd2e1b0b6fd1c4f1d2a3b"}}
	if f, r := w.Admit(j); f != protocol.FaultUnsupported || r != protocol.JobInputsRefused {
		t.Fatalf("a job with inputs: %q %q, want %q %q", f, r, protocol.FaultUnsupported, protocol.JobInputsRefused)
	}
	// A net the host cannot enforce is refused too, rather than run with no network.
	n := testJob(2, "sleep")
	n.Net = protocol.NetPublic
	cfg := helperConfig(t, config.WorkerCaps{}, "sleep")
	cfg.Egress = config.EgressNone
	if f, r := New(cfg, s.send, quiet()).Admit(n); f != protocol.FaultUnsupported || r != protocol.JobNetUnsupported {
		t.Fatalf("net public under egress none: %q %q", f, r)
	}
}
