package node

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"sync"
	"time"

	"github.com/OpenVibers/OpenVibe.Node/internal/credentials"
	"github.com/OpenVibers/OpenVibe.Node/internal/mono"
	"github.com/OpenVibers/OpenVibe.Node/internal/plugins"
	"github.com/OpenVibers/OpenVibe.Node/internal/protocol"
	"github.com/OpenVibers/OpenVibe.Node/internal/safety"
	"github.com/OpenVibers/OpenVibe.Node/internal/video"
)

// The screen stream (screen_stream, contracts 0.182.0): the machine's screen shown live to its owner. Bot mints the
// WHIP URL and publish key of the machine's own private OpenRestream stream and sends them in the start command; the
// computer plugin captures the frames (the same tools as `screen`, at the owner's level) and emits them as video
// with stream "screen"; the core encodes and publishes them on a second WHIP session, apart from a robot's camera.
//
// A stream ends when its time is up, on stop, when the plugin says it ended, and at once when the local kill switch
// or an e-stop latches; a start is refused while either is latched. One stream at a time: a new start replaces it.
const (
	ScreenDefaultSeconds = 600
	ScreenMinSeconds     = 30
	ScreenMaxSeconds     = 1800
	ScreenDefaultFPS     = 2
	ScreenMaxFPS         = 5
	// The built-in encoder's picture size when ffmpeg is missing (ffmpeg keeps the frames' own size).
	screenFallbackW, screenFallbackH = 960, 544
)

type screenStream struct {
	pub    *video.Publisher
	src    *video.JPEGSource
	from   string // the plugin whose frames feed it
	sid    string // the plugin's id for it: its "ended" event names it
	until  time.Time
	cancel context.CancelFunc
	done   chan struct{}
}

type screenState struct {
	mu  sync.Mutex
	cur *screenStream
}

// ScreenStatus is the screen stream in `openvibe-node status`.
type ScreenStatus struct {
	State     string `json:"state"`
	Until     string `json:"until,omitempty"`
	Frames    int64  `json:"frames,omitempty"`
	LastError string `json:"last_error,omitempty"`
}

type screenValue struct {
	Action     string `json:"action"`
	WHIPURL    string `json:"whip_url"`
	PublishKey string `json:"publish_key"`
	Seconds    *int   `json:"seconds"`
	FPS        *int   `json:"fps"`
}

// screenCommand answers screen_stream: the plugin is asked first (it applies the owner's level and has the capture
// tool), and only once it said yes does the core publish.
func (n *Node) screenCommand(c protocol.Command, entry *safety.Entry, received mono.Instant, nack func(code, msg string)) {
	var v screenValue
	if err := json.Unmarshal(c.Value, &v); err != nil {
		nack(protocol.FaultBadValue, "value must be {action, …}")
		return
	}
	p := n.mgr.Route(protocol.KindScreenStream, c.Target, "")
	switch v.Action {
	case "stop":
		stopped := n.stopScreen("stopped")
		if p != nil {
			// Best effort: the plugin stops capturing (its answer does not change the core's).
			wait := p.Begin(n.ctx, c.ID+":stop", protocol.KindScreenStream, json.RawMessage(`{"action":"stop"}`), 5000)
			go wait()
		}
		data, _ := json.Marshal(map[string]any{"streaming": false, "stopped": stopped})
		r := protocol.AckData{ID: c.ID, LatencyMS: mono.Since(received).Milliseconds(), Data: data}
		n.dedup.Complete(entry, r)
		n.send(r)
		return
	case "start":
	default:
		nack(protocol.FaultBadValue, "action is start or stop")
		return
	}
	st := n.latch.State()
	if st.Local {
		nack(protocol.FaultLocalStop, "stopped on the machine with `openvibe-node stop`")
		return
	}
	if st.Remote {
		nack(protocol.FaultEstopped, st.RemoteReason)
		return
	}
	if err := n.checkScreenURL(v.WHIPURL); err != nil {
		nack(protocol.FaultBadValue, err.Error())
		return
	}
	if v.PublishKey == "" || len(v.PublishKey) > 256 {
		nack(protocol.FaultBadValue, "publish_key is required")
		return
	}
	seconds, fps := ScreenDefaultSeconds, ScreenDefaultFPS
	if v.Seconds != nil {
		seconds = min(max(*v.Seconds, ScreenMinSeconds), ScreenMaxSeconds)
	}
	if v.FPS != nil {
		fps = min(max(*v.FPS, 1), ScreenMaxFPS)
	}
	if p == nil {
		nack(protocol.FaultUnsupported, "no driver on this device captures the screen (the computer plugin is off)")
		return
	}
	// The plugin never sees the URL or the key.
	toPlugin, _ := json.Marshal(map[string]any{"action": "start", "seconds": seconds, "fps": fps})
	wait := p.Begin(n.ctx, c.ID, protocol.KindScreenStream, toPlugin, 15000)
	key := credentials.NewSecret(v.PublishKey)
	go func() {
		r := wait()
		var msg protocol.Message
		if r.OK {
			var got struct {
				SID string `json:"sid"`
			}
			_ = json.Unmarshal(r.Data, &got)
			until := n.startScreen(v.WHIPURL, key, seconds, fps, p.Name(), got.SID)
			data, _ := json.Marshal(map[string]any{"streaming": true, "seconds": seconds, "fps": fps, "until_ms": until.UnixMilli()})
			msg = protocol.AckData{ID: c.ID, LatencyMS: mono.Since(received).Milliseconds(), Data: data}
		} else {
			msg = protocol.Nack{ID: c.ID, FaultCode: r.FaultCode, Message: r.Message}
		}
		n.dedup.Complete(entry, msg)
		n.send(msg)
	}()
}

