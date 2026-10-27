//go:build linux

package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/OpenVibers/OpenVibe.Node/internal/config"
)

// deskConfig is a worker config a desk can run under here, or the test skips (fails under OPENVIBE_WORKER_TESTS=require,
// as the jobs' tests do).
func deskConfig(t *testing.T) config.WorkerConfig {
	t.Helper()
	cfg := helperConfig(t, config.WorkerCaps{})
	newRunWorkerConfig(t, cfg) // probes, and skips or fails as the jobs' tests do
	return cfg
}

type deskPipes struct {
	in, out       *os.File // the desk's ends: its fd 3 and fd 4
	write, answer *os.File // ours
}

func newDeskPipes(t *testing.T) deskPipes {
	t.Helper()
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, f := range []*os.File{inR, inW, outR, outW} {
			f.Close()
		}
	})
	return deskPipes{in: inR, out: outW, write: inW, answer: outR}
}

// TestDeskBindsOnlyInstallDirsAndFonts: a desk may bind an install directory or the fonts' configuration, read-only,
// never a home, a secret, the host's runtime or a relative path.
func TestDeskBindsOnlyInstallDirsAndFonts(t *testing.T) {
	for _, bad := range []string{"/home", "/root", "/etc/shadow", "/var/lib", "/run", "/etc", "opt/x", "/opt/../home"} {
		if _, err := CheckDeskBinds(nil, []string{bad}); err == nil {
			t.Errorf("%s was allowed", bad)
		}
	}
	dir := t.TempDir()
	if _, err := CheckDeskBinds([]string{"/opt"}, []string{"/opt"}); err == nil {
		t.Error("a bind holding a Node directory was allowed")
	}
	if _, err := os.Stat("/etc/fonts"); err == nil {
		if got, err := CheckDeskBinds([]string{dir}, []string{"/etc/fonts/"}); err != nil || len(got) != 1 || got[0] != "/etc/fonts" {
			t.Errorf("/etc/fonts: %v %v", got, err)
		}
	}
}

// TestDeskPipesBothWaysConfined: the desk's program reads on fd 3 and answers on fd 4 (as a browser's
// --remote-debugging-pipe does), runs as worker.run_as (nobody, as root), sees no /home, and gets its exit code back.
func TestDeskPipesBothWaysConfined(t *testing.T) {
	cfg := deskConfig(t)
	p := newDeskPipes(t)
	script := `read -r line <&3; printf 'uid=%s\n' "$(id -u)" >&4; printf 'got=%s\n' "$line" >&4; ` +
		`if [ -e /home ]; then echo home=yes >&4; else echo home=no >&4; fi; ` +
		`if [ -w /tmp/desk ]; then echo work=writable >&4; fi; exit 7`
	done := make(chan struct{})
	var code int
	var runErr error
	go func() {
		defer close(done)
		code, runErr = RunDesk(context.Background(), cfg, DeskSpec{Argv: []string{"/bin/sh", "-c", script}, Env: []string{"PATH=/usr/bin:/bin"},
			MemBytes: 64 << 20, CPUMillis: 500, DiskBytes: 16 << 20, Pids: 32, In: p.in, Out: p.out})
	}()
	if _, err := p.write.WriteString("hello desk\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the desk did not end")
	}
	if runErr != nil {
		t.Fatal(runErr)
	}
	p.out.Close() // ours: the desk's copy closed when it ended
	b, _ := io.ReadAll(p.answer)
	got := string(b)
	want := []string{"got=hello desk", "home=no", "work=writable"}
	if os.Geteuid() == 0 {
		want = append(want, "uid=65534")
	}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in %q", w, got)
		}
	}
	if code != 7 {
		t.Fatalf("exit code %d, want 7", code)
	}
}

