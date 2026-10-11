//go:build !windows

package main

import "errors"

var errUserSessionWindowsOnly = errors.New("--user-session is for Windows; on Linux and macOS install the service (sudo openvibe-node install), or start `openvibe-node run --home DIR` from your desktop's autostart")

func userSessionSupported() error                   { return errUserSessionWindowsOnly }
func registerUserSession(string) error              { return errUserSessionWindowsOnly }
func unregisterUserSession() error                  { return errUserSessionWindowsOnly }
func startUserSession(string, string, string) error { return errUserSessionWindowsOnly }
