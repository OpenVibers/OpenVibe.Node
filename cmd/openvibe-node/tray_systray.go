//go:build linux || windows

package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"fyne.io/systray"
)

// The notification area: a StatusNotifierItem over D-Bus on Linux (KDE, XFCE, Cinnamon, MATE, GNOME with the
// AppIndicator extension) and the notification icon on Windows; both without cgo.

type systrayUI struct {
	mu                     sync.Mutex
	header, level          *systray.MenuItem
	stop, resume, panel    *systray.MenuItem
	quit                   *systray.MenuItem
	slots                  [traySlots]*systray.MenuItem
	approve, deny          [traySlots]*systray.MenuItem
	state, tooltip, headTx string
	levelTx                string
	labels                 []string
	stopped, hasPanel      *bool
}

func (u *systrayUI) build() {
	u.header = systray.AddMenuItem("OpenVibe Node", "")
	u.header.Disable()
	u.level = systray.AddMenuItem("", "")
	u.level.Disable()
	u.level.Hide()
	systray.AddSeparator()
	for i := 0; i < traySlots; i++ {
		u.slots[i] = systray.AddMenuItem("", "Approve or deny what your agent asks")
		u.approve[i] = u.slots[i].AddSubMenuItem("Approve", "Let it run")
		u.deny[i] = u.slots[i].AddSubMenuItem("Deny", "Refuse it")
		u.slots[i].Hide()
	}
	systray.AddSeparator()
	u.stop = systray.AddMenuItem("Stop everything", "The kill switch: stop every actuator, observe only, until you resume")
	u.resume = systray.AddMenuItem("Resume", "Release the stop")
	u.panel = systray.AddMenuItem("Open my machines page", "This machine on openvibe.bot")
	u.panel.Hide()
	systray.AddSeparator()
	u.quit = systray.AddMenuItem("Quit the tray", "The Node keeps running; requests wait for `openvibe-node approve`")
}

func (u *systrayUI) SetIcon(state string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if state == u.state {
		return
	}
	u.state = state
	if runtime.GOOS == "windows" {
		systray.SetIcon(trayICO(state))
	} else {
		systray.SetIcon(trayPNG(state))
	}
}

func (u *systrayUI) SetTooltip(s string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if s != u.tooltip {
		u.tooltip = s
		systray.SetTooltip(s)
	}
}

func (u *systrayUI) SetHeader(s string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if s != u.headTx {
		u.headTx = s
		u.header.SetTitle(s)
	}
}

func (u *systrayUI) SetLevel(lv string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if lv == u.levelTx {
		return
	}
	u.levelTx = lv
	if lv == "" {
		u.level.Hide()
		return
	}
	u.level.SetTitle("Computer level: " + lv + " (set on this machine)")
	u.level.Show()
}

func (u *systrayUI) SetStopped(stopped bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.stopped != nil && *u.stopped == stopped {
		return
	}
	u.stopped = &stopped
	if stopped {
		u.stop.Disable()
		u.resume.Enable()
	} else {
		u.stop.Enable()
		u.resume.Disable()
	}
}

func (u *systrayUI) SetSlots(labels []string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if strings.Join(labels, "\x00") == strings.Join(u.labels, "\x00") && u.labels != nil {
		return
	}
	u.labels = append([]string{}, labels...)
	for i := 0; i < traySlots; i++ {
		if i < len(labels) {
			u.slots[i].SetTitle(labels[i])
			u.slots[i].Show()
		} else {
			u.slots[i].Hide()
		}
	}
}

func (u *systrayUI) SetPanel(has bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.hasPanel != nil && *u.hasPanel == has {
		return
	}
	u.hasPanel = &has
	if has {
		u.panel.Show()
	} else {
		u.panel.Hide()
	}
}

