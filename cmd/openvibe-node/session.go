package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// The user-session start (Windows): instead of the LocalSystem service, the Node starts when you sign in, in your
// own session, from your Run key (HKCU\Software\Microsoft\Windows\CurrentVersion\Run; no administrator needed). This
// is for drivers that need what only a signed-in session has, such as Bluetooth through the Windows runtime for a
// bridge robot (docs/hardware-looi.md). It runs in a console window that shows the log; closing the window stops the
// Node, and every actuator stops with it (the plugins' heartbeat and stdin rules). Nothing restarts it until the next
// sign-in.

const (
	runKey       = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`
	runValueName = "OpenVibe Node"
)

var errNeedHome = errors.New(`--user-session needs --home DIR: the directory you paired with, for example --home "%LOCALAPPDATA%\OpenVibe Node"`)

// userSessionCommand is the command line the Run key starts: the executable, `run`, and the Node's own directory.
func userSessionCommand(exe, home, logLevel string) string {
	parts := []string{quoteArg(exe), "run", "--home", quoteArg(home)}
	if logLevel != "" && logLevel != "info" {
		parts = append(parts, "--log-level", logLevel)
	}
	return strings.Join(parts, " ")
}

// quoteArg quotes one Windows command-line argument (paths with spaces, such as "OpenVibe Node").
func quoteArg(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"") {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

func cmdInstallUserSession(g *globals, stdout io.Writer) error {
	if err := userSessionSupported(); err != nil {
		return err
	}
	if g.home == "" {
		return errNeedHome
	}
	home, err := filepath.Abs(g.home)
	if err != nil {
		return err
	}
	if err := g.paths.Ensure(); err != nil {
		return fmt.Errorf("cannot create %s: %w", home, err)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	line := userSessionCommand(exe, home, g.logLevel)
	if err := registerUserSession(line); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "The Node now starts when you sign in, in its own window:", line)
	if err := startUserSession(exe, home, g.logLevel); err != nil {
		fmt.Fprintln(stdout, "It did not start now (", err, "); sign out and in, or run the line above.")
	} else {
		fmt.Fprintln(stdout, "Started it now. Closing its window stops the Node and every actuator.")
	}
	fmt.Fprintln(stdout, "Remove the sign-in start with `openvibe-node uninstall --user-session`.")
	return nil
}

func cmdUninstallUserSession(stdout io.Writer) error {
	if err := userSessionSupported(); err != nil {
		return err
	}
	if err := unregisterUserSession(); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "The Node no longer starts at sign-in. Close its window to stop the running one; config and credential are kept.")
	return nil
}
