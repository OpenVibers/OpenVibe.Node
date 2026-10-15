package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenVibers/OpenVibe.Node/internal/localctl"
	"github.com/OpenVibers/OpenVibe.Node/internal/node"
	"github.com/OpenVibers/OpenVibe.Node/internal/safety"
)

func TestTrayViewStates(t *testing.T) {
	if v := viewOf(nil, nil, errors.New("dial unix: no such file")); v.State != trayDown || !strings.Contains(v.Tooltip, "not running") {
		t.Fatalf("no Node: %+v", v)
	}
	st := &node.Status{Server: "https://openvibe.bot", Link: node.LinkStatus{Connected: true}}
	st.Telemetry.Sensors = map[string]any{"computer_level": "ask"}
	v := viewOf(st, nil, nil)
	if v.State != trayOK || v.Level != "ask" || v.Panel != "https://openvibe.bot/machines" || v.Tooltip != "OpenVibe Node: online, computer level ask" {
		t.Fatalf("all well: %+v", v)
	}
	v = viewOf(st, []node.Approval{{ID: "a"}, {ID: "b"}}, nil)
	if v.State != trayWaiting || v.Header != "2 requests wait for your answer" || !strings.HasSuffix(v.Tooltip, "2 waiting") {
		t.Fatalf("waiting: %+v", v)
	}
	if v = viewOf(st, []node.Approval{{ID: "a"}}, nil); v.Header != "1 request waits for your answer" {
		t.Fatalf("one waiting: %q", v.Header)
	}
	st.Latch = safety.LatchState{Local: true}
	if v = viewOf(st, []node.Approval{{ID: "a"}}, nil); v.State != trayStopped || !v.Stopped {
		t.Fatalf("a latched stop wins over waiting: %+v", v)
	}
}

