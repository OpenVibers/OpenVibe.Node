package main

import (
	"bytes"
	"runtime"
	"strings"
	"testing"
)

func TestUserSessionCommand(t *testing.T) {
	got := userSessionCommand(`C:\Program Files\OpenVibe\openvibe-node.exe`, `C:\Users\m\AppData\Local\OpenVibe Node`, "info")
	want := `"C:\Program Files\OpenVibe\openvibe-node.exe" run --home "C:\Users\m\AppData\Local\OpenVibe Node"`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if got := userSessionCommand(`C:\bin\node.exe`, `D:\ov`, "debug"); got != `C:\bin\node.exe run --home D:\ov --log-level debug` {
		t.Fatalf("plain paths and the log level: %s", got)
	}
}

func TestUserSessionRefusedOffWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the refusal is for other systems")
	}
	var out, errb bytes.Buffer
	if code := run([]string{"install", "--user-session", "--home", t.TempDir()}, &out, &errb); code == 0 || !strings.Contains(errb.String(), "for Windows") {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
}
