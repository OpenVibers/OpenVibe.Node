//go:build linux

package node

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/OpenVibers/OpenVibe.Node/internal/config"
	"github.com/OpenVibers/OpenVibe.Node/internal/fakebot"
	"github.com/OpenVibers/OpenVibe.Node/internal/link"
	"github.com/OpenVibers/OpenVibe.Node/internal/protocol"
)

// TestWorkerHelperProcess is the job's process when this test binary runs as the `hello` function (`-- job`): it
// appends a line to the marker file named in its args, prints, holds for 1.5 s and writes its result to fd 3.
func TestWorkerHelperProcess(t *testing.T) {
	i := slices.Index(os.Args, "--")
	if i < 0 || i+1 >= len(os.Args) || os.Args[i+1] != "job" {
		return
	}
	var args struct {
		Marker string `json:"marker"`
	}
	_ = json.NewDecoder(os.Stdin).Decode(&args)
	if f, err := os.OpenFile(args.Marker, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		fmt.Fprintln(f, "started")
		f.Close()
	}
	fmt.Println("hello from the job")
	time.Sleep(1500 * time.Millisecond)
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
	cfg.Worker = config.WorkerConfig{Enabled: true, AllowSameUser: true, Caps: config.WorkerCaps{MaxMemBytes: 8 << 30},
		Functions: []config.FunctionConfig{{Name: "hello", Version: "1.0.0", Command: []string{exe, "-test.run=^TestWorkerHelperProcess$", "--", "job"}}}}
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
	_, err = e.conn.Expect(protocol.TypeStatus, 10*time.Second, func(m protocol.Message) bool {
		_, ok := m.(protocol.Status).Capabilities[protocol.CapWorker]
		return ok
	})
	if err != nil {
		t.Fatal(err)
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
	b, err := os.ReadFile(marker)
	if err != nil || string(b) != "started\n" {
		t.Fatalf("the function ran %q (%v), want once", b, err)
	}
}
