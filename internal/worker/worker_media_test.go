//go:build linux

package worker

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenVibers/OpenVibe.Node/internal/config"
	"github.com/OpenVibers/OpenVibe.Node/internal/protocol"
)

const (
	testMediaToken = "ov-media-test-token-3f1c9a"
	testMediaEnv   = "OPENVIBE_TEST_MEDIA_TOKEN"
	inputID        = "med_01JAB2C3D4E5F6G7H8J9K0MNPQ"
	otherID        = "med_01JAB2C3D4E5F6G7H8J9K0MNPR"
	resultID       = "med_01JAB2C3D4E5F6G7H8J9K0MNPS"
)

func digestOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// mediaFake is OpenVibe.Media's objects API on TLS: download answers a 302 to where (by default the object's bytes
// on a second host, as a signed storage URL is), and uploads are kept.
type mediaFake struct {
	srv, store *httptest.Server
	mu         sync.Mutex
	objects    map[string][]byte
	where      func(id string) string // the download redirect's target
	uploaded   []byte
	bounce     bool     // every API request is redirected to the storage host
	storeAuth  []string // Authorization headers the storage host saw
	apiAuth    []string // Authorization headers the API saw
}

func newMediaFake(t *testing.T) *mediaFake {
	m := &mediaFake{objects: map[string][]byte{}}
	m.store = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.storeAuth = append(m.storeAuth, r.Header.Get("Authorization"))
		b, ok := m.objects[strings.TrimPrefix(r.URL.Path, "/o/")]
		m.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(m.store.Close)
	m.where = func(id string) string { return m.store.URL + "/o/" + id + "?exp=1&sig=s3cr3t" }
	m.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.apiAuth = append(m.apiAuth, r.Header.Get("Authorization"))
		if m.bounce {
			http.Redirect(w, r, m.store.URL+"/o/x", http.StatusFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+testMediaToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/api/v2/run/objects")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/download") && r.URL.Query().Get("redirect") == "1":
			http.Redirect(w, r, m.where(strings.TrimSuffix(strings.TrimPrefix(p, "/"), "/download")), http.StatusFound)
		case r.Method == http.MethodPost && p == "":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"` + resultID + `","upload":{"method":"PUT"}}`))
		case r.Method == http.MethodPut && p == "/"+resultID+"/content":
			m.uploaded, _ = io.ReadAll(r.Body)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": resultID, "content_hash": digestOf(m.uploaded)})
		case r.Method == http.MethodPost && p == "/"+resultID+"/complete":
			_, _ = w.Write([]byte(`{"id":"` + resultID + `","lifecycle_status":"ready"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mediaFake) config(cfg *config.WorkerConfig) {
	cfg.Media = config.MediaConfig{Enabled: true, Endpoint: m.srv.URL, App: "run", TokenEnv: testMediaEnv, MaxBytes: 1 << 20}
}

// mediaWorker is a worker whose jobs fetch from m, with its log in log and isolate standing for the sandbox: it records
// the plan and the job's environment, and runs nothing.
func mediaWorker(t *testing.T, m *mediaFake) (w *Worker, s *sink, log *logBuf, isolated func() (plan, []string, bool)) {
	t.Helper()
	t.Setenv(testMediaEnv, testMediaToken)
	cfg := helperConfig(t, config.WorkerCaps{}, "sleep")
	m.config(&cfg)
	s, log = &sink{}, &logBuf{}
	w = New(cfg, s.send, slog.New(slog.NewTextHandler(log, nil)))
	w.mediaRT = m.srv.Client().Transport
	var mu sync.Mutex
	var got plan
	var env []string
	called := false
	w.isolate = func(cmd *exec.Cmd, _ config.WorkerConfig, p plan) (*sandbox, error) {
		mu.Lock()
		defer mu.Unlock()
		got, env, called = p, cmd.Env, true
		// what lies in the job's directory when its sandbox would be made, read now: execute removes it after
		for _, n := range p.inputs {
			b, err := os.ReadFile(filepath.Join(p.root, n))
			got.inputs = append(got.inputs, n+"="+string(b)+"|"+errString(err))
		}
		return nil, errors.New("not in this test")
	}
	t.Cleanup(w.Close)
	return w, s, log, func() (plan, []string, bool) {
		mu.Lock()
		defer mu.Unlock()
		return got, env, called
	}
}

func errString(err error) string {
	if err != nil {
		return err.Error()
	}
	return ""
}

func inputJob(n int, ins ...protocol.JobInput) protocol.Job {
	j := testJob(n, "sleep")
	j.Inputs = ins
	return j
}

// TestInputFetched: an input whose bytes match its pinned sha256 lies in the job's directory, under its name, when the
// sandbox is made; the token went to the API only (not to the storage host it redirected to) and is in neither the
// job's environment nor the log.
func TestInputFetched(t *testing.T) {
	m := newMediaFake(t)
	data := []byte("frame bytes\n")
	m.objects[inputID] = data
	w, s, log, isolated := mediaWorker(t, m)
	j := inputJob(1, protocol.JobInput{Name: "clip.mp4", MediaID: inputID, SHA256: digestOf(data), SizeBytes: int64(len(data))})
	run(t, w, j)
	s.exit(t, j.ID)
	p, env, called := isolated()
	if !called || len(p.inputs) != 2 || p.inputs[0] != "clip.mp4" || p.inputs[1] != "clip.mp4=frame bytes\n|" {
		t.Fatalf("called %v, inputs %q", called, p.inputs)
	}
	for _, e := range env {
		if strings.Contains(e, testMediaToken) || strings.HasPrefix(e, testMediaEnv+"=") {
			t.Fatalf("the token reached the job's environment: %q", e)
		}
	}
	if strings.Contains(log.String(), testMediaToken) || strings.Contains(log.String(), "s3cr3t") {
		t.Fatalf("the token or a signed URL was logged: %s", log)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.storeAuth) != 1 || m.storeAuth[0] != "" {
		t.Fatalf("the storage host saw Authorization %q", m.storeAuth)
	}
}

// TestInputRefused: a flipped byte, a body over max_bytes, a size other than declared, no token, a redirect to http
// or to another object's bytes each end the job failed with nothing run: no sandbox, no job_started, no job_stdout,
// and no token in the log.
func TestInputRefused(t *testing.T) {
	data := []byte("frame bytes\n")
	flipped := append([]byte(nil), data...)
	flipped[0] ^= 1
	good := protocol.JobInput{Name: "clip.mp4", MediaID: inputID, SHA256: digestOf(data)}
	for _, c := range []struct {
		name, why string // why: in the logged error
		set       func(t *testing.T, m *mediaFake, in *protocol.JobInput)
	}{
		{"flipped byte", "sha256 does not match", func(t *testing.T, m *mediaFake, in *protocol.JobInput) { m.objects[inputID] = flipped }},
		{"over max_bytes", "over the 1048576 bytes allowed", func(t *testing.T, m *mediaFake, in *protocol.JobInput) {
			big := bytes.Repeat([]byte("x"), 1<<20+1)
			m.objects[inputID], in.SHA256 = big, digestOf(big)
		}},
		{"size not as declared", "not the 13 declared", func(t *testing.T, m *mediaFake, in *protocol.JobInput) { in.SizeBytes = int64(len(data)) + 1 }},
		{"token_env unset", "OPENVIBE_TEST_MEDIA_TOKEN is not set", func(t *testing.T, m *mediaFake, in *protocol.JobInput) { t.Setenv(testMediaEnv, "") }},
		{"redirect to http", "a redirect left https", func(t *testing.T, m *mediaFake, in *protocol.JobInput) {
			m.where = func(id string) string { return strings.Replace(m.store.URL, "https:", "http:", 1) + "/o/" + id }
		}},
		{"redirect to another digest", "sha256 does not match", func(t *testing.T, m *mediaFake, in *protocol.JobInput) {
			m.objects[otherID] = []byte("other bytes\n")
			m.where = func(string) string { return m.store.URL + "/o/" + otherID }
		}},
		{"media unreachable", "connect", func(t *testing.T, m *mediaFake, in *protocol.JobInput) { m.srv.Close() }},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := newMediaFake(t)
			m.objects[inputID] = data
			w, s, log, isolated := mediaWorker(t, m)
			in := good
			c.set(t, m, &in)
			j := inputJob(1, in)
			run(t, w, j)
			ex := s.exit(t, j.ID)
			if ex.Reason != protocol.ExitFailed || ex.Code != nil || ex.Usage.StartedMS != nil || string(ex.Result) != "null" {
				t.Fatalf("%+v", ex)
			}
			if _, _, called := isolated(); called {
				t.Fatal("the job's sandbox was made")
			}
			if n, o := s.count(protocol.TypeJobStarted, j.ID), s.count(protocol.TypeJobStdout, j.ID); n != 0 || o != 0 {
				t.Fatalf("%d job_started, %d job_stdout", n, o)
			}
			if strings.Contains(log.String(), testMediaToken) || strings.Contains(log.String(), "s3cr3t") {
				t.Fatalf("the token or a signed URL was logged: %s", log)
			}
			if !strings.Contains(log.String(), "job not run") || !strings.Contains(log.String(), c.why) {
				t.Fatalf("the failure was not logged: %s", log)
			}
		})
	}
}

