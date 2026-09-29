//go:build windows

package main

import "errors"

func chownLikeDir(path, dir string) {}

func chownTree(name string, dirs ...string) error {
	return errors.New("--user is not supported on Windows; the service runs as LocalSystem")
}
