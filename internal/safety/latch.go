// Package safety holds the rules that keep motors from running when they should not: the persisted e-stop and local
// kill-switch latch, the owner's limits applied to every value before it reaches a plugin, deadline capping, and the
// command-id cache that makes a repeated command answer with its first result instead of running twice.
package safety

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// LatchState is the persisted e-stop state. Two independent latches:
//
//   - Remote: set by a server `estop {latched:true}` (or config.estop_latched); cleared by `estop {latched:false}` (the
//     owner), config.estop_latched:false, or a local `resume`.
//   - Local: set by `openvibe-node stop` on the machine itself; cleared only by `openvibe-node resume`.
//
// Anything that moves is refused while either is set.
type LatchState struct {
	Remote       bool      `json:"remote"`
	RemoteReason string    `json:"remote_reason,omitempty"`
	RemoteAt     time.Time `json:"remote_at,omitempty"`
	Local        bool      `json:"local"`
	LocalAt      time.Time `json:"local_at,omitempty"`
}

// Stopped reports whether anything is latched.
func (s LatchState) Stopped() bool { return s.Remote || s.Local }

// Latch is the persisted latch. It survives restarts: a Node that was e-stopped comes back e-stopped.
type Latch struct {
	mu       sync.Mutex
	path     string
	state    LatchState
	lastRaw  []byte
	onChange []func(LatchState)
	// cbMu runs change callbacks one at a time, each with the state current when it runs, so a slow callback can
	// never apply an older state after a newer one.
	cbMu sync.Mutex
}

// OpenLatch reads the latch file (missing = clear). A corrupt file is treated as latched: failing safe.
func OpenLatch(path string) (*Latch, error) {
	l := &Latch{path: path}
	st, raw, err := readLatch(path)
	if err != nil {
		l.state = LatchState{Local: true, LocalAt: time.Now()}
		return l, err
	}
	l.state, l.lastRaw = st, raw
	return l, nil
}

func readLatch(path string) (LatchState, []byte, error) {
	var st LatchState
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil, nil
	}
	if err != nil {
		return st, nil, err
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return st, raw, errors.New("latch file is corrupt; holding the local stop until `openvibe-node resume`")
	}
	return st, raw, nil
}

// State returns the current state.
func (l *Latch) State() LatchState {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.state
}

// OnChange registers a callback run (outside the state lock, one at a time, with the current state) after every
// change.
func (l *Latch) OnChange(f func(LatchState)) {
	l.mu.Lock()
	l.onChange = append(l.onChange, f)
	l.mu.Unlock()
}

func (l *Latch) update(f func(*LatchState)) (LatchState, error) {
	l.mu.Lock()
	before := l.state
	f(&l.state)
	st := l.state
	err := l.persistLocked()
	l.mu.Unlock()
	if before != st {
		l.notify()
	}
	return st, err
}

func (l *Latch) notify() {
	l.cbMu.Lock()
	defer l.cbMu.Unlock()
	l.mu.Lock()
	st := l.state
	cbs := append([]func(LatchState){}, l.onChange...)
	l.mu.Unlock()
	for _, cb := range cbs {
		cb(st)
	}
}

func (l *Latch) persistLocked() error {
	b, err := json.Marshal(l.state)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, l.path); err != nil {
		return err
	}
	l.lastRaw = b
	return nil
}

// SetRemote latches the server e-stop.
func (l *Latch) SetRemote(reason string) (LatchState, error) {
	return l.update(func(s *LatchState) {
		if !s.Remote {
			s.RemoteAt = time.Now().UTC()
		}
		s.Remote, s.RemoteReason = true, reason
	})
}

// ClearRemote clears the server e-stop (the owner's estop {latched:false}). The local latch is untouched.
func (l *Latch) ClearRemote() (LatchState, error) {
	return l.update(func(s *LatchState) { s.Remote, s.RemoteReason, s.RemoteAt = false, "", time.Time{} })
}

// SetLocal latches the local kill switch.
func (l *Latch) SetLocal() (LatchState, error) {
	return l.update(func(s *LatchState) {
		if !s.Local {
			s.LocalAt = time.Now().UTC()
		}
		s.Local = true
	})
}

// Resume clears both latches (`openvibe-node resume`).
func (l *Latch) Resume() (LatchState, error) {
	return l.update(func(s *LatchState) { *s = LatchState{} })
}

// Reload adopts the file's state if another process (the CLI, when the Node's socket was unreachable) changed it.
// It reports whether anything changed.
func (l *Latch) Reload() (bool, error) {
	// Read under the lock: an update between our read and our adoption would otherwise be overwritten by stale bytes.
	l.mu.Lock()
	st, raw, err := readLatch(l.path)
	if err != nil {
		if raw == nil || string(raw) == string(l.lastRaw) {
			l.mu.Unlock()
			return false, err
		}
		st = l.state
		st.Local, st.LocalAt = true, time.Now().UTC()
	} else if string(raw) == string(l.lastRaw) {
		l.mu.Unlock()
		return false, nil
	}
	changed := st != l.state
	l.state, l.lastRaw = st, raw
	l.mu.Unlock()
	if changed {
		l.notify()
	}
	return changed, err
}

// WriteLocalFile latches the local stop (or clears everything, for resume) directly in the file. The CLI uses it
// when no Node is running or its socket does not answer; a running Node picks the change up within its poll interval.
func WriteLocalFile(path string, stop bool) error {
	l, _ := OpenLatch(path)
	var err error
	if stop {
		_, err = l.SetLocal()
	} else {
		_, err = l.Resume()
	}
	return err
}