// TestAdmitInputs: with worker.media on a job naming inputs is admitted; malformed inputs are refused even when Check
// was skipped.
func TestAdmitInputs(t *testing.T) {
	m := newMediaFake(t)
	w, _, _, _ := mediaWorker(t, m)
	if f, r := w.Admit(inputJob(1, protocol.JobInput{Name: "a.bin", MediaID: inputID, SHA256: strings.Repeat("a", 64)})); f != "" {
		t.Fatalf("%s %s", f, r)
	}
	if _, r := w.Admit(inputJob(2, protocol.JobInput{Name: "../a.bin", MediaID: inputID, SHA256: strings.Repeat("a", 64)})); r != protocol.JobBadInputs {
		t.Fatalf("a path-escaping name: %q", r)
	}
}

// TestResultUploaded: a result over 256 KiB is uploaded as a Media object and job_exit carries {"media_id": …}; one
// within 256 KiB rides the link as it is; an upload Media redirects is refused.
func TestResultUploaded(t *testing.T) {
	m := newMediaFake(t)
	w, _, _, _ := mediaWorker(t, m)
	jb := &job{req: testJob(1, "sleep"), received: time.Now(), ttl: time.Minute}
	big := []byte(`{"frames":"` + strings.Repeat("x", resultBytes) + `"}`)
	got, err := w.storeResult(jb, big)
	if err != nil || string(got) != `{"media_id":"`+resultID+`"}` {
		t.Fatalf("%s %v", got, err)
	}
	if !bytes.Equal(m.uploaded, big) {
		t.Fatalf("uploaded %d bytes, want the %d of the result", len(m.uploaded), len(big))
	}
	if got, err := w.storeResult(jb, []byte(`{"ok":true}`)); err != nil || string(got) != `{"ok":true}` {
		t.Fatalf("%s %v", got, err)
	}
	if jb.abort != nil {
		t.Fatal("the upload's context outlived it")
	}
	m.mu.Lock()
	m.bounce = true
	m.mu.Unlock()
	if _, err := w.storeResult(jb, big); err == nil {
		t.Fatal("a redirected upload succeeded")
	}
	t.Setenv(testMediaEnv, "")
	if _, err := w.storeResult(jb, big); err == nil {
		t.Fatal("uploaded without a token")
	}
	// ended past its ttl: no request is made at all
	jb.received = time.Now().Add(-2 * time.Minute)
	t.Setenv(testMediaEnv, testMediaToken)
	if _, err := w.storeResult(jb, big); err == nil {
		t.Fatal("uploaded past the job's ttl")
	}
}

