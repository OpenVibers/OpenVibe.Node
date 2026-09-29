package video

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/OpenVibers/OpenVibe.Node/internal/credentials"
)

func TestPublishTestPatternOverWHIP(t *testing.T) {
	rx := &WHIPReceiver{PublishKey: "pk_test"}
	srv := httptest.NewServer(rx)
	defer srv.Close()
	defer rx.Close()
	p := NewPublisher(Options{WHIPURL: srv.URL + "/whip/dev_1", PublishKey: credentials.NewSecret("pk_test"),
		IncludeLoopback: true, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx, &TestPattern{W: 160, H: 96, FPS: 20}); close(done) }()
	deadline := time.Now().Add(15 * time.Second)
	for rx.Packets.Load() < 20 {
		if time.Now().After(deadline) {
			t.Fatalf("no media: state %s frames %d packets %d err %q", p.State(), p.Frames(), rx.Packets.Load(), p.LastError())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if p.State() != StateLive {
		t.Fatalf("state %s", p.State())
	}
	cancel()
	<-done
	if rx.Deletes.Load() != 1 {
		t.Fatalf("session not deleted: %d", rx.Deletes.Load())
	}
	if rx.BadAuth.Load() != 0 {
		t.Fatal("publish key not sent")
	}
}

func TestPublishRetriesOnBadKey(t *testing.T) {
	rx := &WHIPReceiver{PublishKey: "right"}
	srv := httptest.NewServer(rx)
	defer srv.Close()
	p := NewPublisher(Options{WHIPURL: srv.URL + "/whip/x", PublishKey: credentials.NewSecret("wrong-key-value"),
		IncludeLoopback: true, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx, &TestPattern{W: 32, H: 32, FPS: 5}); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for p.State() != StateRetrying {
		if time.Now().After(deadline) {
			t.Fatalf("state %s", p.State())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if e := p.LastError(); e == "" || contains(e, "wrong-key-value") {
		t.Fatalf("last error %q", e)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