// TestDeskEndsWithItsContext: cancelling a running desk kills it, and RunDesk returns.
func TestDeskEndsWithItsContext(t *testing.T) {
	cfg := deskConfig(t)
	p := newDeskPipes(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := RunDesk(ctx, cfg, DeskSpec{Argv: []string{"/bin/sh", "-c", "echo up >&4; exec sleep 600"}, Env: []string{"PATH=/usr/bin:/bin"},
			MemBytes: 64 << 20, CPUMillis: 500, DiskBytes: 16 << 20, Pids: 32, In: p.in, Out: p.out})
		done <- err
	}()
	line, err := bufio.NewReader(p.answer).ReadString('\n')
	if err != nil || line != "up\n" {
		t.Fatalf("the desk did not start: %q %v", line, err)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("no error for a cancelled desk")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("a cancelled desk kept running")
	}
}

// TestDeskRunsARealBrowser: headless Chrome in the desk sandbox answers DevTools over its pipe (Browser.getVersion,
// a page made and read), with no network: a desk host's browser works confined. Skipped where no Chrome is installed.
func TestDeskRunsARealBrowser(t *testing.T) {
	bin, dir := "", ""
	for _, c := range []struct{ bin, dir string }{{"/opt/google/chrome/chrome", "/opt/google/chrome"}, {"/opt/chromium/chrome", "/opt/chromium"}} {
		if _, err := os.Stat(c.bin); err == nil {
			bin, dir = c.bin, c.dir
			break
		}
	}
	if bin == "" {
		t.Skip("no Chrome in /opt here")
	}
	cfg := deskConfig(t)
	p := newDeskPipes(t)
	binds := []string{dir}
	if _, err := os.Stat("/etc/fonts"); err == nil {
		binds = append(binds, "/etc/fonts")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stderr bytes.Buffer
	ef, ew, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = io.Copy(&stderr, ef) }()
	done := make(chan error, 1)
	go func() {
		_, err := RunDesk(ctx, cfg, DeskSpec{Argv: []string{bin, "--headless=new", "--no-sandbox", "--no-zygote", "--disable-gpu",
			"--disable-dev-shm-usage", "--no-first-run", "--no-default-browser-check", "--remote-debugging-pipe",
			"--user-data-dir=/tmp/desk/profile", "about:blank"}, Env: []string{"HOME=/tmp/desk", "PATH=/usr/bin:/bin"},
			Binds: binds, MemBytes: 1 << 30, CPUMillis: 1000, DiskBytes: 256 << 20, Pids: 256, In: p.in, Out: p.out, Stderr: ew})
		done <- err
	}()
	ew.Close()
	answers := bufio.NewReader(p.answer)
	call := func(id int, method string, params any) map[string]any {
		t.Helper()
		msg, _ := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
		if _, err := p.write.Write(append(msg, 0)); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		deadline := time.Now().Add(45 * time.Second)
		for time.Now().Before(deadline) {
			raw, err := answers.ReadBytes(0)
			if err != nil {
				t.Fatalf("%s: the browser closed its pipe: %v\nstderr: %s", method, err, stderr.String())
			}
			var m map[string]any
			if json.Unmarshal(raw[:len(raw)-1], &m) == nil && m["id"] == float64(id) {
				return m
			}
		}
		t.Fatalf("%s: no answer", method)
		return nil
	}
	v := call(1, "Browser.getVersion", map[string]any{})
	res, _ := v["result"].(map[string]any)
	if res == nil || !strings.Contains(res["product"].(string), "Chrome") {
		t.Fatalf("Browser.getVersion: %v\nstderr: %s", v, stderr.String())
	}
	tg := call(2, "Target.createTarget", map[string]any{"url": "data:text/html,<title>desk</title><p>confined"})
	if r, _ := tg["result"].(map[string]any); r == nil || r["targetId"] == "" {
		t.Fatalf("Target.createTarget: %v", tg)
	}
	call(3, "Browser.close", map[string]any{})
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the browser did not end after Browser.close")
	}
	t.Logf("Chrome %s ran confined", res["product"])
}