// TestRunInputsAndBigResult: in a real sandbox, a fetched input is in the job's working directory and a result over
// 256 KiB arrives as a media_id.
func TestRunInputsAndBigResult(t *testing.T) {
	m := newMediaFake(t)
	data := []byte("frame bytes\n")
	m.objects[inputID] = data
	t.Setenv(testMediaEnv, testMediaToken)
	cfg := helperConfig(t, config.WorkerCaps{MaxMemBytes: 8 << 30}, "inputs", "bigresult")
	m.config(&cfg)
	w, s := newRunWorkerConfig(t, cfg)
	w.mediaRT = m.srv.Client().Transport
	j := testJob(1, "inputs")
	j.Inputs = []protocol.JobInput{{Name: "clip.mp4", MediaID: inputID, SHA256: digestOf(data)}}
	run(t, w, j)
	ex := s.exit(t, j.ID)
	var got map[string]string
	if err := json.Unmarshal(ex.Result, &got); err != nil || ex.Reason != protocol.ExitExited || got["clip.mp4"] != string(data) {
		t.Fatalf("%+v %s: %v", ex, ex.Result, err)
	}
	if strings.Contains(got["env"], testMediaToken) {
		t.Fatal("the token reached the job's environment")
	}
	b := testJob(2, "bigresult")
	run(t, w, b)
	ex = s.exit(t, b.ID)
	if ex.Reason != protocol.ExitExited || string(ex.Result) != `{"media_id":"`+resultID+`"}` || len(m.uploaded) <= resultBytes {
		t.Fatalf("%+v %s (%d bytes uploaded)", ex, ex.Result, len(m.uploaded))
	}
}
