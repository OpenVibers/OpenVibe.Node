//go:build windows

package main

import (
	"fmt"
	"os/exec"
	"strings"
)

func userSessionSupported() error { return nil }

func registerUserSession(line string) error {
	out, err := exec.Command("reg", "add", runKey, "/v", runValueName, "/t", "REG_SZ", "/d", line, "/f").CombinedOutput()
	if err != nil {
		return fmt.Errorf("reg add %s: %v: %s", runKey, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func unregisterUserSession() error {
	out, err := exec.Command("reg", "delete", runKey, "/v", runValueName, "/f").CombinedOutput()
	if err != nil && !strings.Contains(strings.ToLower(string(out)), "unable to find") {
		return fmt.Errorf("reg delete %s: %v: %s", runKey, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// startUserSession starts the Node now in a window of its own, as the Run key will at the next sign-in.
func startUserSession(exe, home, logLevel string) error {
	args := []string{"/c", "start", "OpenVibe Node", exe, "run", "--home", home}
	if logLevel != "" && logLevel != "info" {
		args = append(args, "--log-level", logLevel)
	}
	return exec.Command("cmd", args...).Start()
}
