// Package fleet reports what this machine's coding agents have left (Codes C9c).
//
// The reading is the OpenVibe.Codes runner's own probe — `openvibe-codes logins --json` — the same one a person sees
// on openvibe.codes/agents: each CLI, whether it is installed, whether it is signed in, and the limit windows its
// login file carries with when they free. Nothing else leaves the machine: no key, no token and no account name; the
// probe reads the owner's own files read-only and reports labels, numbers and times.
//
// The prober runs the CLI as the Node's own user (worker.plan_logins are files under that user's home, so the same home
// is where the logins are), caches the last answer, and the link sends it with the heartbeat. A probe that fails keeps
// the previous reading until the next one, and says so in the log; it never invents a state.
package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/OpenVibers/OpenVibe.Node/internal/protocol"
)

// Defaults for the prober: a minute between readings (a plan window moves in hours, not seconds) and a probe that
// may take this long before it is killed.
const (
	DefaultInterval = 60 * time.Second
	DefaultTimeout  = 20 * time.Second
	MaxAgents       = 16
	MaxWindows      = 10
)

var (
	agentID   = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)
	windowMax = 40
)

// Options configure a Prober.
type Options struct {
	Command  []string      // the probe: [node, /opt/openvibe-codes/…/openvibe-codes.js, logins, --json]
	Home     string        // HOME for the probe; empty means the Node user's home
	Interval time.Duration // default DefaultInterval
	Timeout  time.Duration // default DefaultTimeout
	Log      *slog.Logger
}

// Prober runs the Codes logins probe on a timer and keeps its last answer.
type Prober struct {
	opt Options
	log *slog.Logger

	mu   sync.Mutex
	last []protocol.FleetAgent
	at   time.Time
	// probed is false until the first probe finished, so a machine that has never read its logins sends no fleet
	// rather than an empty one (Bot keeps what it has; Codes does not conclude "nothing installed" from silence).
	probed bool
}

// New builds a Prober. A nil Command makes Latest return nothing: the machine has no Codes runner, so it has no
// coding agents to report.
func New(opt Options) *Prober {
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	if opt.Interval <= 0 {
		opt.Interval = DefaultInterval
	}
	if opt.Timeout <= 0 {
		opt.Timeout = DefaultTimeout
	}
	return &Prober{opt: opt, log: opt.Log.With("component", "fleet")}
}

// Latest is the last reading, for the link's heartbeat. It never blocks on a probe.
func (p *Prober) Latest() []protocol.FleetAgent {
	if p == nil || len(p.opt.Command) == 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.probed {
		return nil
	}
	out := make([]protocol.FleetAgent, len(p.last))
	for i, a := range p.last {
		out[i] = a
		if len(a.Windows) > 0 {
			out[i].Windows = append([]protocol.FleetWindow(nil), a.Windows...)
		}
	}
	return out
}

// At is when the last reading was taken (the zero time before the first one).
func (p *Prober) At() time.Time {
	if p == nil {
		return time.Time{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.at
}

// Run probes now and then every Interval until ctx ends.
func (p *Prober) Run(ctx context.Context) {
	if p == nil || len(p.opt.Command) == 0 {
		return
	}
	p.probe(ctx)
	t := time.NewTicker(p.opt.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.probe(ctx)
		}
	}
}

// probe runs the CLI once and replaces the reading on success.
func (p *Prober) probe(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, p.opt.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.opt.Command[0], p.opt.Command[1:]...)
	home := p.opt.Home
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	cmd.Env = append(os.Environ(), "HOME="+home)
	cmd.Stderr = nil
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		p.log.Warn("logins probe failed; the last reading stands", "err", err)
		return
	}
	agents, err := Parse(out.Bytes())
	if err != nil {
		p.log.Warn("logins probe answered something that is not a fleet; the last reading stands", "err", err)
		return
	}
	p.mu.Lock()
	p.last, p.at, p.probed = agents, time.Now(), true
	p.mu.Unlock()
}

// Parse reads the OpenVibe.Codes runner's `logins --json` answer into the fleet the heartbeat carries. Only the four
// things a person may see are kept, and everything is bounded: an agent id shaped like an id, at most MaxAgents
// agents, at most MaxWindows windows each, a short label and reset time, and two finite numbers.
func Parse(raw []byte) ([]protocol.FleetAgent, error) {
	var rows []struct {
		ID        string  `json:"id"`
		Installed bool    `json:"installed"`
		SignedIn  bool    `json:"signed_in"`
		Windows   []struct {
			Name     string   `json:"name"`
			Used     *float64 `json:"used"`
			Limit    *float64 `json:"limit"`
			ResetsAt string   `json:"resets_at"`
		} `json:"windows"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	out := make([]protocol.FleetAgent, 0, len(rows))
	seen := map[string]bool{}
	for _, r := range rows {
		if !agentID.MatchString(r.ID) || seen[r.ID] {
			continue
		}
		seen[r.ID] = true
		a := protocol.FleetAgent{Agent: r.ID, Installed: r.Installed, SignedIn: r.SignedIn}
		for _, w := range r.Windows {
			if len(a.Windows) >= MaxWindows {
				break
			}
			name := strings.TrimSpace(w.Name)
			if name == "" {
				name = "window"
			}
			if len(name) > windowMax {
				name = name[:windowMax]
			}
			resets := strings.TrimSpace(w.ResetsAt)
			if len(resets) > windowMax {
				resets = resets[:windowMax]
			}
			a.Windows = append(a.Windows, protocol.FleetWindow{Name: name, Used: finite(w.Used), Limit: finite(w.Limit), ResetsAt: resets})
		}
		out = append(out, a)
		if len(out) >= MaxAgents {
			break
		}
	}
	return out, nil
}

// finite drops a number that would not survive JSON (NaN, ±Inf), so the frame is always encodable.
func finite(v *float64) *float64 {
	if v == nil {
		return nil
	}
	f := *v
	if f != f || f > 1e18 || f < -1e18 {
		return nil
	}
	return &f
}
