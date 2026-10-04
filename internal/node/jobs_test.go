//go:build !windows

package node

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/OpenVibers/OpenVibe.Node/internal/protocol"
)

func jobID(n int) string { return fmt.Sprintf("job_01JAB2C3D4E5F6G7H8J9K%05d", n) }

func testJob(id string) protocol.Job {
	return protocol.Job{ID: id, Class: protocol.ClassFunction, Artifact: &protocol.Artifact{Name: "thumbnail", Version: "1.2.0"},
		Args: json.RawMessage(`{"width":320}`), TTLMS: 60000, Limits: protocol.JobLimits{WallMS: 30000, CPUMS: 30000, MemBytes: 268435456}}
}

func (e *env) jobReply(j protocol.Job) protocol.Message {
	e.t.Helper()
	if err := e.conn.Job(j); err != nil {
		e.t.Fatal(err)
	}
	r, err := e.conn.Reply(j.ID, 5*time.Second)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *env) jobNack(j protocol.Job, reason string) protocol.Nack {
	e.t.Helper()
	r := e.jobReply(j)
	nk, ok := r.(protocol.Nack)
	if !ok || nk.ID != j.ID || nk.Message != reason || nk.FaultCode == "" {
		e.t.Fatalf("job %s: %+v, want a nack %q", j.ID, r, reason)
	}
	return nk
}

// TestJobsRefused: with no runtime class advertised (nothing executes yet) every job is nacked, with the first problem
// as the reason, a resent job id gets the same nack, and job_cancel, job_exit_ack and unknown frames change nothing.
func TestJobsRefused(t *testing.T) {
	e := start(t)
	for _, f := range e.conn.Frames() {
		if s, ok := f.Msg.(protocol.Status); ok {
			if _, ok := s.Capabilities[protocol.CapWorker]; ok {
				t.Fatalf("worker capability advertised with no runtime class: %+v", s.Capabilities)
			}
		}
	}

	first := e.jobNack(testJob(jobID(1)), protocol.JobNotAvailable)
	if first.FaultCode != protocol.FaultUnsupported {
		t.Fatalf("%+v", first)
	}
	if again := e.jobNack(testJob(jobID(1)), protocol.JobNotAvailable); again != first {
		t.Fatalf("resent job answered %+v, first %+v", again, first)
	}

	j := testJob(jobID(2))
	j.Class = "quantum"
	e.jobNack(j, protocol.JobUnknownClass)

	j = testJob(jobID(3))
	j.Net = "allow"
	e.jobNack(j, protocol.JobNetUnsupported)

	// A malformed input is nacked by Check, before the class is even looked at.
	j = testJob(jobID(8))
	j.Inputs = []protocol.JobInput{{Name: "../escape", MediaID: "med_01JAB2C3D4E5F6G7H8J9K0MNPQ",
		SHA256: "9f86d081884c7d659a2feb15b0b4f8f1c0e6ad1d7e6fd2e1b0b6fd1c4f1d2a3b"}}
	e.jobNack(j, protocol.JobBadInputs)

	e.jobNack(testJob("job_123"), protocol.JobBadID)

	j = testJob(jobID(4))
	j.Artifact.Version = ">=1.0"
	e.jobNack(j, protocol.JobNoArtifact)

	// A body that does not decode is nacked by its id, not dropped.
	bad := fmt.Sprintf(`{"v":1,"seq":900,"ts":%d,"type":"job","job":{"id":%q,"class":"function","ttl_ms":"soon"}}`, time.Now().UnixMilli(), jobID(5))
	if err := e.conn.SendRaw([]byte(bad)); err != nil {
		t.Fatal(err)
	}
	if r, err := e.conn.Reply(jobID(5), 5*time.Second); err != nil || r.(protocol.Nack).Message != protocol.JobBadFrame {
		t.Fatalf("%+v %v", r, err)
	}

	// An unknown frame type, job_cancel and job_exit_ack for unknown ids: ignored, nothing answers them, and the next job
	// is still answered.
	if err := e.conn.SendRaw([]byte(fmt.Sprintf(`{"v":1,"seq":901,"ts":%d,"type":"future","id":%q}`, time.Now().UnixMilli(), jobID(6)))); err != nil {
		t.Fatal(err)
	}
	if err := e.conn.JobCancel(jobID(6)); err != nil {
		t.Fatal(err)
	}
	if err := e.conn.JobCancel(jobID(1)); err != nil { // refused earlier: not running either
		t.Fatal(err)
	}
	if err := e.conn.JobExitAck(jobID(6)); err != nil {
		t.Fatal(err)
	}
	e.jobNack(testJob(jobID(7)), protocol.JobNotAvailable)
	nacks1 := 0
	for _, f := range e.conn.Frames() {
		switch m := f.Msg.(type) {
		case protocol.Ack:
			t.Fatalf("ack with nothing accepted: %+v", m)
		case protocol.Nack:
			if m.ID == jobID(6) {
				t.Fatalf("job_cancel or job_exit_ack answered: %+v", m)
			}
			if m.ID == jobID(1) {
				nacks1++
			}
		}
	}
	if nacks1 != 2 {
		t.Fatalf("job %s answered %d times, want 2 (one per job frame)", jobID(1), nacks1)
	}
}

// TestJobAcceptedOnce: once a class is advertised a job is acked, a resent one is acked again (never a second
// acceptance), and status.capabilities.worker lists the classes.
func TestJobAcceptedOnce(t *testing.T) {
	e := start(t)
	e.node.jobs.mu.Lock()
	e.node.jobs.classes = []string{protocol.ClassFunction}
	e.node.jobs.mu.Unlock()

	if r := e.jobReply(testJob(jobID(1))); r != (protocol.Ack{ID: jobID(1)}) {
		t.Fatalf("%+v", r)
	}
	if _, accepted := e.node.jobs.request(protocol.JobRequest{Job: testJob(jobID(1))}); accepted {
		t.Fatal("a resent job was accepted twice")
	}
	if r := e.jobReply(testJob(jobID(1))); r != (protocol.Ack{ID: jobID(1)}) {
		t.Fatalf("resent: %+v", r)
	}
	if err := e.conn.JobCancel(jobID(1)); err != nil {
		t.Fatal(err)
	}

	// The next connection's status advertises the class.
	e.conn.Close()
	e.conn = e.nextConn()
	_, err := e.conn.Expect(protocol.TypeStatus, 10*time.Second, func(m protocol.Message) bool {
		w, _ := m.(protocol.Status).Capabilities[protocol.CapWorker].(map[string]any)
		c, _ := w["runtime_classes"].([]any)
		return len(c) == 1 && c[0] == protocol.ClassFunction
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestJobAnswersBounded: the Node remembers the answers of the last jobSeenMax valid job ids only, and never a
// malformed one.
func TestJobAnswersBounded(t *testing.T) {
	j := newJobs(nil)
	for i := 0; i <= jobSeenMax; i++ {
		j.request(protocol.JobRequest{Job: testJob(jobID(i))})
	}
	j.request(protocol.JobRequest{Job: testJob("job_bad")})
	if len(j.answers) != jobSeenMax || len(j.order) != jobSeenMax {
		t.Fatalf("%d answers, %d ids", len(j.answers), len(j.order))
	}
	if _, ok := j.answers[jobID(0)]; ok {
		t.Fatal("the oldest id was not forgotten")
	}
	if _, ok := j.answers[jobID(jobSeenMax)]; !ok {
		t.Fatal("the newest id was forgotten")
	}
}
