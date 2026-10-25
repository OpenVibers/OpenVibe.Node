//go:build !windows

package node

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/OpenVibers/OpenVibe.Node/internal/config"
	"github.com/OpenVibers/OpenVibe.Node/internal/fakebot"
	"github.com/OpenVibers/OpenVibe.Node/internal/link"
	"github.com/OpenVibers/OpenVibe.Node/internal/plugins"
	"github.com/OpenVibers/OpenVibe.Node/internal/protocol"
)

// TestVoiceEndToEnd runs the real voice plugin (its file engine stands in for the microphone) under a Node paired with
// the fake Bot: nothing goes to Bot until the owner's config turns voice on; then only what follows the call word goes,
// as a voice_ask frame (never as a telemetry event), and Bot's answer reaches the plugin, which speaks it.
func TestVoiceEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("end-to-end tests run on unix")
	}
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	root, _ := filepath.Abs("../..")
	home := t.TempDir()
	heard := filepath.Join(home, "heard.txt")
	said := filepath.Join(home, "said.txt")
	paths := config.HomePaths(home)
	srv := fakebot.New()
	t.Cleanup(srv.Close)
	srv.Config = protocol.Config{HeartbeatMS: 100, Limits: protocol.Limits{MaxCommandMS: 1000}, AllowedCommands: []string{protocol.KindHalt}}
	srv.AddCode("VOICE234")
	creds, err := link.Pair(context.Background(), nil, srv.URL(), protocol.PairRequest{Code: "VOICE234", AgentVersion: "test", DeviceKind: "onboard", Drivers: []string{"voice"}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Python = py
	cfg.Plugins = []config.PluginConfig{{Name: "voice", Config: map[string]any{"engine": "file", "heard_file": heard, "notify": false,
		"speak": []string{"sh", "-c", `cat >> "$0"`, said}},
		Env: map[string]string{"PYTHONPATH": filepath.Join(root, "plugins/sdk") + string(os.PathListSeparator) + filepath.Join(root, "plugins/voice")}}}
	cfg.Video = config.VideoConfig{Source: "off"}
	n, err := New(Options{Config: cfg, Paths: paths, Creds: creds, Version: "test", HeartbeatInterval: 100 * time.Millisecond,
		LinkBackoffMin: 50 * time.Millisecond, LatchPoll: 50 * time.Millisecond, Log: testLog()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { n.Run(ctx); close(done) }()
	e := &env{t: t, srv: srv, node: n, paths: paths, cancel: cancel, done: done}
	t.Cleanup(e.stop)
	e.conn = e.nextConn()
	if _, err := e.conn.Expect(protocol.TypeStatus, 20*time.Second, func(m protocol.Message) bool {
		s := m.(protocol.Status)
		return len(s.Drivers) == 1 && s.Drivers[0].State == plugins.StateReady
	}); err != nil {
		t.Fatal(err)
	}
	appendLine := func(s string) {
		f, err := os.OpenFile(heard, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString(s + "\n")
		f.Close()
	}

	// Voice off (the first config did not say voice): the plugin is not listening, nothing reaches Bot.
	appendLine("hey vibe are you there")
	if _, err := e.conn.Expect(protocol.TypeVoiceAsk, 700*time.Millisecond, nil); err == nil {
		t.Fatal("a voice_ask while voice is off")
	}

	// The owner turns voice on: the core passes it to the plugin, which listens for the call word.
	on := srv.Config
	on.Voice, on.CallWord = true, "hey desk"
	if err := e.conn.Send(on); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	appendLine("so anyway the meeting ran long")
	appendLine("Hey desk, what is on my calendar this afternoon?")
	f, err := e.conn.Expect(protocol.TypeVoiceAsk, 10*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	ask := f.Msg.(protocol.VoiceAsk)
	if ask.Text != "what is on my calendar this afternoon" || !strings.HasPrefix(ask.ID, "va-") {
		t.Fatalf("voice_ask %+v", ask)
	}
	for _, fr := range e.conn.Frames() {
		if tel, ok := fr.Msg.(protocol.Telemetry); ok {
			for _, ev := range tel.Events {
				if ev.Name == "voice_ask" {
					t.Fatal("the question went out as a telemetry event")
				}
			}
		}
		if fr.Type == protocol.TypeVoiceAsk && strings.Contains(string(mustJSON(fr.Msg)), "meeting") {
			t.Fatal("what was not for Vibe left the machine")
		}
	}

	// Bot answers working, then with Vibe's answer: the plugin speaks the answer, not the working line.
	if err := e.conn.Send(protocol.VoiceAnswer{ID: ask.ID, State: "working", Text: "On it."}); err != nil {
		t.Fatal(err)
	}
	if err := e.conn.Send(protocol.VoiceAnswer{ID: ask.ID, State: "answered", Text: "A call at three, nothing after four.", URL: "https://openvibe.chat/vibe/v-0123456789ab"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, _ := os.ReadFile(said)
		if string(b) == "A call at three, nothing after four." {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("spoken %q", string(b))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
