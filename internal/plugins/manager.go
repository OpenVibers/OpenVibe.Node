package plugins

import (
	"context"
	"log/slog"
	"sync"

	"github.com/OpenVibers/OpenVibe.Node/internal/protocol"
)

// capabilityFor maps a command kind to the describe capability key that declares it.
var capabilityFor = map[string]string{
	protocol.KindDrive:        "drive",
	protocol.KindPTZ:          "ptz",
	protocol.KindActuator:     "actuator",
	protocol.KindSay:          "say",
	protocol.KindDisplay:      "display",
	protocol.KindButton:       "button",
	protocol.KindPoint:        "point",
	protocol.KindExec:         "terminal",
	protocol.KindFS:           "files",
	protocol.KindScreen:       "screen",
	protocol.KindScreenStream: "screen",
	protocol.KindProbe:        "probe",
	protocol.KindInput:        "input",
	protocol.KindBrowser:      "browser",
	protocol.KindDesk:         "desk",
	protocol.KindAI:           "ai",
}

// deskKinds are the kinds a plugin's desks take too. A desk host (install.sh --desk-host) has no screen, mouse or
// keyboard of its own, only its desks', so its computer plugin offers no "screen" or "input": these kinds then reach
// the plugin that offers "desk", which acts on the desk a command names and refuses the machine's own screen itself.
// A desk host for other people (desk_isolation sandbox, v0.32) offers no browser of its own either: "browser" on one
// of its desks reaches the plugin the same way.
var deskKinds = map[string]bool{protocol.KindScreen: true, protocol.KindScreenStream: true, protocol.KindInput: true, protocol.KindBrowser: true}

// Manager supervises every configured plugin.
type Manager struct {
	log     *slog.Logger
	plugins []*Plugin
	out     chan Output
	wg      sync.WaitGroup
}

func NewManager(specs []Spec, log *slog.Logger) *Manager {
	m := &Manager{log: log, out: make(chan Output, 1024)}
	for _, s := range specs {
		m.plugins = append(m.plugins, newPlugin(s, log, m.out))
	}
	return m
}

// Output is the stream of telemetry, events, video frames, faults and state changes from every plugin.
func (m *Manager) Output() <-chan Output { return m.out }

// Start supervises every plugin until ctx ends. Wait returns once all have stopped their actuators and exited.
func (m *Manager) Start(ctx context.Context) {
	for _, p := range m.plugins {
		m.wg.Add(1)
		go func(p *Plugin) {
			defer m.wg.Done()
			p.run(ctx)
		}(p)
	}
}

func (m *Manager) Wait() { m.wg.Wait() }

func (m *Manager) Plugins() []*Plugin { return m.plugins }

func (m *Manager) Get(name string) *Plugin {
	for _, p := range m.plugins {
		if p.spec.Name == name {
			return p
		}
	}
	return nil
}

// StopAll tells every plugin to stop every actuator, saying why (StopLinkLost, StopShutdown).
func (m *Manager) StopAll(cause string) {
	for _, p := range m.plugins {
		p.Stop(cause)
	}
}

// HaltAll stops every plugin for a halt command, telling each the halt's turn_epoch (Plugin.Halt).
func (m *Manager) HaltAll(turnEpoch int64) {
	for _, p := range m.plugins {
		p.Halt(turnEpoch)
	}
}

// EstopAll latches every plugin's e-stop, saying why (EstopRemote, EstopLocal, EstopDriver, EstopLatched).
func (m *Manager) EstopAll(cause, reason string) {
	for _, p := range m.plugins {
		p.Estop(cause, reason)
	}
}

func (m *Manager) ResumeAll() {
	for _, p := range m.plugins {
		p.Resume()
	}
}

// Infos snapshots every plugin.
func (m *Manager) Infos() []Info {
	out := make([]Info, 0, len(m.plugins))
	for _, p := range m.plugins {
		out = append(out, p.Info())
	}
	return out
}

// Route picks the plugin for a command: the named target, or the first plugin whose describe declares the kind's
// capability (for actuators, the first that lists the actuator's name or "any"). Plugins that have not described
// themselves yet are considered by their configured order only when targeted by name.
func (m *Manager) Route(kind, target, actuator string) *Plugin {
	if target != "" {
		return m.Get(target)
	}
	key := capabilityFor[kind]
	if key == "" {
		return nil
	}
	var fallback *Plugin
	for _, p := range m.plugins {
		info := p.Info()
		if info.Describe == nil {
			continue
		}
		c, ok := info.Describe.Capabilities[key]
		if !ok {
			continue
		}
		if kind != protocol.KindActuator || actuator == "" {
			return p
		}
		if names, ok := c.(map[string]any)["names"].([]any); ok {
			for _, n := range names {
				if n == actuator || n == "any" {
					return p
				}
			}
		}
		if fallback == nil {
			fallback = p
		}
	}
	if fallback == nil && deskKinds[kind] {
		for _, p := range m.plugins {
			if info := p.Info(); info.Describe != nil {
				if _, ok := info.Describe.Capabilities["desk"]; ok {
					return p
				}
			}
		}
	}
	return fallback
}
