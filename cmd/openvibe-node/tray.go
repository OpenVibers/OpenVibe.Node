package main

// `openvibe-node tray`: the Node in its owner's notification area. It runs in the graphical session (not in the
// service), talks to the running Node over the local control socket like the other commands, and shows:
//
//   - the state in the icon: blue when all is well, amber while a computer request waits for an answer, red while a
//     stop is latched, grey when the Node does not answer;
//   - what waits for approval (the computer plugin at level "ask"), each with Approve and Deny;
//   - Stop everything (the local kill switch), Resume, and the machines page on openvibe.bot.
//
// A new request also raises a desktop notification with Approve and Deny buttons where the desktop offers them
// (Linux, org.freedesktop.Notifications). While the tray runs, the computer plugin's own prompts stay quiet. The core
// refuses an Approve while the agent drives the mouse or keyboard and just after (node.InputGuard), so an approved
// click cannot press Approve here; the tray then asks again.

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/OpenVibers/OpenVibe.Node/internal/localctl"
	"github.com/OpenVibers/OpenVibe.Node/internal/node"
)

// trayPoll is how often the tray asks the Node what changed.
const trayPoll = 1500 * time.Millisecond

// traySlots is how many waiting requests the menu lists (the plugin lets 8 wait at once).
const traySlots = 8

// Tray states, which pick the icon.
const (
	trayOK      = "ok"
	trayWaiting = "waiting"
	trayStopped = "stopped"
	trayDown    = "down"
)

// trayView is what the tray shows for one poll.
type trayView struct {
	State   string
	Tooltip string
	Header  string // the first, disabled menu line
	Level   string // the computer plugin's level, "" without it
	Stopped bool
	Waiting []node.Approval
	Panel   string // the machines page on the Bot this Node is paired with, "" if unknown
}

