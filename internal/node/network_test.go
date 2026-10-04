package node

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/OpenVibers/OpenVibe.Node/internal/config"
	"github.com/OpenVibers/OpenVibe.Node/internal/fakebot"
	"github.com/OpenVibers/OpenVibe.Node/internal/fakenetwork"
	"github.com/OpenVibers/OpenVibe.Node/internal/link"
	"github.com/OpenVibers/OpenVibe.Node/internal/protocol"
)

// A Network-paired Node connects with node tokens and stops on its own, with link.ErrNodeRevoked, once Network
// refuses its credential: no retry loop.
func TestNetworkPairedNodeStopsWhenRevoked(t *testing.T) {
	home, err := os.MkdirTemp("", "ovn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	bot := fakebot.New()
	t.Cleanup(bot.Close)
	net := fakenetwork.New(bot)
	t.Cleanup(net.Close)
	net.AddCode("TEST2345")
	creds, err := link.PairNetwork(context.Background(), nil, net.URL(), protocol.NodePairingRequest{Code: "TEST2345"}, "t")
	if err != nil {
		t.Fatal(err)
	}
	creds.Server = bot.URL()
	if creds, err = link.Bind(context.Background(), nil, creds, link.NewTokenSource(creds, nil, "t"), "t"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Plugins = nil
	cfg.Video.Source = "off"
	n, err := New(Options{Config: cfg, Paths: config.HomePaths(home), Creds: creds, Version: "test", HeartbeatInterval: 100 * time.Millisecond,
		ReauthInterval: 200 * time.Millisecond, LinkBackoffMin: 50 * time.Millisecond, LatchPoll: 50 * time.Millisecond,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- n.Run(ctx) }()
	conn, err := bot.NextConn(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Expect(protocol.TypeReauth, 3*time.Second, nil); err != nil {
		t.Fatal(err)
	}
	net.Revoke()
	select {
	case err := <-done:
		if !errors.Is(err, link.ErrNodeRevoked) {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the node kept running with a revoked credential")
	}
}
