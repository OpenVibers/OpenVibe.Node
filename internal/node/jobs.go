package node

import (
	"sync"

	"github.com/OpenVibers/OpenVibe.Node/internal/protocol"
	"github.com/OpenVibers/OpenVibe.Node/internal/safety"
)

// jobSeenMax bounds how many job ids the Node remembers answers for.
const jobSeenMax = 1024

// jobs answers job frames: a job is refused unless its class is advertised and admit (the worker) takes it. Running
// jobs live in the worker's own table, not here.
type jobs struct {
	mu      sync.Mutex
	classes []string                    // runtime classes this Node runs and advertises; empty: none
	answers map[string]protocol.Message // job id → the ack or nack it got, or its job_exit once it ended
	order   []string                    // ids in answers, oldest first
	// admit reserves an accepted job in the worker or returns the fault and reason to nack it with; nil accepts.
	admit func(protocol.Job) (fault, reason string)
}

func newJobs(classes []string) *jobs {
	return &jobs{classes: append([]string(nil), classes...), answers: map[string]protocol.Message{}}
}

func (j *jobs) runtimeClasses() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]string(nil), j.classes...)
}

// request answers a job frame. A job id already answered gets the same answer again, never a second acceptance;
// a job whose id is malformed is nacked but not remembered, so junk cannot evict real ids.
func (j *jobs) request(r protocol.JobRequest) (answer protocol.Message, accepted bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	id := r.Job.ID
	if a, ok := j.answers[id]; ok {
		return a, false
	}
	fault, reason := r.Check(j.classes)
	if fault == "" && j.admit != nil {
		fault, reason = j.admit(r.Job)
	}
	if fault != "" {
		answer = protocol.Nack{ID: id, FaultCode: fault, Message: reason}
	} else {
		answer, accepted = protocol.Ack{ID: id}, true
	}
	if !protocol.ValidJobID(id) {
		return answer, false
	}
	if len(j.order) >= jobSeenMax {
		delete(j.answers, j.order[0])
		j.order = j.order[1:]
	}
	j.answers[id] = answer
	j.order = append(j.order, id)
	return answer, accepted
}

// ended makes a job's job_exit the answer to a resent job frame for it.
func (j *jobs) ended(e protocol.JobExit) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, ok := j.answers[e.ID]; ok {
		j.answers[e.ID] = e
	}
}

// accepted reports whether id was accepted (and so would be running).
func (j *jobs) accepted(id string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	_, ok := j.answers[id].(protocol.Ack)
	return ok
}

// job answers a job frame with ack or nack, keyed by the job id, and runs an accepted job. A job the worker holds is
// answered from it (an ack while it runs, its job_exit once it ended) and never started twice.
func (n *Node) job(r protocol.JobRequest) {
	if n.worker != nil {
		if m, ok := n.worker.Known(r.Job.ID); ok {
			n.send(m)
			return
		}
	}
	answer, accepted := n.jobs.request(r)
	if nk, ok := answer.(protocol.Nack); ok {
		n.log.Info("job refused", "id", r.Job.ID, "class", r.Job.Class, "reason", nk.Message, "err", r.Err)
	} else if accepted {
		n.log.Info("job accepted", "id", r.Job.ID, "class", r.Job.Class)
	}
	n.send(answer)
	if accepted && n.worker != nil {
		n.worker.Launch(r.Job.ID)
	}
}

// sendJobFrame sends a worker frame; a job_exit also becomes the answer to a resent job frame.
func (n *Node) sendJobFrame(m protocol.Message) {
	if e, ok := m.(protocol.JobExit); ok {
		n.jobs.ended(e)
	}
	n.send(m)
}

// jobCancel stops an accepted job. An unknown, refused or ended id is ignored, as platform.job-frame@1 says, and no
// frame answers it: an ack keyed by a job id means the job was accepted. An ended job's unacked job_exit is resent.
func (n *Node) jobCancel(c protocol.JobCancel) {
	if n.worker != nil {
		if !n.worker.Cancel(c.ID) {
			n.log.Debug("job_cancel for a job that is not running ignored", "id", c.ID)
		}
		return
	}
	if !n.jobs.accepted(c.ID) {
		n.log.Debug("job_cancel for a job that is not running ignored", "id", c.ID)
		return
	}
	n.log.Warn("job_cancel: no worker runs jobs on this Node, nothing to stop", "id", c.ID)
}

// stopFault is the fault a job is refused with while the stop latch is set; "" when it is clear.
func stopFault(st safety.LatchState) string {
	switch {
	case st.Local:
		return protocol.FaultLocalStop
	case st.Remote:
		return protocol.FaultEstopped
	}
	return ""
}
