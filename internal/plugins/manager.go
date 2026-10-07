package plugins

import (
	"context"
	"log/slog"
	"sync"

	"github.com/OpenVibers/OpenVibe.Node/internal/protocol"
)

// capabilityFor maps a command kind to the describe capability key that declares it.
var capabilityFor = map[string]string{
	protocol.KindDrive:    "drive",
	protocol.KindPTZ:      "ptz",
	protocol.KindActuator: "actuator",
	protocol.KindSay:      "say",
	protocol.KindDisplay:  "display",
	protocol.KindButton:   "button",
	protocol.KindPoint:    "point",
}

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

// StopAll tells every plugin to stop every actuator.
func (m *Manager) StopAll() {
	for _, p := range m.plugins {
		p.Stop()
	}
}

func (m *Manager) EstopAll() {
	for _, p := range m.plugins {
		p.Estop()
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
	return fallback
}