// checkScreenURL accepts an https WHIP endpoint with no credentials in it (http only to loopback, for tests).
func (n *Node) checkScreenURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || len(raw) > 2048 {
		return fmt.Errorf("whip_url must be an https URL")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if ip := net.ParseIP(u.Hostname()); n.opt.VideoLoopback && ip != nil && ip.IsLoopback() {
			return nil
		}
	}
	return fmt.Errorf("whip_url must be an https URL")
}

// startScreen publishes the plugin's screen frames until the time is up (replacing a stream that runs) → the end.
func (n *Node) startScreen(whip string, key credentials.Secret, seconds, fps int, from, sid string) time.Time {
	n.stopScreen("replaced")
	ctx, cancel := context.WithCancel(n.ctx)
	until := time.Now().Add(time.Duration(seconds) * time.Second)
	src := &video.JPEGSource{FFmpeg: n.opt.Config.Video.FFmpeg, FPS: fps, W: screenFallbackW, H: screenFallbackH, Log: n.log}
	pub := video.NewPublisher(video.Options{WHIPURL: whip, PublishKey: key, ICEServers: n.screenICE(), Log: n.log, IncludeLoopback: n.opt.VideoLoopback})
	s := &screenStream{pub: pub, src: src, from: from, sid: sid, until: until, cancel: cancel, done: make(chan struct{})}
	n.screen.mu.Lock()
	n.screen.cur = s
	n.screen.mu.Unlock()
	n.log.Info("screen stream started", "seconds", seconds, "fps", fps)
	go func() {
		defer close(s.done)
		pub.Run(ctx, src)
	}()
	go func() {
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				st := n.latch.State()
				switch {
				case st.Local || st.Remote:
					n.endScreen(s, "stopped on the machine")
					return
				case now.After(until.Add(3 * time.Second)):
					n.endScreen(s, "time is up")
					return
				}
			}
		}
	}()
	return until
}

func (n *Node) screenICE() []protocol.ICEServer {
	if n.opt.Creds == nil {
		return nil
	}
	return n.opt.Creds.ICEServers
}

// endScreen ends s if it is still the running stream.
func (n *Node) endScreen(s *screenStream, why string) {
	n.screen.mu.Lock()
	if n.screen.cur != s {
		n.screen.mu.Unlock()
		return
	}
	n.screen.cur = nil
	n.screen.mu.Unlock()
	s.cancel()
	n.log.Info("screen stream ended", "why", why)
}

// stopScreen ends the running stream, if any → whether one ran.
func (n *Node) stopScreen(why string) bool {
	n.screen.mu.Lock()
	s := n.screen.cur
	n.screen.mu.Unlock()
	if s == nil {
		return false
	}
	n.endScreen(s, why)
	return true
}

// screenEnded ends the stream the plugin says it ended (by its id), never a newer one.
func (n *Node) screenEnded(plugin, sid string) {
	n.screen.mu.Lock()
	s := n.screen.cur
	n.screen.mu.Unlock()
	if s != nil && s.from == plugin && s.sid != "" && s.sid == sid {
		n.endScreen(s, "the plugin ended it")
	}
}

// screenFrame passes a plugin's screen frame to the running stream (frames from any other plugin are dropped).
func (n *Node) screenFrame(o plugins.Output) {
	n.screen.mu.Lock()
	s := n.screen.cur
	n.screen.mu.Unlock()
	if s == nil || s.from != o.Plugin {
		return
	}
	if p, ok := o.Data["path"].(string); ok {
		s.src.Push(p)
	}
}

func (n *Node) screenStatus() ScreenStatus {
	n.screen.mu.Lock()
	s := n.screen.cur
	n.screen.mu.Unlock()
	if s == nil {
		return ScreenStatus{State: video.StateOff}
	}
	return ScreenStatus{State: s.pub.State(), Until: s.until.UTC().Format(time.RFC3339), Frames: s.pub.Frames(), LastError: s.pub.LastError()}
}
