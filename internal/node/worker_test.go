//go:build linux

package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/OpenVibers/OpenVibe.Node/internal/config"
	"github.com/OpenVibers/OpenVibe.Node/internal/fakebot"
	"github.com/OpenVibers/OpenVibe.Node/internal/link"
	"github.com/OpenVibers/OpenVibe.Node/internal/localctl"
	"github.com/OpenVibers/OpenVibe.Node/internal/protocol"
	"github.com/OpenVibers/OpenVibe.Node/internal/worker"
)

// TestWorkerHelperProcess is the job's process when this test binary runs as the `hello` function (`-- job`): it
// tries to append a line to the marker file named in its args (the sandbox must prevent it), prints, holds for hold_ms (1.5 s by default) and writes its
// result to fd 3.
func TestWorkerHelperProcess(t *testing.T) {
	i := slices.Index(os.Args, "--")
	if i < 0 || i+1 >= len(os.Args) || os.Args[i+1] != "job" {
		return
	}
	var args struct {
		Marker string `json:"marker"`
		HoldMS int64  `json:"hold_ms"`
	}
	_ = json.NewDecoder(os.Stdin).Decode(&args)
	if args.HoldMS == 0 {
		args.HoldMS = 1500
	}
	if f, err := os.OpenFile(args.Marker, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		fmt.Fprintln(f, "started")
		f.Close()
	}
	fmt.Println("hello from the job")
	time.Sleep(time.Duration(args.HoldMS) * time.Millisecond)
	fmt.Fprint(os.NewFile(3, "result"), `{"ok":true}`)
	os.Exit(0)
}

