package main

import (
	"bytes"
	"os"
	"runtime"
	"strings"
	"testing"
)

// desk-sandbox reads its own flags up to "--" (a browser's flags after it are never the Node's), shows its usage
// without a program, and refuses (125, never running the program unconfined) where desks cannot be sandboxed: here a
// config with no worker.run_as.
func TestDeskSandboxCommand(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"desk-sandbox"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "usage: openvibe-node desk-sandbox") {
		t.Fatalf("no program: code %d, stderr %q", code, errb.String())
	}
	errb.Reset()
	// Never the test process's own fds 3 and 4: they are the Go runtime's poller here.
	in, out2, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	defer out2.Close()
	deskPipe = func() (*os.File, *os.File, error) { return in, out2, nil }
	defer func() { deskPipe = inheritedDeskPipe }()
	home := t.TempDir()
	code := run([]string{"desk-sandbox", "--home", home, "--memory-mb", "64", "--", "/bin/true", "--headless=new", "--home", "x"}, &out, &errb)
	if code != 125 || !strings.Contains(errb.String(), "desk-sandbox:") {
		t.Fatalf("unconfinable: code %d, stderr %q", code, errb.String())
	}
	if runtime.GOOS == "linux" && !strings.Contains(errb.String(), "worker.run_as") {
		t.Fatalf("the refusal should say what is missing: %q", errb.String())
	}
}