// viewOf builds the view from the Node's status and approvals; err is the socket error (the Node is not running).
func viewOf(st *node.Status, list []node.Approval, err error) trayView {
	if err != nil || st == nil {
		return trayView{State: trayDown, Tooltip: "OpenVibe Node: not running", Header: "The Node is not running"}
	}
	v := trayView{Waiting: list, Panel: panelURL(st.Server)}
	if lv, ok := st.Telemetry.Sensors["computer_level"].(string); ok {
		v.Level = lv
	}
	v.Stopped = st.Latch.Local || st.Latch.Remote
	link := "offline"
	if st.Link.Connected {
		link = "online"
	}
	switch {
	case v.Stopped:
		v.State = trayStopped
		v.Header = "Stopped: observe only"
	case len(list) > 0:
		v.State = trayWaiting
		v.Header = fmt.Sprintf("%d %s your answer", len(list), plural(len(list), "request waits for", "requests wait for"))
	default:
		v.State = trayOK
		v.Header = "OpenVibe Node: " + link
	}
	tip := []string{"OpenVibe Node: " + link}
	if v.Level != "" {
		tip = append(tip, "computer level "+v.Level)
	}
	if v.Stopped {
		tip = append(tip, "stopped")
	}
	if len(list) > 0 {
		tip = append(tip, fmt.Sprintf("%d waiting", len(list)))
	}
	v.Tooltip = strings.Join(tip, ", ")
	return v
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// panelURL is the machines page of the Bot the Node is paired with: an https origin, or a loopback http one.
func panelURL(server string) string {
	u, err := url.Parse(strings.TrimSpace(server))
	if err != nil || u.Host == "" {
		return ""
	}
	host := u.Hostname()
	if u.Scheme != "https" && !(u.Scheme == "http" && (host == "127.0.0.1" || host == "localhost" || host == "::1")) {
		return ""
	}
	return u.Scheme + "://" + u.Host + "/machines"
}

// slotLabel is one waiting request in the menu: its kind and what it would do, cut to fit a menu.
func slotLabel(a node.Approval) string {
	s := strings.Join(strings.Fields(a.Summary), " ")
	if len([]rune(s)) > 70 {
		s = string([]rune(s)[:69]) + "…"
	}
	if a.Kind != "" {
		return a.Kind + ": " + s
	}
	return s
}

// trayUI is the notification-area side (systray on Linux and Windows; a fake in tests).
type trayUI interface {
	SetIcon(state string)
	SetTooltip(string)
	SetHeader(string)
	SetLevel(string)
	SetStopped(bool)
	SetSlots([]string) // one label per waiting request, at most traySlots; the rest hidden
	SetPanel(bool)
}

// notifier raises and takes down desktop notifications (D-Bus on Linux; nothing elsewhere).
type notifier interface {
	// Ask shows a request with Approve and Deny; again marks a second asking after a refused answer.
	Ask(a node.Approval, again string)
	Close(id string)
	Info(title, body string)
	// Answers carries {id, approved} when a notification's button is pressed; nil if there are none.
	Answers() <-chan trayAnswer
	Shut()
}

type trayAnswer struct {
	ID       string
	Approved bool
}

type nopNotifier struct{}

func (nopNotifier) Ask(node.Approval, string)  {}
func (nopNotifier) Close(string)               {}
func (nopNotifier) Info(string, string)        {}
func (nopNotifier) Answers() <-chan trayAnswer { return nil }
func (nopNotifier) Shut()                      {}

// trayCore polls the Node and drives the UI and the notifications; the platform files only wire it up.
type trayCore struct {
	sock  string
	ui    trayUI
	note  notifier
	mu    sync.Mutex
	view  trayView
	shown map[string]bool // approval ids with a notification up
}

func newTrayCore(sock string, ui trayUI, note notifier) *trayCore {
	if note == nil {
		note = nopNotifier{}
	}
	return &trayCore{sock: sock, ui: ui, note: note, shown: map[string]bool{}}
}

func (t *trayCore) call(req localctl.Request) (localctl.Response, error) {
	return localctl.Call(t.sock, req, 2*time.Second)
}

// tick asks the Node for its status and approvals, says the tray is there, and updates everything.
func (t *trayCore) tick() trayView {
	var st *node.Status
	var list []node.Approval
	resp, err := t.call(localctl.Request{Cmd: localctl.CmdStatus})
	if err == nil {
		var s node.Status
		if err = json.Unmarshal(resp.Data, &s); err == nil {
			st = &s
		}
	}
	if err == nil {
		if resp, aerr := t.call(localctl.Request{Cmd: localctl.CmdApprovals}); aerr == nil {
			_ = json.Unmarshal(resp.Data, &list)
		}
		_, _ = t.call(localctl.Request{Cmd: localctl.CmdTray, On: true})
	}
	v := viewOf(st, list, err)
	t.mu.Lock()
	t.view = v
	fresh, gone := []node.Approval{}, []string{}
	now := map[string]bool{}
	for _, a := range list {
		now[a.ID] = true
		if !t.shown[a.ID] {
			fresh = append(fresh, a)
			t.shown[a.ID] = true
		}
	}
	for id := range t.shown {
		if !now[id] {
			gone = append(gone, id)
			delete(t.shown, id)
		}
	}
	t.mu.Unlock()
	for _, id := range gone {
		t.note.Close(id)
	}
	for _, a := range fresh {
		t.note.Ask(a, "")
	}
	labels := make([]string, 0, traySlots)
	for i, a := range v.Waiting {
		if i == traySlots {
			break
		}
		labels = append(labels, slotLabel(a))
	}
	t.ui.SetIcon(v.State)
	t.ui.SetTooltip(v.Tooltip)
	t.ui.SetHeader(v.Header)
	t.ui.SetLevel(v.Level)
	t.ui.SetStopped(v.Stopped)
	t.ui.SetSlots(labels)
	t.ui.SetPanel(v.Panel != "")
	return v
}

// slotID is the approval id the menu's slot i shows now ("" if none).
func (t *trayCore) slotID(i int) (node.Approval, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if i < 0 || i >= len(t.view.Waiting) {
		return node.Approval{}, false
	}
	return t.view.Waiting[i], true
}

// answer approves or denies one request; a refusal (the agent just used the mouse or keyboard) asks again.
func (t *trayCore) answer(a node.Approval, approved bool) error {
	cmd := localctl.CmdDeny
	if approved {
		cmd = localctl.CmdApprove
	}
	_, err := t.call(localctl.Request{Cmd: cmd, ID: a.ID})
	if err != nil {
		if approved && strings.Contains(err.Error(), "mouse or keyboard") {
			t.note.Ask(a, "Not taken: the agent was using the mouse or keyboard. Answer again if it was you.")
		} else {
			t.note.Info("OpenVibe Node", err.Error())
		}
		return err
	}
	t.mu.Lock()
	delete(t.shown, a.ID)
	t.mu.Unlock()
	t.note.Close(a.ID)
	return nil
}

// answerByID is a notification's button: the request must still wait.
func (t *trayCore) answerByID(id string, approved bool) error {
	t.mu.Lock()
	var found *node.Approval
	for i := range t.view.Waiting {
		if t.view.Waiting[i].ID == id {
			a := t.view.Waiting[i]
			found = &a
		}
	}
	t.mu.Unlock()
	if found == nil {
		return errors.New("it no longer waits")
	}
	return t.answer(*found, approved)
}

func (t *trayCore) stop() error {
	_, err := t.call(localctl.Request{Cmd: localctl.CmdStop})
	return err
}

func (t *trayCore) resume() error {
	_, err := t.call(localctl.Request{Cmd: localctl.CmdResume})
	return err
}

// quit tells the Node the tray is gone (the plugin prompts again) and takes the notifications down.
func (t *trayCore) quit() {
	_, _ = t.call(localctl.Request{Cmd: localctl.CmdTray, On: false})
	t.mu.Lock()
	ids := make([]string, 0, len(t.shown))
	for id := range t.shown {
		ids = append(ids, id)
	}
	t.shown = map[string]bool{}
	t.mu.Unlock()
	for _, id := range ids {
		t.note.Close(id)
	}
	t.note.Shut()
}

func (t *trayCore) panel() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.view.Panel
}

