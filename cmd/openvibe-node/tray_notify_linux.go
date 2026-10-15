//go:build linux

package main

import (
	"log/slog"
	"strings"
	"sync"

	"github.com/godbus/dbus/v5"

	"github.com/OpenVibers/OpenVibe.Node/internal/node"
)

// Desktop notifications over org.freedesktop.Notifications (every Linux desktop): a waiting request with Approve and
// Deny buttons when the server has "actions", else the command to answer with.

const (
	notifyName  = "org.freedesktop.Notifications"
	notifyPath  = dbus.ObjectPath("/org/freedesktop/Notifications")
	notifyIface = "org.freedesktop.Notifications"
)

type dbusNotifier struct {
	conn    *dbus.Conn
	obj     dbus.BusObject
	actions bool
	markup  bool
	log     *slog.Logger
	mu      sync.Mutex
	byID    map[string]uint32 // approval id → notification id
	byNote  map[uint32]string
	owner   string // the notification server's unique bus name: only its signals count
	answers chan trayAnswer
}

func newDesktopNotifier(log *slog.Logger) notifier {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		log.Warn("tray: no desktop notifications (no session bus)", "err", err)
		return nopNotifier{}
	}
	n := &dbusNotifier{conn: conn, obj: conn.Object(notifyName, notifyPath), log: log,
		byID: map[string]uint32{}, byNote: map[uint32]string{}, answers: make(chan trayAnswer, 8)}
	var caps []string
	if err := n.obj.Call(notifyIface+".GetCapabilities", 0).Store(&caps); err == nil {
		for _, c := range caps {
			n.actions = n.actions || c == "actions"
			n.markup = n.markup || c == "body-markup"
		}
	}
	_ = conn.BusObject().Call("org.freedesktop.DBus.GetNameOwner", 0, notifyName).Store(&n.owner)
	var host bool
	if conn.BusObject().Call("org.freedesktop.DBus.NameHasOwner", 0, "org.kde.StatusNotifierWatcher").Store(&host) == nil && !host {
		log.Warn("tray: this desktop shows no tray icons (on GNOME, the AppIndicator extension adds them); the notifications still ask")
	}
	if err := conn.AddMatchSignal(dbus.WithMatchInterface(notifyIface), dbus.WithMatchObjectPath(notifyPath)); err != nil {
		log.Warn("tray: notification buttons will not answer", "err", err)
	}
	ch := make(chan *dbus.Signal, 16)
	conn.Signal(ch)
	go n.listen(ch)
	return n
}

func (n *dbusNotifier) listen(ch <-chan *dbus.Signal) {
	for sig := range ch {
		if sig == nil || sig.Path != notifyPath {
			continue
		}
		if n.owner != "" && sig.Sender != n.owner {
			// The server may have restarted: ask who owns the name now before trusting it.
			var owner string
			if err := n.conn.BusObject().Call("org.freedesktop.DBus.GetNameOwner", 0, notifyName).Store(&owner); err != nil || owner != sig.Sender {
				continue
			}
			n.owner = owner
		}
		switch sig.Name {
		case notifyIface + ".ActionInvoked":
			if len(sig.Body) != 2 {
				continue
			}
			nid, _ := sig.Body[0].(uint32)
			key, _ := sig.Body[1].(string)
			n.mu.Lock()
			id := n.byNote[nid]
			n.mu.Unlock()
			if id == "" || (key != "approve" && key != "deny") {
				continue
			}
			select {
			case n.answers <- trayAnswer{ID: id, Approved: key == "approve"}:
			default:
			}
		case notifyIface + ".NotificationClosed":
			if len(sig.Body) < 1 {
				continue
			}
			nid, _ := sig.Body[0].(uint32)
			n.mu.Lock()
			if id, ok := n.byNote[nid]; ok {
				delete(n.byNote, nid)
				if n.byID[id] == nid {
					delete(n.byID, id)
				}
			}
			n.mu.Unlock()
		}
	}
}

func (n *dbusNotifier) text(s string) string {
	if !n.markup {
		return s
	}
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func (n *dbusNotifier) Ask(a node.Approval, again string) {
	body := a.Summary
	if again != "" {
		body = again + "\n" + body
	}
	var actions []string
	if n.actions {
		actions = []string{"approve", "Approve", "deny", "Deny"}
	} else {
		body += "\nopenvibe-node approve " + a.ID + "  (or deny " + a.ID + ")"
	}
	n.mu.Lock()
	replaces := n.byID[a.ID]
	n.mu.Unlock()
	expire := int32(-1)
	if a.ExpiresS > 0 {
		expire = int32(a.ExpiresS * 1000)
	}
	hints := map[string]dbus.Variant{"urgency": dbus.MakeVariant(byte(2)), "desktop-entry": dbus.MakeVariant("openvibe-node-tray")}
	var nid uint32
	err := n.obj.Call(notifyIface+".Notify", 0, "OpenVibe Node", replaces, "dialog-question", "OpenVibe: your agent asks",
		n.text(body), actions, hints, expire).Store(&nid)
	if err != nil {
		n.log.Warn("tray: notification failed", "err", err)
		return
	}
	n.mu.Lock()
	n.byID[a.ID] = nid
	n.byNote[nid] = a.ID
	n.mu.Unlock()
}

func (n *dbusNotifier) Close(id string) {
	n.mu.Lock()
	nid, ok := n.byID[id]
	delete(n.byID, id)
	if ok {
		delete(n.byNote, nid)
	}
	n.mu.Unlock()
	if ok {
		n.obj.Call(notifyIface+".CloseNotification", 0, nid)
	}
}

func (n *dbusNotifier) Info(title, body string) {
	hints := map[string]dbus.Variant{"urgency": dbus.MakeVariant(byte(1))}
	n.obj.Call(notifyIface+".Notify", 0, "OpenVibe Node", uint32(0), "dialog-information", title, n.text(body), []string{}, hints, int32(6000))
}

func (n *dbusNotifier) Answers() <-chan trayAnswer { return n.answers }

func (n *dbusNotifier) Shut() { _ = n.conn.Close() }
