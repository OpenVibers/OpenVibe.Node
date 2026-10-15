//go:build !linux

package main

import "log/slog"

// Desktop notifications with buttons are Linux's (org.freedesktop.Notifications); on Windows the icon turns amber and
// the menu lists what waits.
func newDesktopNotifier(*slog.Logger) notifier { return nopNotifier{} }