// startWorkerNode pairs a Node with no plugin and no video whose worker runs the `hello` function, and waits for the
// status that advertises it. It is skipped where the kernel does not let this user create the job namespaces.
func startWorkerNode(t *testing.T) *env {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.MkdirTemp("", "ovn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	paths := config.HomePaths(home)
	srv := fakebot.New()
	t.Cleanup(srv.Close)
	srv.Config = protocol.Config{HeartbeatMS: 100, Limits: protocol.Limits{MaxCommandMS: 1000}}
	srv.AddCode("TEST2345")
	creds, err := link.Pair(context.Background(), nil, srv.URL(), protocol.PairRequest{Code: "TEST2345", AgentVersion: "test",
		DeviceKind: "onboard"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Plugins = nil
	cfg.Video = config.VideoConfig{Source: "off"}
	// A race-instrumented binary maps terabytes of shadow memory up front: RLIMIT_AS 64 TiB leaves it room. The job
	// gets a copy of its artifact directory, this test binary, in max_disk_bytes.
	cfg.Worker = config.WorkerConfig{Enabled: true, AllowSameUser: true, Caps: config.WorkerCaps{MaxMemBytes: 8 << 30, MaxVMBytes: 1 << 46,
		MaxDiskBytes: 256 << 20},
		Functions: []config.FunctionConfig{{Name: "hello", Version: "1.0.0", Command: []string{exe, "-test.run=^TestWorkerHelperProcess$", "--", "job"},
			Env: map[string]string{"GOMAXPROCS": "2"}}}}
	if os.Geteuid() == 0 { // as root, jobs run as nobody: the test binary must then lie in a directory nobody can read
		cfg.Worker.AllowSameUser, cfg.Worker.RunAs = false, &config.RunAs{UID: 65534, GID: 65534}
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	n, err := New(Options{Config: cfg, Paths: paths, Creds: creds, Version: "test", HeartbeatInterval: 100 * time.Millisecond,
		LinkBackoffMin: 50 * time.Millisecond, LatchPoll: 50 * time.Millisecond, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(n.jobs.runtimeClasses(), protocol.ClassFunction) {
		if os.Getenv("OPENVIBE_WORKER_TESTS") == "require" {
			t.Fatal("job isolation unavailable: the worker probe failed")
		}
		t.Skip("job isolation unavailable here (OPENVIBE_WORKER_TESTS=require fails instead)")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { n.Run(ctx); close(done) }()
	e := &env{t: t, srv: srv, node: n, paths: paths, cancel: cancel, done: done}
	t.Cleanup(e.stop)
	e.conn = e.nextConn()
	f, err := e.conn.Expect(protocol.TypeStatus, 10*time.Second, func(m protocol.Message) bool {
		_, ok := m.(protocol.Status).Capabilities[protocol.CapWorker]
		return ok
	})
	if err != nil {
		t.Fatal(err)
	}
	caps := cfg.Worker.Caps.WithDefaults()
	want := map[string]any{"runtime_classes": []any{protocol.ClassFunction}, "max_jobs": float64(caps.MaxJobs),
		"max_ttl_ms": float64(caps.MaxTTLMS), "max_wall_ms": float64(caps.MaxWallMS),
		"max_cpu_ms": float64(caps.MaxCPUMS), "max_mem_bytes": float64(caps.MaxMemBytes),
		"max_output_bytes": float64(caps.MaxOutputBytes)}
	got := f.Msg.(protocol.Status).Capabilities[protocol.CapWorker]
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("worker capability = %#v, want %#v", got, want)
	}
	return e
}

// TestJobRunsOverTheLink: a job sent by the fake Bot is acked, then job_started, job_stdout, job_usage (second 0)
// and job_exit arrive in that order; a job frame resent while it runs is acked again and starts no second process;
// after the end a resent job is answered with the same job_exit, before and after job_exit_ack.
func TestJobRunsOverTheLink(t *testing.T) {
	e := startWorkerNode(t)
	marker := filepath.Join(e.paths.StateDir, "starts")
	j := testJob(jobID(1))
	j.Artifact = &protocol.Artifact{Name: "hello", Version: "1.0.0"}
	j.Args = json.RawMessage(fmt.Sprintf(`{"marker":%q}`, marker))
	j.Limits.MemBytes = 4 << 30
	isJob := func(m protocol.Message) bool {
		switch f := m.(type) {
		case protocol.JobStarted:
			return f.ID == j.ID
		case protocol.JobExit:
			return f.ID == j.ID
		}
		return false
	}

	if r := e.jobReply(j); r != (protocol.Ack{ID: j.ID}) {
		t.Fatalf("%+v", r)
	}
	if _, err := e.conn.Expect(protocol.TypeJobStarted, 10*time.Second, isJob); err != nil {
		t.Fatal(err)
	}
	if r := e.jobReply(j); r != (protocol.Ack{ID: j.ID}) {
		t.Fatalf("resent while running: %+v", r)
	}
	f, err := e.conn.Expect(protocol.TypeJobExit, 15*time.Second, isJob)
	if err != nil {
		t.Fatal(err)
	}
	ex := f.Msg.(protocol.JobExit)
	if ex.Reason != protocol.ExitExited || ex.Code == nil || *ex.Code != 0 || string(ex.Result) != `{"ok":true}` || ex.Usage.StartedMS == nil {
		t.Fatalf("%+v %s", ex, ex.Result)
	}

	// Order on the wire: ack, job_started, job_stdout, job_usage, job_exit; one job_started only.
	var kinds []string
	var out strings.Builder
	started := 0
	for _, f := range e.conn.Frames() {
		switch m := f.Msg.(type) {
		case protocol.JobStarted:
			if m.ID == j.ID {
				started++
				kinds = append(kinds, f.Type)
			}
		case protocol.JobStdout:
			if m.ID == j.ID {
				out.WriteString(m.Chunk)
				kinds = append(kinds, f.Type)
			}
		case protocol.JobUsage:
			if m.ID == j.ID && m.Second == 0 && m.StartedMS == *ex.Usage.StartedMS {
				kinds = append(kinds, f.Type)
			}
		case protocol.JobExit:
			if m.ID == j.ID {
				kinds = append(kinds, f.Type)
			}
		}
	}
	kinds = slices.Compact(kinds)
	want := []string{protocol.TypeJobStarted, protocol.TypeJobStdout, protocol.TypeJobUsage, protocol.TypeJobExit}
	if started != 1 || !slices.Equal(kinds, want) || out.String() != "hello from the job\n" {
		t.Fatalf("%d job_started, frames %v, stdout %q", started, kinds, out.String())
	}

	// Ended: a resent job gets the same job_exit, before and after job_exit_ack, and never runs again.
	for _, ack := range []bool{false, true} {
		if ack {
			if err := e.conn.JobExitAck(j.ID); err != nil {
				t.Fatal(err)
			}
		}
		if err := e.conn.Job(j); err != nil {
			t.Fatal(err)
		}
		f, err := e.conn.Expect(protocol.TypeJobExit, 5*time.Second, isJob)
		if err != nil {
			t.Fatal(err)
		}
		if again := f.Msg.(protocol.JobExit); again.Reason != ex.Reason || *again.Usage.StartedMS != *ex.Usage.StartedMS || again.Usage.WallMS != ex.Usage.WallMS {
			t.Fatalf("resent job answered %+v, first job_exit %+v", again, ex)
		}
	}
	started = 0
	for _, f := range e.conn.Frames() {
		if m, ok := f.Msg.(protocol.JobStarted); ok && m.ID == j.ID {
			started++
		}
	}
	if started != 1 {
		t.Fatalf("the function started %d times, want once", started)
	}
	// The marker lies in the Node's state directory, which the job cannot see.
	if _, err := os.Stat(marker); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the job wrote into the Node's state directory: %v", err)
	}
}

// statusJobs is the jobs field of `openvibe-node status` (over the local control socket).
func (e *env) statusJobs() []worker.JobInfo {
	e.t.Helper()
	r, err := localctl.Call(e.paths.SocketPath(), localctl.Request{Cmd: localctl.CmdStatus}, 2*time.Second)
	if err != nil || !r.OK {
		e.t.Fatalf("status: %+v %v", r, err)
	}
	var s struct {
		Jobs []worker.JobInfo `json:"jobs"`
	}
	if err := json.Unmarshal(r.Data, &s); err != nil || s.Jobs == nil {
		e.t.Fatalf("status %s: %v", r.Data, err)
	}
	return s.Jobs
}

// TestLocalStopEndsJobs: `openvibe-node status` lists a running job; `openvibe-node stop` ends it stopped and new jobs
// are refused with local_stop; after `resume` jobs run again, and status lists none once they ended.
func TestLocalStopEndsJobs(t *testing.T) {
	e := startWorkerNode(t)
	marker := filepath.Join(e.paths.StateDir, "starts")
	hello := func(n, holdMS int) protocol.Job {
		j := testJob(jobID(n))
		j.Artifact = &protocol.Artifact{Name: "hello", Version: "1.0.0"}
		j.Args = json.RawMessage(fmt.Sprintf(`{"marker":%q,"hold_ms":%d}`, marker, holdMS))
		j.Limits.MemBytes = 4 << 30
		return j
	}
	exitOf := func(id string) protocol.JobExit {
		t.Helper()
		f, err := e.conn.Expect(protocol.TypeJobExit, 15*time.Second, func(m protocol.Message) bool {
			return m.(protocol.JobExit).ID == id
		})
		if err != nil {
			t.Fatal(err)
		}
		return f.Msg.(protocol.JobExit)
	}
	started := func(id string) {
		t.Helper()
		if _, err := e.conn.Expect(protocol.TypeJobStarted, 10*time.Second, func(m protocol.Message) bool {
			return m.(protocol.JobStarted).ID == id
		}); err != nil {
			t.Fatal(err)
		}
	}

	if jobs := e.statusJobs(); len(jobs) != 0 {
		t.Fatalf("jobs before any: %+v", jobs)
	}
	a := hello(1, 60000)
	if r := e.jobReply(a); r != (protocol.Ack{ID: a.ID}) {
		t.Fatalf("%+v", r)
	}
	started(a.ID)
	jobs := e.statusJobs()
	if len(jobs) != 1 || jobs[0].ID != a.ID || jobs[0].Function != "hello@1.0.0" || jobs[0].State != "running" || jobs[0].StartedMS == 0 {
		t.Fatalf("status jobs while running: %+v", jobs)
	}

	e.ctl(localctl.CmdStop)
	if ex := exitOf(a.ID); ex.Reason != protocol.ExitStopped || ex.Code != nil {
		t.Fatalf("%+v", ex)
	}
	if nk := e.jobNack(hello(2, 0), protocol.JobStopped); nk.FaultCode != protocol.FaultLocalStop {
		t.Fatalf("while stopped: %+v", nk)
	}
	if jobs := e.statusJobs(); len(jobs) != 0 {
		t.Fatalf("status jobs after the stop: %+v", jobs)
	}

	e.ctl(localctl.CmdResume)
	e.expectEstopState(false, false)
	b := hello(3, 0)
	if r := e.jobReply(b); r != (protocol.Ack{ID: b.ID}) {
		t.Fatalf("after resume: %+v", r)
	}
	started(b.ID)
	if ex := exitOf(b.ID); ex.Reason != protocol.ExitExited {
		t.Fatalf("%+v", ex)
	}
	if jobs := e.statusJobs(); len(jobs) != 0 {
		t.Fatalf("status jobs after the end: %+v", jobs)
	}
}

// TestResendAndCancelShareOneUsageSet: a job that runs for whole wall-clock seconds sends one job_usage per elapsed
// second (0 then 1, no gap and no duplicate); a resent job and a job_cancel after it ended both answer with the same
// job_exit, never a second job_started, a repeated usage second, or a second run of the function.
func TestResendAndCancelShareOneUsageSet(t *testing.T) {
	e := startWorkerNode(t)
	marker := filepath.Join(e.paths.StateDir, "starts")
	j := testJob(jobID(1))
	j.Artifact = &protocol.Artifact{Name: "hello", Version: "1.0.0"}
	j.Args = json.RawMessage(fmt.Sprintf(`{"marker":%q,"hold_ms":2500}`, marker))
	j.Limits.MemBytes = 4 << 30
	isExit := func(m protocol.Message) bool {
		ex, ok := m.(protocol.JobExit)
		return ok && ex.ID == j.ID
	}

	if r := e.jobReply(j); r != (protocol.Ack{ID: j.ID}) {
		t.Fatalf("%+v", r)
	}
	f, err := e.conn.Expect(protocol.TypeJobExit, 15*time.Second, isExit)
	if err != nil {
		t.Fatal(err)
	}
	ex := f.Msg.(protocol.JobExit)
	if ex.Reason != protocol.ExitExited || ex.Code == nil || *ex.Code != 0 || ex.Usage.StartedMS == nil {
		t.Fatalf("%+v", ex)
	}

	// usageSeconds is the `second` of every job_usage for this job, in arrival order, checking each names the same
	// started_ms as its job_exit and carries a cpu_ms.
	usageSeconds := func() []int64 {
		var seconds []int64
		for _, fr := range e.conn.Frames() {
			u, ok := fr.Msg.(protocol.JobUsage)
			if !ok || u.ID != j.ID {
				continue
			}
			if u.StartedMS != *ex.Usage.StartedMS || u.CPUMS == nil {
				t.Fatalf("usage %+v", u)
			}
			seconds = append(seconds, u.Second)
		}
		return seconds
	}
	jobStarts := func() int {
		n := 0
		for _, fr := range e.conn.Frames() {
			if s, ok := fr.Msg.(protocol.JobStarted); ok && s.ID == j.ID {
				n++
			}
		}
		return n
	}

	first := usageSeconds()
	if len(first) < 2 {
		t.Fatalf("usage seconds %v, want 0..n-1 for a job that held 2.5 s", first)
	}
	for n, second := range first {
		if second != int64(n) || ex.Usage.WallMS < (second+1)*1000 {
			t.Fatalf("usage seconds %v, wall_ms %d, want 0..%d contiguous and covering each second sent", first, ex.Usage.WallMS, len(first)-1)
		}
	}
	if n := jobStarts(); n != 1 {
		t.Fatalf("job_started sent %d times, want 1", n)
	}

	// A resent job and a job_cancel of the ended job each answer with the same job_exit (same result and usage).
	for _, resend := range []struct {
		name string
		send func() error
	}{
		{"resent job", func() error { return e.conn.Job(j) }},
		{"job_cancel", func() error { return e.conn.JobCancel(j.ID) }},
	} {
		if err := resend.send(); err != nil {
			t.Fatal(err)
		}
		f, err := e.conn.Expect(protocol.TypeJobExit, 5*time.Second, isExit)
		if err != nil {
			t.Fatalf("%s: %v", resend.name, err)
		}
		again := f.Msg.(protocol.JobExit)
		if again.Reason != ex.Reason || !reflect.DeepEqual(again.Code, ex.Code) || !reflect.DeepEqual(again.Usage, ex.Usage) || string(again.Result) != string(ex.Result) {
			t.Fatalf("%s answered %+v, first job_exit %+v", resend.name, again, ex)
		}
	}

	// No second process, job_started or usage second, and one usage set only.
	if n := jobStarts(); n != 1 {
		t.Fatalf("job_started sent %d times after the resends, want 1", n)
	}
	if again := usageSeconds(); !slices.Equal(again, first) {
		t.Fatalf("usage seconds %v after the resends, first %v", again, first)
	}
	b, err := os.ReadFile(marker)
	if err != nil || string(b) != "started\n" {
		t.Fatalf("the function ran %q (%v), want once", b, err)
	}
}
