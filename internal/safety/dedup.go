package safety

import (
	"container/list"
	"sync"
	"time"

	"github.com/OpenVibers/OpenVibe.Node/internal/protocol"
)

// Entry is one command id's result, pending until Complete.
type Entry struct {
	id     string
	done   chan struct{}
	result protocol.Message
	at     time.Time
	elem   *list.Element
}

// Done is closed when the result is known.
func (e *Entry) Done() <-chan struct{} { return e.done }

// Result is the first result (an Ack or a Nack). Valid after Done.
func (e *Entry) Result() protocol.Message { return e.result }

// Dedup remembers command ids so a repeated id is answered with the first result and never executed twice. It lives
// for the whole process, across reconnects, so a server that re-sends after a reconnect gets the old answer.
type Dedup struct {
	mu    sync.Mutex
	max   int
	ttl   time.Duration
	items map[string]*Entry
	order *list.List // oldest first
	now   func() time.Time
}

func NewDedup(max int, ttl time.Duration) *Dedup {
	return &Dedup{max: max, ttl: ttl, items: map[string]*Entry{}, order: list.New(), now: time.Now}
}

// Begin registers id. first is true when the caller must execute the command and later call Complete; otherwise the
// caller waits on e.Done() and answers with e.Result().
func (d *Dedup) Begin(id string) (e *Entry, first bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	for f := d.order.Front(); f != nil; f = d.order.Front() {
		old := f.Value.(*Entry)
		if len(d.items) <= d.max && now.Sub(old.at) < d.ttl {
			break
		}
		d.order.Remove(f)
		delete(d.items, old.id)
	}
	if e, ok := d.items[id]; ok {
		return e, false
	}
	e = &Entry{id: id, done: make(chan struct{}), at: now}
	e.elem = d.order.PushBack(e)
	d.items[id] = e
	return e, true
}

// Complete records the first result. Later calls are ignored.
func (d *Dedup) Complete(e *Entry, result protocol.Message) {
	d.mu.Lock()
	defer d.mu.Unlock()
	select {
	case <-e.done:
		return
	default:
	}
	e.result = result
	close(e.done)
}
