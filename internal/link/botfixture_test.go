package link

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/OpenVibers/OpenVibe.Node/internal/fakebot"
	"github.com/OpenVibers/OpenVibe.Node/internal/protocol"
)

func botFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "protocol", "testdata", "bot", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.TrimSpace(b)
}

// TestBotFixturesEndToEnd drives the fake server with the exact frames of OpenVibe.Bot's docs/protocol.md
// (internal/protocol/testdata/bot): pair → connect (hello + config) → command → ack → reconnect.
func TestBotFixturesEndToEnd(t *testing.T) {
	srv := fakebot.New()
	defer srv.Close()
	srv.PairBody = protocol.PairBodyFromPaired(botFixture(t, "paired"))
	srv.Greeting = [][]byte{botFixture(t, "hello"), botFixture(t, "config")}

	// Pair with the fields of Bot's pair frame (the code normalized, as `openvibe-node pair` does).
	var req protocol.PairRequest
	if err := json.Unmarshal(botFixture(t, "pair"), &req); err != nil {
		t.Fatal(err)
	}
	code, err := NormalizeCode(req.Code)
	if err != nil {
		t.Fatal(err)
	}
	srv.AddCode(code)
	req.Code = code
	creds, err := Pair(context.Background(), nil, srv.URL(), req)
	if err != nil {
		t.Fatal(err)
	}
	if creds.DeviceID != "dev_01J8Z4…" || creds.Credential.Reveal() != "Xb3…" || creds.PublishKey.Reveal() != "Vt9…" ||
		creds.RobotID != "rob_01J8Z4M2Q0R7T9YV3K6N8P1W2X" || len(creds.Profile) == 0 {
		t.Fatalf("credentials %+v", creds)
	}
	if got := srv.PairRequests(); len(got) != 1 || !reflect.DeepEqual(got[0], req) {
		t.Fatalf("pair request %+v, want %+v", got, req)
	}

	// Connect: hello and config arrive as Bot sends them.
	u, _ := DeviceURL(srv.URL())
	rec := newRecorder()
	l := New(Options{URL: u, Credential: creds.Credential, HeartbeatInterval: 100 * time.Millisecond, BackoffMin: 20 * time.Millisecond,
		BackoffMax: 100 * time.Millisecond, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, rec)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	greet := func() *fakebot.Conn {
		t.Helper()
		c, err := srv.NextConn(3 * time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if h := waitFrame(t, rec, protocol.TypeHello).Msg.(protocol.Hello); h.DeviceID != creds.DeviceID || h.SessionID != "sess_01J8Z4…" {
			t.Fatalf("hello %+v", h)
		}
		if cf := waitFrame(t, rec, protocol.TypeConfig).Msg.(protocol.Config); cf.HeartbeatMS != 1000 || cf.Limits.MaxCommandMS != 300 ||
			len(cf.AllowedCommands) != 6 || cf.EstopLatched {
			t.Fatalf("config %+v", cf)
		}
		if !l.Connected() {
			t.Fatal("not connected after hello")
		}
		return c
	}
	c := greet()

	// Command → ack, both in Bot's shapes.
	if err := c.SendRaw(botFixture(t, "command_drive")); err != nil {
		t.Fatal(err)
	}
	cmd := waitFrame(t, rec, protocol.TypeCommand).Msg.(protocol.Command)
	if cmd.ID != "cmd_01J8Z4F…" || cmd.Kind != protocol.KindDrive || cmd.DeadlineMS != 1738065600300 || cmd.Operator == nil ||
		cmd.Operator.Subject != "usr_01J8…" || cmd.RobotID != creds.RobotID {
		t.Fatalf("command %+v", cmd)
	}
	if err := l.Send(protocol.Ack{ID: cmd.ID}); err != nil {
		t.Fatal(err)
	}
	r, err := c.Reply(cmd.ID, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	want, err := protocol.Decode(botFixture(t, "ack"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r, want.Msg) {
		t.Fatalf("ack %+v, want %+v", r, want.Msg)
	}
	// Heartbeats carry Bot's seq and are acked by it.
	if f, err := c.Expect(protocol.TypeHeartbeat, 2*time.Second, nil); err != nil || f.Msg.(protocol.Heartbeat).Seq != f.Seq {
		t.Fatalf("heartbeat %+v %v", f, err)
	}

	// Reconnect: the link comes back with the same credential and gets hello + config again.
	c.Close()
	<-rec.gotDown
	greet()
	if n := srv.Connections(); n != 2 {
		t.Fatalf("connections %d", n)
	}
}