// cmdTray runs the tray, or with --autostart on|off adds it to (or removes it from) the session's start.
func cmdTray(g *globals, autostart string, stdout io.Writer) error {
	switch autostart {
	case "":
		return runTray(g)
	case "on", "off":
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		return trayAutostart(autostart == "on", exe, g.home, stdout)
	}
	return fmt.Errorf("--autostart is on or off")
}

// trayDesktopEntry is the XDG autostart entry that starts the tray when its owner signs in (Linux).
func trayDesktopEntry(exe, home string) string {
	line := shellQuote(exe) + " tray"
	if home != "" {
		line += " --home " + shellQuote(home)
	}
	return "[Desktop Entry]\nType=Application\nName=OpenVibe Node\nComment=Approve what your agents ask to do on this computer\n" +
		"Exec=" + line + "\nIcon=dialog-question\nTerminal=false\nX-GNOME-Autostart-enabled=true\nNoDisplay=false\n"
}

// shellQuote quotes one Exec argument the way the desktop entry spec reads them.
func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n\"'\\$`") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "`", "\\`", `$`, `\$`)
	return `"` + r.Replace(s) + `"`
}

// trayAutostartPath is where the Linux autostart entry goes: $XDG_CONFIG_HOME/autostart, else ~/.config/autostart.
func trayAutostartPath() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "autostart", "openvibe-node-tray.desktop"), nil
}

// ---- icons ---------------------------------------------------------------------------------------------------------

var trayColors = map[string]color.NRGBA{
	trayOK:      {0x3b, 0x82, 0xf6, 0xff}, // the OpenVibe blue
	trayWaiting: {0xf5, 0x9e, 0x0b, 0xff},
	trayStopped: {0xdc, 0x26, 0x26, 0xff},
	trayDown:    {0x9c, 0xa3, 0xaf, 0xff},
}

// trayImage draws the tray icon: a filled disc in the state's colour with a white ring-and-dot (the Node's mark);
// while something waits the dot becomes a bar, an exclamation.
func trayImage(state string, size int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	c, ok := trayColors[state]
	if !ok {
		c = trayColors[trayDown]
	}
	white := color.NRGBA{0xff, 0xff, 0xff, 0xff}
	r := float64(size) / 2
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx, dy := float64(x)+0.5-r, float64(y)+0.5-r
			d := math.Hypot(dx, dy)
			a := clamp01(r - d) // anti-aliased edge of the disc
			if a <= 0 {
				continue
			}
			px := c
			ring := math.Abs(d - r*0.55)
			if state == trayWaiting {
				// an exclamation mark: a bar and a dot
				if (math.Abs(dx) < r*0.13 && dy > -r*0.55 && dy < r*0.15) || math.Hypot(dx, dy-r*0.42) < r*0.14 {
					px = white
				}
			} else if ring < r*0.1 || d < r*0.18 {
				px = white
			}
			px.A = uint8(float64(px.A) * a)
			img.SetNRGBA(x, y, px)
		}
	}
	return img
}

func clamp01(v float64) float64 { return math.Max(0, math.Min(1, v)) }

// trayPNG is the icon for the StatusNotifierItem (Linux), which takes PNG bytes.
func trayPNG(state string) []byte {
	var b bytes.Buffer
	_ = png.Encode(&b, trayImage(state, 64))
	return b.Bytes()
}

// trayICO is the icon for Windows: a 32×32 32-bit ICO (BMP image with its AND mask), which LoadImage reads.
func trayICO(state string) []byte {
	const size = 32
	img := trayImage(state, size)
	var dib bytes.Buffer
	hdr := struct {
		Size                         uint32
		Width, Height                int32
		Planes, BitCount             uint16
		Compression, SizeImage       uint32
		XPels, YPels, Used, Imported int32
	}{40, size, size * 2, 1, 32, 0, size * size * 4, 0, 0, 0, 0}
	_ = binary.Write(&dib, binary.LittleEndian, hdr)
	for y := size - 1; y >= 0; y-- { // bottom-up rows, BGRA
		for x := 0; x < size; x++ {
			p := img.NRGBAAt(x, y)
			dib.Write([]byte{p.B, p.G, p.R, p.A})
		}
	}
	dib.Write(make([]byte, size*4)) // the AND mask: 32 bits a row, all zero (alpha decides)
	var out bytes.Buffer
	_ = binary.Write(&out, binary.LittleEndian, [3]uint16{0, 1, 1}) // ICONDIR: reserved, type icon, one image
	entry := struct {
		W, H, Colors, Reserved uint8
		Planes, BitCount       uint16
		Bytes, Offset          uint32
	}{size, size, 0, 0, 1, 32, uint32(dib.Len()), 6 + 16}
	_ = binary.Write(&out, binary.LittleEndian, entry)
	out.Write(dib.Bytes())
	return out.Bytes()
}
