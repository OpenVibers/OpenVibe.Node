// Package localctl is the running Node's local control socket: `openvibe-node stop`, `resume`, `status` and
// `plugins` talk to it. It is a Unix domain socket (Windows 10+ supports them too) in the state directory with mode
// 0600, so only the account running the Node and root can use it. One JSON request per connection, one JSON answer.
package localctl

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"time"
)

// Commands.
const (
	CmdStatus  = "status"
	CmdStop    = "stop"
	CmdResume  = "resume"
	CmdPlugins = "plugins"
)

type Request struct {
	Cmd string `json:"cmd"`
}

type Response struct {
	OK    bool            `json:"ok"`
	Error string          `json:"error,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// Handler answers one request.
type Handler func(Request) Response

// Serve listens on path until ctx ends.
func Serve(ctx context.Context, path string, h Handler) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// A socket left by a crashed Node: remove it if nobody answers.
	if _, err := os.Stat(path); err == nil {
		if c, err := net.DialTimeout("unix", path, 200*time.Millisecond); err == nil {
			c.Close()
			return errors.New("another openvibe-node is already running (its control socket answers)")
		}
		_ = os.Remove(path)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	_ = os.Chmod(path, 0o600)
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	defer os.Remove(path)
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		go func(c net.Conn) {
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))
			var req Request
			if err := json.NewDecoder(c).Decode(&req); err != nil {
				_ = json.NewEncoder(c).Encode(Response{Error: "bad request"})
				return
			}
			_ = json.NewEncoder(c).Encode(h(req))
		}(c)
	}
}

// Call sends one request to a running Node.
func Call(path string, req Request, timeout time.Duration) (Response, error) {
	var resp Response
	c, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		return resp, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(timeout))
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return resp, err
	}
	if err := json.NewDecoder(c).Decode(&resp); err != nil {
		return resp, err
	}
	if !resp.OK && resp.Error != "" {
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}