func runTray(g *globals) error {
	if runtime.GOOS == "linux" && os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" && os.Getenv("XDG_RUNTIME_DIR") == "" {
		return fmt.Errorf("the tray runs in your desktop session: start it there (openvibe-node tray --autostart on starts it when you sign in)")
	}
	ui := &systrayUI{}
	core := newTrayCore(g.paths.SocketPath(), ui, newDesktopNotifier(g.log))
	stopSig := make(chan os.Signal, 1)
	signal.Notify(stopSig, os.Interrupt, syscall.SIGTERM)
	systray.Run(func() {
		systray.SetTitle("OpenVibe Node")
		ui.build()
		core.tick()
		go trayLoop(core, ui, stopSig, g)
	}, core.quit)
	return nil
}

func trayLoop(core *trayCore, ui *systrayUI, stopSig <-chan os.Signal, g *globals) {
	for i := 0; i < traySlots; i++ {
		i := i
		go func() {
			for range ui.approve[i].ClickedCh {
				if a, ok := core.slotID(i); ok {
					_ = core.answer(a, true)
					core.tick()
				}
			}
		}()
		go func() {
			for range ui.deny[i].ClickedCh {
				if a, ok := core.slotID(i); ok {
					_ = core.answer(a, false)
					core.tick()
				}
			}
		}()
	}
	t := time.NewTicker(trayPoll)
	defer t.Stop()
	answers := core.note.Answers()
	for {
		select {
		case <-t.C:
			core.tick()
		case a := <-answers:
			if err := core.answerByID(a.ID, a.Approved); err != nil {
				g.log.Info("tray answer", "id", a.ID, "err", err)
			}
			core.tick()
		case <-ui.stop.ClickedCh:
			if err := core.stop(); err != nil {
				core.note.Info("OpenVibe Node", "Could not stop: "+err.Error())
			}
			core.tick()
		case <-ui.resume.ClickedCh:
			if err := core.resume(); err != nil {
				core.note.Info("OpenVibe Node", "Could not resume: "+err.Error())
			}
			core.tick()
		case <-ui.panel.ClickedCh:
			if u := core.panel(); u != "" {
				openURL(u)
			}
		case <-ui.quit.ClickedCh:
			systray.Quit()
			return
		case <-stopSig:
			systray.Quit()
			return
		}
	}
}

// openURL opens the machines page in the default browser.
func openURL(u string) {
	var c *exec.Cmd
	if runtime.GOOS == "windows" {
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	} else {
		c = exec.Command("xdg-open", u)
	}
	_ = c.Start()
}

// trayAutostart starts the tray when its owner signs in: an XDG autostart entry on Linux, a Run value on Windows.
func trayAutostart(on bool, exe, home string, stdout io.Writer) error {
	if runtime.GOOS == "windows" {
		line := quoteArg(exe) + " tray"
		if home != "" {
			line += " --home " + quoteArg(home)
		}
		var out []byte
		var err error
		if on {
			out, err = exec.Command("reg", "add", runKey, "/v", "OpenVibe Node tray", "/t", "REG_SZ", "/d", line, "/f").CombinedOutput()
		} else {
			out, err = exec.Command("reg", "delete", runKey, "/v", "OpenVibe Node tray", "/f").CombinedOutput()
			if err != nil && strings.Contains(strings.ToLower(string(out)), "unable to find") {
				err = nil
			}
		}
		if err != nil {
			return fmt.Errorf("reg: %v: %s", err, strings.TrimSpace(string(out)))
		}
		if on {
			fmt.Fprintln(stdout, "The tray now starts when you sign in:", line)
		} else {
			fmt.Fprintln(stdout, "The tray no longer starts when you sign in.")
		}
		return nil
	}
	path, err := trayAutostartPath()
	if err != nil {
		return err
	}
	if !on {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		fmt.Fprintln(stdout, "The tray no longer starts when you sign in (removed", path+")")
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(trayDesktopEntry(exe, home)), 0o644); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "The tray now starts when you sign in:", path)
	return nil
}
