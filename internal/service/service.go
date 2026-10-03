// Package service installs and runs the Node as a system service (systemd, launchd or the Windows service manager)
// through kardianos/service. The same code path runs `openvibe-node run` in a terminal: Ctrl-C stops it cleanly.
package service

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"time"

	ks "github.com/kardianos/service"
)

const (
	Name        = "openvibe-node"
	DisplayName = "OpenVibe Node"
	Description = "Connects this device to OpenVibe (robots, cameras, computer control) over one outbound link."
)

// Program is the body of the service: run until ctx ends.
type Program func(ctx context.Context) error

type program struct {
	run    Program
	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

func (p *program) Start(s ks.Service) error {
	ctx, cancel := context.WithCancel(context.Background())
	p.mu.Lock()
	p.cancel, p.done = cancel, make(chan struct{})
	p.mu.Unlock()
	go func() {
		err := p.run(ctx)
		p.mu.Lock()
		p.err = err
		p.mu.Unlock()
		close(p.done)
		if err != nil && ctx.Err() == nil {
			// The Node ended on its own (for example a second instance): let the service manager restart it.
			_ = s.Stop()
		}
	}()
	return nil
}

// Stop cancels the Node and waits for it to stop every actuator (at most 15 s).
func (p *program) Stop(ks.Service) error {
	p.mu.Lock()
	cancel, done := p.cancel, p.done
	p.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		return errors.New("timed out waiting for the node to stop")
	}
	return nil
}

// Options for the service definition.
type Options struct {
	// Arguments passed to the executable by the service manager (["run"] plus --home when set).
	Arguments []string
	// UserName runs the service as this account (Linux and macOS); empty = the service manager's default (root).
	UserName string
}

func config(opt Options) *ks.Config {
	c := &ks.Config{
		Name: Name, DisplayName: DisplayName, Description: Description, Arguments: opt.Arguments, UserName: opt.UserName,
		Option: ks.KeyValue{
			"Restart":                "always", // systemd
			"SuccessExitStatus":      "0",
			"KeepAlive":              true, // launchd
			"RunAtLoad":              true,
			"OnFailure":              "restart", // Windows
			"OnFailureDelayDuration": "5s",
			"DelayedAutoStart":       false,
			"SystemdScript":          systemdScript,
		},
	}
	if runtime.GOOS == "darwin" {
		// launchd has no journal: write /usr/local/var/log/openvibe-node.{out,err}.log. systemd keeps journald.
		c.Option["LogOutput"] = true
	}
	return c
}

// systemdScript is kardianos/service's systemd unit with Delegate=yes: the worker makes a cgroup per job inside the
// Node's own (internal/worker), which systemd allows only in a delegated cgroup.
const systemdScript = `[Unit]
Description={{Description}}
ConditionFileIsExecutable={{Path | cmdEscape}}
{{range Dependencies}}{{.}}
{{end}}
[Service]
StartLimitInterval=5
StartLimitBurst=10
ExecStart={{Path | cmdEscape}}{{range Arguments}} {{. | cmd}}{{end}}
{{if ChRoot}}RootDirectory={{ChRoot | cmd}}
{{end}}{{if WorkingDirectory}}WorkingDirectory={{WorkingDirectory | cmdEscape}}
{{end}}{{if UserName}}User={{UserName}}
{{end}}{{if ReloadSignal}}ExecReload=/bin/kill -{{ReloadSignal}} "$MAINPID"
{{end}}{{if PIDFile}}PIDFile={{PIDFile | cmd}}
{{end}}{{if OutputFileSupport}}StandardOutput=file:{{LogDirectory}}/{{Name}}.out
StandardError=file:{{LogDirectory}}/{{Name}}.err
{{end}}{{if LimitNOFILE}}LimitNOFILE={{LimitNOFILE}}
{{end}}{{if Restart}}Restart={{Restart}}
{{end}}{{if SuccessExitStatus}}SuccessExitStatus={{SuccessExitStatus}}
{{end}}RestartSec=120
Delegate=yes
EnvironmentFile=-/etc/sysconfig/{{Name}}

{{range EnvVars}}{{.}}
{{end}}[Install]
WantedBy=multi-user.target
`

// Run runs program under the service manager, or in the foreground when started from a terminal.
func Run(opt Options, run Program) error {
	p := &program{run: run}
	s, err := ks.New(p, config(opt))
	if err != nil {
		return err
	}
	if err := s.Run(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// Control performs install, uninstall, start, stop or restart on the service definition.
func Control(opt Options, action string) error {
	s, err := ks.New(&program{}, config(opt))
	if err != nil {
		return err
	}
	return ks.Control(s, action)
}

// Status reports running, stopped, not installed or unknown.
func Status(opt Options) string {
	s, err := ks.New(&program{}, config(opt))
	if err != nil {
		return "unknown"
	}
	st, err := s.Status()
	if errors.Is(err, ks.ErrNotInstalled) {
		return "not installed"
	}
	if err != nil {
		return "unknown"
	}
	switch st {
	case ks.StatusRunning:
		return "running"
	case ks.StatusStopped:
		return "stopped"
	}
	return "unknown"
}

// Interactive reports whether the process runs in a terminal rather than under a service manager.
func Interactive() bool { return ks.Interactive() }
