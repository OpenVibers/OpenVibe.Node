package node

import (
	"sync"

	"github.com/OpenVibers/OpenVibe.Node/internal/protocol"
)

// jobSeenMax bounds how many job ids the Node remembers answers for.
const jobSeenMax = 1024

// jobs answers job frames. Nothing runs yet: a job is refused unless its class is advertised, and an accepted job is
// only remembered (the worker that runs it fills classes and executes accepted jobs).
type jobs struct {
	mu      sync.Mutex
	classes []string                    // runtime classes this Node runs and advertises; empty: none
	answers map[string]protocol.Message // job id → the ack or nack it got, resent unchanged for a repeated job
	order   []string                    // ids in answers, oldest first
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

// accepted reports whether id was accepted (and so would be running).
func (j *jobs) accepted(id string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	_, ok := j.answers[id].(protocol.Ack)
	return ok
}

// job answers a job frame with ack or nack, keyed by the job id.
func (n *Node) job(r protocol.JobRequest) {
	answer, accepted := n.jobs.request(r)
	if nk, ok := answer.(protocol.Nack); ok {
		n.log.Info("job refused", "id", r.Job.ID, "class", r.Job.Class, "reason", nk.Message, "err", r.Err)
	} else if accepted {
		n.log.Info("job accepted", "id", r.Job.ID, "class", r.Job.Class)
	}
	n.send(answer)
}

// jobCancel stops an accepted job. An unknown, refused or ended id is ignored, as platform.job-frame@1 says, and no
// frame answers it: an ack keyed by a job id means the job was accepted.
func (n *Node) jobCancel(c protocol.JobCancel) {
	if !n.jobs.accepted(c.ID) {
		n.log.Debug("job_cancel for a job that is not running ignored", "id", c.ID)
		return
	}
	n.log.Warn("job_cancel: job execution is not implemented, nothing to stop", "id", c.ID)
}