func TestTrayPanelURLOnlyForTheBot(t *testing.T) {
	for in, want := range map[string]string{
		"https://openvibe.bot":         "https://openvibe.bot/machines",
		"https://openvibe.bot/api/x":   "https://openvibe.bot/machines",
		"http://127.0.0.1:4100":        "http://127.0.0.1:4100/machines",
		"http://example.com":           "",
		"javascript:alert(1)":          "",
		"":                             "",
		"file:///etc/passwd":           "",
		"https://user@openvibe.bot:44": "https://openvibe.bot:44/machines", // no user info
	} {
		if got := panelURL(in); got != want {
			t.Errorf("panelURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTraySlotLabel(t *testing.T) {
	if got := slotLabel(node.Approval{Kind: "exec", Summary: "run  echo\nhi"}); got != "exec: run echo hi" {
		t.Fatalf("label: %q", got)
	}
	long := slotLabel(node.Approval{Kind: "input", Summary: strings.Repeat("é", 100)})
	if !strings.HasSuffix(long, "…") || len([]rune(long)) != len("input: ")+70 {
		t.Fatalf("long label: %q (%d runes)", long, len([]rune(long)))
	}
}

func TestTrayIcons(t *testing.T) {
	for _, state := range []string{trayOK, trayWaiting, trayStopped, trayDown} {
		img, err := png.Decode(bytes.NewReader(trayPNG(state)))
		if err != nil || img.Bounds().Dx() != 64 {
			t.Fatalf("%s png: %v %v", state, err, img.Bounds())
		}
		_, _, _, a := img.At(0, 0).RGBA()
		if a != 0 {
			t.Fatalf("%s: the corner is transparent", state)
		}
		ico := trayICO(state)
		if !bytes.Equal(ico[:6], []byte{0, 0, 1, 0, 1, 0}) || ico[6] != 32 || ico[7] != 32 {
			t.Fatalf("%s ico header: % x", state, ico[:8])
		}
		if size := binary.LittleEndian.Uint32(ico[14:18]); int(size) != len(ico)-22 || size != 40+32*32*4+32*4 {
			t.Fatalf("%s ico image size %d of %d", state, size, len(ico))
		}
		if binary.LittleEndian.Uint32(ico[22:26]) != 40 {
			t.Fatalf("%s: BITMAPINFOHEADER", state)
		}
	}
	ok, _ := png.Decode(bytes.NewReader(trayPNG(trayOK)))
	waiting, _ := png.Decode(bytes.NewReader(trayPNG(trayWaiting)))
	r1, g1, b1, _ := ok.At(10, 32).RGBA()
	r2, g2, b2, _ := waiting.At(10, 32).RGBA()
	if r1 == r2 && g1 == g2 && b1 == b2 {
		t.Fatal("waiting and all-well icons look the same")
	}
}

func TestTrayDesktopEntry(t *testing.T) {
	e := trayDesktopEntry("/opt/Open Vibe/openvibe-node", "/home/a/node home")
	if !strings.Contains(e, `Exec="/opt/Open Vibe/openvibe-node" tray --home "/home/a/node home"`+"\n") || !strings.HasPrefix(e, "[Desktop Entry]\n") {
		t.Fatalf("entry:\n%s", e)
	}
	if e := trayDesktopEntry("/usr/local/bin/openvibe-node", ""); !strings.Contains(e, "Exec=/usr/local/bin/openvibe-node tray\n") {
		t.Fatalf("entry:\n%s", e)
	}
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if p, err := trayAutostartPath(); err != nil || p != filepath.Join(dir, "autostart", "openvibe-node-tray.desktop") {
		t.Fatalf("path %q %v", p, err)
	}
}

// fakeNode answers the control socket like a running Node with the computer plugin at level ask.
type fakeNode struct {
	mu       sync.Mutex
	list     []node.Approval
	refuse   int // approvals to refuse as "the agent is using the mouse or keyboard"
	answered []string
	tray     []bool
	stopped  bool
}

func (f *fakeNode) handle(r localctl.Request) localctl.Response {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Cmd {
	case localctl.CmdStatus:
		st := node.Status{Server: "https://openvibe.bot", Link: node.LinkStatus{Connected: true}, Latch: safety.LatchState{Local: f.stopped}}
		st.Telemetry.Sensors = map[string]any{"computer_level": "ask"}
		b, _ := json.Marshal(st)
		return localctl.Response{OK: true, Data: b}
	case localctl.CmdApprovals:
		b, _ := json.Marshal(f.list)
		return localctl.Response{OK: true, Data: b}
	case localctl.CmdTray:
		f.tray = append(f.tray, r.On)
		return localctl.Response{OK: true}
	case localctl.CmdApprove, localctl.CmdDeny:
		if r.Cmd == localctl.CmdApprove && f.refuse > 0 {
			f.refuse--
			return localctl.Response{Error: "the agent is using the mouse or keyboard (or just did): answer again in a moment"}
		}
		for i, a := range f.list {
			if a.ID == r.ID {
				f.list = append(f.list[:i], f.list[i+1:]...)
				f.answered = append(f.answered, r.Cmd+" "+r.ID)
				return localctl.Response{OK: true}
			}
		}
		return localctl.Response{Error: "nothing with the id waits"}
	case localctl.CmdStop:
		f.stopped = true
		return localctl.Response{OK: true}
	case localctl.CmdResume:
		f.stopped = false
		return localctl.Response{OK: true}
	}
	return localctl.Response{Error: "unknown"}
}

type fakeUI struct {
	state, tooltip, header, level string
	stopped, panel                bool
	slots                         []string
}

func (u *fakeUI) SetIcon(s string)              { u.state = s }
func (u *fakeUI) SetTooltip(s string)           { u.tooltip = s }
func (u *fakeUI) SetHeader(s string)            { u.header = s }
func (u *fakeUI) SetLevel(s string)             { u.level = s }
func (u *fakeUI) SetStopped(b bool)             { u.stopped = b }
func (u *fakeUI) SetSlots(l []string)           { u.slots = l }
func (u *fakeUI) SetPanel(b bool)               { u.panel = b }
func (n *fakeNotes) Answers() <-chan trayAnswer { return nil }
func (n *fakeNotes) Shut()                      { n.shut = true }

type fakeNotes struct {
	asked  []string // id, or "id again" for a second asking
	closed []string
	infos  []string
	shut   bool
}

func (n *fakeNotes) Ask(a node.Approval, again string) {
	if again != "" {
		n.asked = append(n.asked, a.ID+" again")
		return
	}
	n.asked = append(n.asked, a.ID)
}
func (n *fakeNotes) Close(id string)     { n.closed = append(n.closed, id) }
func (n *fakeNotes) Info(_, body string) { n.infos = append(n.infos, body) }

func TestTrayCoreAgainstANode(t *testing.T) {
	dir, err := os.MkdirTemp("", "ovt") // short: unix socket paths are limited to ~100 bytes
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "n.sock")
	f := &fakeNode{list: []node.Approval{{ID: "c1", Kind: "exec", Summary: "run echo hi", ExpiresS: 120}}, refuse: 1}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = localctl.Serve(ctx, sock, f.handle) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	ui, notes := &fakeUI{}, &fakeNotes{}
	core := newTrayCore(sock, ui, notes)

	core.tick()
	if ui.state != trayWaiting || ui.level != "ask" || !ui.panel || len(ui.slots) != 1 || ui.slots[0] != "exec: run echo hi" {
		t.Fatalf("ui after the first poll: %+v", ui)
	}
	if strings.Join(notes.asked, ",") != "c1" || len(f.tray) != 1 || !f.tray[0] {
		t.Fatalf("asked %v, tray %v", notes.asked, f.tray)
	}
	core.tick()
	if len(notes.asked) != 1 {
		t.Fatalf("asked again without a change: %v", notes.asked)
	}

	a, ok := core.slotID(0)
	if !ok || a.ID != "c1" {
		t.Fatalf("slot 0: %+v %v", a, ok)
	}
	if err := core.answer(a, true); err == nil {
		t.Fatal("the Node refused the approval; the tray should say so")
	}
	if strings.Join(notes.asked, ",") != "c1,c1 again" {
		t.Fatalf("a refused approval asks again: %v", notes.asked)
	}
	if err := core.answerByID("c1", true); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.answered, ",") != "approve c1" || strings.Join(notes.closed, ",") != "c1" {
		t.Fatalf("answered %v closed %v", f.answered, notes.closed)
	}
	core.tick()
	if ui.state != trayOK || len(ui.slots) != 0 || len(notes.closed) != 1 {
		t.Fatalf("after the answer: ui %+v closed %v", ui, notes.closed)
	}

	// A request answered elsewhere (the CLI, or it expired) takes its notification down at the next poll.
	f.mu.Lock()
	f.list = append(f.list, node.Approval{ID: "c2", Kind: "input", Summary: "click at 1, 2"})
	f.mu.Unlock()
	core.tick()
	f.mu.Lock()
	f.list = nil
	f.mu.Unlock()
	core.tick()
	if strings.Join(notes.asked, ",") != "c1,c1 again,c2" || strings.Join(notes.closed, ",") != "c1,c2" {
		t.Fatalf("asked %v closed %v", notes.asked, notes.closed)
	}
	if err := core.answerByID("c2", false); err == nil {
		t.Fatal("answered a request that no longer waits")
	}

	if err := core.stop(); err != nil {
		t.Fatal(err)
	}
	core.tick()
	if ui.state != trayStopped || !ui.stopped {
		t.Fatalf("stopped: %+v", ui)
	}
	if err := core.resume(); err != nil {
		t.Fatal(err)
	}
	core.tick()
	if ui.state != trayOK || ui.stopped {
		t.Fatalf("resumed: %+v", ui)
	}

	core.quit()
	if last := f.tray[len(f.tray)-1]; last || !notes.shut {
		t.Fatalf("quit tells the Node and shuts the notifier: tray %v shut %v", f.tray, notes.shut)
	}
	cancel()
	time.Sleep(50 * time.Millisecond)
	core.tick()
	if ui.state != trayDown {
		t.Fatalf("the Node gone: %+v", ui)
	}
}
