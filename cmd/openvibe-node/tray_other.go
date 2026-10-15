//go:build !linux && !windows

package main

import (
	"errors"
	"io"
)

// The tray library needs cgo on macOS and the release builds without it: there the computer plugin asks with a
// dialog for each request instead (docs/computer.md).
var errTrayPlatform = errors.New("the tray is for Linux and Windows; on macOS the computer plugin asks with a dialog for each request (Approve or Deny)")

func runTray(*globals) error                              { return errTrayPlatform }
func trayAutostart(bool, string, string, io.Writer) error { return errTrayPlatform }
