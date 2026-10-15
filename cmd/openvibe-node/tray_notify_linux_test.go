//go:build linux

package main

import (
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/OpenVibers/OpenVibe.Node/internal/node"
)

// fakeNotifications is an org.freedesktop.Notifications server on a private session bus.
type fakeNotifications struct {
	conn   *dbus.Conn
	notify chan []any
	closed chan uint32
	next   uint32
}

func (f *fakeNotifications) GetCapabilities() ([]string, *dbus.Error) {
	return []string{"actions", "body", "body-markup"}, nil
}

func (f *fakeNotifications) Notify(app string, replaces uint32, icon, summary, body string, actions []string, hints map[string]dbus.Variant, expire int32) (uint32, *dbus.Error) {
	f.next++
	id := f.next
	if replaces != 0 {
		id = replaces
	}
	f.notify <- []any{id, summary, body, actions, expire}
	return id, nil
}

func (f *fakeNotifications) CloseNotification(id uint32) *dbus.Error {
	f.closed <- id
	return nil
}

// Run under a private bus: dbus-run-session -- go test ./cmd/openvibe-node -run DesktopNotifier
func TestDesktopNotifierAsksAndHearsTheButtons(t *testing.T) {
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		t.Skip("no session bus (run it under dbus-run-session)")
	}
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		t.Skip("session bus:", err)
	}
	defer conn.Close()
	reply, err := conn.RequestName(notifyName, dbus.NameFlagDoNotQueue)
	if err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		t.Skip("a real notification server owns the name: run it under dbus-run-session")
	}
	f := &fakeNotifications{conn: conn, notify: make(chan []any, 4), closed: make(chan uint32, 4)}
	if err := conn.Export(f, notifyPath, notifyIface); err != nil {
		t.Fatal(err)
	}
	n := newDesktopNotifier(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer n.Shut()
	n.Ask(node.Approval{ID: "c1", Kind: "exec", Summary: "run <b>rm</b> & co", ExpiresS: 120}, "")
	var got []any
	select {
	case got = <-f.notify:
	case <-time.After(5 * time.Second):
		t.Fatal("no Notify call")
	}
	if got[1] != "OpenVibe: your agent asks" || got[2] != "run &lt;b&gt;rm&lt;/b&gt; &amp; co" || got[4] != int32(120000) {
		t.Fatalf("notify: %v", got)
	}
	if a := got[3].([]string); len(a) != 4 || a[0] != "approve" || a[2] != "deny" {
		t.Fatalf("actions: %v", a)
	}
	nid := got[0].(uint32)
	// Somebody else on the bus faking the button: ignored. The server's own signal: an answer.
	other, err := dbus.ConnectSessionBus(dbus.WithSignalHandler(dbus.NewSequentialSignalHandler()))
	if err == nil {
		_ = other.Emit(notifyPath, notifyIface+".ActionInvoked", nid, "approve")
	}
	select {
	case a := <-n.Answers():
		t.Fatalf("an impostor's signal answered: %+v", a)
	case <-time.After(300 * time.Millisecond):
	}
	if err := conn.Emit(notifyPath, notifyIface+".ActionInvoked", nid, "approve"); err != nil {
		t.Fatal(err)
	}
	select {
	case a := <-n.Answers():
		if a.ID != "c1" || !a.Approved {
			t.Fatalf("answer: %+v", a)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the Approve button was not heard")
	}
	n.Ask(node.Approval{ID: "c1", Summary: "again"}, "Not taken")
	if again := <-f.notify; again[0] != nid {
		t.Fatalf("asking again replaces the same notification: %v", again)
	}
	n.Close("c1")
	select {
	case id := <-f.closed:
		if id != nid {
			t.Fatalf("closed %d, want %d", id, nid)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("not closed")
	}
}
