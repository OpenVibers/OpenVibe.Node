package localctl

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestServeCall(t *testing.T) {
	dir, err := os.MkdirTemp("", "ovn") // short path: unix socket paths are limited to ~100 bytes
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "n.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, path, func(r Request) Response {
			if r.Cmd == CmdStop {
				return Response{OK: true, Data: []byte(`"stopped"`)}
			}
			return Response{Error: "unknown"}
		})
	}()
	var resp Response
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err = Call(path, Request{Cmd: CmdStop}, time.Second)
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil || string(resp.Data) != `"stopped"` {
		t.Fatalf("%v %+v", err, resp)
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(path)
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("socket mode %o", st.Mode().Perm())
		}
	}
	if _, err := Call(path, Request{Cmd: "nope"}, time.Second); err == nil {
		t.Fatal("unknown accepted")
	}
	// A second server refuses to start.
	if err := Serve(context.Background(), path, nil); err == nil {
		t.Fatal("second server started")
	}
	cancel()
	<-done
	if _, err := os.Stat(path); err == nil {
		t.Fatal("socket left behind")
	}
}
