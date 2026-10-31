//go:build !windows

package node

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenVibers/OpenVibe.Node/internal/config"
	"github.com/OpenVibers/OpenVibe.Node/internal/fakebot"
	"github.com/OpenVibers/OpenVibe.Node/internal/link"
	"github.com/OpenVibers/OpenVibe.Node/internal/protocol"
)

// The machine roles of the network's hub (plan L34): the Node reads them in the config frame at its check-in, keeps
// them, and applies the ones this machine has. A role it does not know is ignored, never fatal, and the local
// configuration always wins (ADR-045).
func TestRolesAtCheckIn(t *testing.T) {
	home, err := os.MkdirTemp("", "ovn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	bot := fakebot.New()
	t.Cleanup(bot.Close)
	bot.Config = protocol.Config{HeartbeatMS: 100,
		Limits:          protocol.Limits{MaxSpeed: f64(0.5), MaxTurn: f64(0.4), MaxCommandMS: 1000},
		AllowedCommands: append([]string(nil), protocol.Kinds...),
		Roles:           []string{"codes.worker", "desks.host", "nope.nope"}}
	bot.AddCode("TEST2345")
	creds, err := link.Pair(context.Background(), nil, bot.URL(), protocol.PairRequest{Code: "TEST2345", AgentVersion: "test",
		DeviceKind: "onboard"})
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	cfg := config.Default()
	cfg.Plugins = nil
	cfg.Video.Source = "off"
	n, err := New(Options{Config: cfg, Paths: config.HomePaths(home), Creds: creds, Version: "test", HeartbeatInterval: 100 * time.Millisecond,
		LinkBackoffMin: 50 * time.Millisecond, LatchPoll: 50 * time.Millisecond,
		Log: slog.New(slog.NewTextHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- n.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the node did not stop")
		}
	})
	if _, err := bot.NextConn(10 * time.Second); err != nil {
		t.Fatal(err)
	}
	// The roles are in the machine's status as soon as the config was applied.
	deadline := time.Now().Add(10 * time.Second)
	for {
		roles := n.Status().Roles
		if len(roles) == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the roles never arrived: %v", roles)
		}
		time.Sleep(10 * time.Millisecond)
	}
	got := strings.Join(n.Status().Roles, ",")
	if got != "codes.worker,desks.host,nope.nope" {
		t.Fatalf("the roles are %q", got)
	}
	// The role the machine does not know is ignored, and the worker role does not turn on what the local
	// configuration turned off.
	text := logs.String()
	if !strings.Contains(text, "role not known here") {
		t.Errorf("an unknown role was not ignored: %s", text)
	}
	if !strings.Contains(text, "local configuration wins") {
		t.Errorf("the local configuration did not win over the role: %s", text)
	}
	if !strings.Contains(text, "no desk host installed") {
		t.Errorf("the desk host role was not said to be unapplied: %s", text)
	}
}

// hasDeskHost and hasAIHost: the features a machine was installed for, which the roles name.
func TestRoleFeatures(t *testing.T) {
	cfg := config.Default()
	cfg.Plugins = []config.PluginConfig{
		{Name: "computer", Config: map[string]any{"desk": map[string]any{"max_desks": 2}}},
		{Name: "ai"},
	}
	n := &Node{opt: Options{Config: cfg}}
	if !n.hasDeskHost() {
		t.Error("the computer plugin with a desk is a desk host")
	}
	if !n.hasAIHost() {
		t.Error("the ai plugin serves the machine's own models")
	}
	plain := &Node{opt: Options{Config: config.Default()}}
	if plain.hasDeskHost() || plain.hasAIHost() {
		t.Error("a machine with neither plugin has neither feature")
	}
}

// syncBuffer collects the log lines of a test.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

var _ io.Writer = (*syncBuffer)(nil)
