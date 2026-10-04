// Package worker runs `function` jobs (OpenVibe.Contracts platform.job@1) from the control link. It runs only the
// functions the owner declared in the local config, each job as one child process in a sandbox: its own process group,
// user, mount, network (no network at all), PID, IPC, UTS and cgroup namespaces, a private read-only root holding only
// the system paths and the function's artifact, its own cgroup v2 (cpu, memory, pids, io), rlimits, no_new_privs and a
// seccomp allowlist, with a scrubbed environment and a fresh /tmp. It kills a job at its ttl, at its limits, on
// job_cancel and while the stop latch is set. It never runs a job with less isolation: a job whose sandbox cannot be
// set up fails without running, and a Node whose boot-time Probe fails does not advertise the class. docs/worker.md
// has the model and its limits.
package worker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/OpenVibers/OpenVibe.Node/internal/config"
	"github.com/OpenVibers/OpenVibe.Node/internal/protocol"
)

const (
	chunkBytes  = 16 << 10  // the most stdout bytes one job_stdout carries
	resultBytes = 256 << 10 // the most bytes of result a function may write to fd 3
	sampleEvery = 200 * time.Millisecond
	endedMax    = 1024 // ended jobs kept for their job_exit until job_exit_ack
)

// Worker runs function jobs and sends their frames with send.
type Worker struct {
	cfg  config.WorkerConfig
	caps config.WorkerCaps
	send func(protocol.Message)
	log  *slog.Logger

	// isolate puts a job's command in its sandbox; tests replace it to stand for a kernel that cannot.
	isolate isolateFunc

	mu      sync.Mutex
	stopped string          // the fault code while the stop latch is set; "" when clear
	closed  bool            // shutting down
	jobs    map[string]*job // admitted, running, or ended with its job_exit not yet acked
	ended   []string        // ids of ended jobs in jobs, oldest first
	wg      sync.WaitGroup
}

// isolateFunc turns a job's command into the start of its sandbox.
type isolateFunc func(*exec.Cmd, config.WorkerConfig, plan) (*sandbox, error)

// plan is what isolate needs to know of a job besides its command.
type plan struct {
	root   string                // the host directory (empty, the job's) its private root is mounted on
	fn     config.FunctionConfig // zero for the probe
	limits protocol.JobLimits
	egress string // the job's network policy (egressFor); "" is none
}

// egressFor is the network policy a job runs under: the stricter of the net the job names and the host's worker.egress,
// which must be the host's own, the only ruleset its veth is written with. A net of deny (or none, the same policy as
// the Node spells it), which is also what an absent net means in platform.job@1, runs under no network whatever the
// host allows: the contract's default is deny and the Node never widens it. A named public runs under a public or
// openvibe-only host's policy; a named openvibe-only needs an openvibe-only host (a public one has no egress_allow to
// restrict it to). Anything else, a host under none (or no worker.egress, the same) included, refuses the job at
// Admit rather than running it with less, or more, than was asked.
func egressFor(host, net string) (string, bool) {
	switch net {
	case "", protocol.NetDeny, protocol.NetNone:
		return "", true
	case protocol.NetPublic:
		if host == config.EgressPublic || host == config.EgressOpenVibeOnly {
			return host, true
		}
	case protocol.NetOpenVibeOnly:
		if host == config.EgressOpenVibeOnly {
			return host, true
		}
	}
	return "", false
}

type job struct {
	req      protocol.Job
	fn       config.FunctionConfig
	received time.Time
	ttl      time.Duration
	limits   protocol.JobLimits // the job's, clamped to the local caps

	reason    string      // why it was killed (the first reason wins); "" while nothing killed it
	launched  bool        // Launch was called
	started   bool        // job_started was sent (its CPU limit is set)
	proc      *os.Process // set while the process runs
	startedMS int64
	exit      *protocol.JobExit // set once it ended
}

// New returns a Worker for cfg. It runs nothing until Probe has passed and a job is admitted and launched.
func New(cfg config.WorkerConfig, send func(protocol.Message), log *slog.Logger) *Worker {
	if log == nil {
		log = slog.Default()
	}
	return &Worker{cfg: cfg, caps: cfg.Caps.WithDefaults(), send: send, log: log, isolate: isolate, jobs: map[string]*job{}}
}

// Probe starts a process the way a job is started and checks every control of the sandbox (docs/worker.md). The class
// must not be advertised when Probe fails.
func (w *Worker) Probe() error { return probe(w.cfg, w.isolate) }

// Admit reserves a job, or returns the fault code and reason to nack it with. It runs nothing (Launch does, once the
// ack is out). An id the worker already holds is admitted again without a second reservation.
func (w *Worker) Admit(j protocol.Job) (fault, reason string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.jobs[j.ID]; ok {
		return "", ""
	}
	if w.stopped != "" {
		return w.stopped, protocol.JobStopped
	}
	if w.closed {
		return protocol.FaultShuttingDown, protocol.JobNotAvailable
	}
	if j.Class != protocol.ClassFunction && j.Class != protocol.ClassCode {
		return protocol.FaultUnsupported, protocol.JobNotAvailable
	}
	// inputs are refused until the Node can stage them and check their digests (platform.job@1): a job never runs
	// with the files and the digest check silently dropped.
	if len(j.Inputs) > 0 {
		return protocol.FaultUnsupported, protocol.JobInputsRefused
	}
	if _, ok := egressFor(w.cfg.Egress, j.Net); !ok {
		return protocol.FaultUnsupported, protocol.JobNetUnsupported
	}
	fn, ok := w.entry(j.Class, j.Artifact)
	if !ok {
		return protocol.FaultUnsupported, protocol.JobUnknownArtifact
	}
	running := 0
	for _, jb := range w.jobs {
		if jb.exit == nil {
			running++
		}
	}
	if running >= w.caps.MaxJobs {
		return protocol.FaultNotReady, protocol.JobBusy
	}
	w.jobs[j.ID] = &job{req: j, fn: fn, received: time.Now(), ttl: ms(min(j.TTLMS, w.caps.MaxTTLMS)),
		limits: protocol.JobLimits{WallMS: min(j.Limits.WallMS, w.caps.MaxWallMS), CPUMS: min(j.Limits.CPUMS, w.caps.MaxCPUMS),
			MemBytes: min(j.Limits.MemBytes, w.caps.MaxMemBytes)}}
	return "", ""
}

// entry finds the declared artifact that implements class: a code job never runs a function entry, nor the reverse.
func (w *Worker) entry(class string, a *protocol.Artifact) (config.FunctionConfig, bool) {
	if a == nil {
		return config.FunctionConfig{}, false
	}
	for _, f := range w.cfg.Functions {
		if f.Name == a.Name && f.Version == a.Version && f.EffectiveClass() == class {
			return f, true
		}
	}
	return config.FunctionConfig{}, false
}

// Classes is the runtime classes the declared entries implement, in the order of protocol.RuntimeClasses: none
// when no entry is declared.
func (w *Worker) Classes() []string {
	var out []string
	for _, c := range protocol.RuntimeClasses {
		for _, f := range w.cfg.Functions {
			if f.EffectiveClass() == c {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// Launch runs an admitted job on its own goroutine. A job already launched, or unknown, is left alone.
func (w *Worker) Launch(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	jb, ok := w.jobs[id]
	if !ok || jb.launched || w.closed {
		return
	}
	jb.launched = true
	w.wg.Add(1)
	go w.run(jb)
}

// Known returns what a resent job frame for id is answered with: an ack while the job waits or runs, its job_exit
// once it ended (until job_exit_ack).
func (w *Worker) Known(id string) (protocol.Message, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	jb, ok := w.jobs[id]
	switch {
	case !ok:
		return nil, false
	case jb.exit != nil:
		return *jb.exit, true
	}
	return protocol.Ack{ID: id}, true
}

// Cancel kills a job (its job_exit says cancelled; one not started yet never starts). For an ended job whose job_exit
// is not acked yet it resends that job_exit. An unknown id is ignored; Cancel reports whether id was known.
func (w *Worker) Cancel(id string) bool {
	w.mu.Lock()
	jb, ok := w.jobs[id]
	var resend *protocol.JobExit
	if ok {
		resend = jb.exit
		w.killLocked(jb, protocol.ExitCancelled)
	}
	w.mu.Unlock()
	if resend != nil {
		w.send(*resend)
	}
	return ok
}

// ExitAck forgets an ended job: its usage is recorded, so its job_exit is not resent any more.
func (w *Worker) ExitAck(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if jb, ok := w.jobs[id]; !ok || jb.exit == nil {
		return
	}
	delete(w.jobs, id)
	for i, e := range w.ended {
		if e == id {
			w.ended = append(w.ended[:i], w.ended[i+1:]...)
			break
		}
	}
}

// SetStopped follows the stop latch (the e-stop or the local stop). A fault code kills every job at once (job_exit
// says stopped) and refuses new ones with that fault until SetStopped("").
func (w *Worker) SetStopped(fault string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopped = fault
	if fault == "" {
		return
	}
	for _, jb := range w.jobs {
		w.killLocked(jb, protocol.ExitStopped)
	}
}

// JobInfo is a job that has not ended, as `openvibe-node status` lists it.
type JobInfo struct {
	ID        string `json:"id"`
	Function  string `json:"function"` // name@version
	State     string `json:"state"`    // admitted (not started yet), running, or stopping (killed, not ended yet)
	StartedMS int64  `json:"started_ms,omitempty"`
}

// Jobs lists the jobs admitted or running, by id; ended jobs are not listed. Never nil.
func (w *Worker) Jobs() []JobInfo {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := []JobInfo{}
	for _, jb := range w.jobs {
		if jb.exit != nil {
			continue
		}
		i := JobInfo{ID: jb.req.ID, Function: jb.fn.Name + "@" + jb.fn.Version, State: "admitted"}
		if jb.started {
			i.State, i.StartedMS = "running", jb.startedMS
		}
		if jb.reason != "" {
			i.State = "stopping"
		}
		out = append(out, i)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}

// Resend sends again, after a reconnect, job_started for every running job and job_exit for every ended job whose
// job_exit is not acked yet.
func (w *Worker) Resend() {
	w.mu.Lock()
	var msgs []protocol.Message
	for _, jb := range w.jobs {
		if jb.exit == nil && jb.started && jb.proc != nil {
			msgs = append(msgs, protocol.JobStarted{ID: jb.req.ID, StartedMS: jb.startedMS})
		}
	}
	for _, id := range w.ended {
		msgs = append(msgs, *w.jobs[id].exit)
	}
	w.mu.Unlock()
	for _, m := range msgs {
		w.send(m)
	}
}

// Close kills every job (job_exit says stopped), refuses new ones and waits up to 5 s for them to end. A job admitted
// but not launched yet never will be: its job_exit (stopped) is sent here.
func (w *Worker) Close() {
	w.mu.Lock()
	w.closed = true
	var never []protocol.JobExit
	for _, jb := range w.jobs {
		w.killLocked(jb, protocol.ExitStopped)
		if jb.exit == nil && !jb.launched {
			ex := notRun(jb.req.ID, protocol.ExitStopped)
			w.endLocked(jb, ex)
			never = append(never, ex)
		}
	}
	w.mu.Unlock()
	for _, ex := range never {
		w.send(ex)
	}
	done := make(chan struct{})
	go func() { w.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		w.log.Error("jobs did not end within 5 s of being killed")
	}
}

func (w *Worker) kill(jb *job, reason string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.killLocked(jb, reason)
}

// killLocked records why jb is killed and SIGKILLs its process group; a job not started yet then never starts.
func (w *Worker) killLocked(jb *job, reason string) {
	if jb.exit != nil {
		return
	}
	if jb.reason == "" {
		jb.reason = reason
	}
	if jb.proc != nil {
		killGroup(jb.proc)
	}
}

func (w *Worker) run(jb *job) {
	defer w.wg.Done()
	ex := w.execute(jb)
	w.mu.Lock()
	w.endLocked(jb, ex)
	w.mu.Unlock()
	w.log.Info("job ended", "id", ex.ID, "reason", ex.Reason, "wall_ms", ex.Usage.WallMS)
	w.send(ex)
}

// endLocked records jb's job_exit, kept until job_exit_ack; past endedMax the oldest ended jobs are forgotten.
func (w *Worker) endLocked(jb *job, ex protocol.JobExit) {
	jb.exit = &ex
	w.ended = append(w.ended, ex.ID)
	for len(w.ended) > endedMax {
		delete(w.jobs, w.ended[0])
		w.ended = w.ended[1:]
	}
}

// notRun is the job_exit of a job whose process never ran: no code, no result, no usage.
func notRun(id, reason string) protocol.JobExit {
	return protocol.JobExit{ID: id, Reason: reason, Result: json.RawMessage("null")}
}

// execute runs jb's process to its end and returns its job_exit.
func (w *Worker) execute(jb *job) protocol.JobExit {
	id := jb.req.ID
	never := func(reason string, err error) protocol.JobExit {
		if err != nil {
			w.log.Error("job not run", "id", id, "err", err)
		}
		return notRun(id, reason)
	}
	w.mu.Lock()
	reason := jb.reason
	w.mu.Unlock()
	if reason != "" { // cancelled or stopped before it started
		return never(reason, nil)
	}
	dir, err := os.MkdirTemp("", "openvibe-job-")
	if err != nil {
		return never(protocol.ExitFailed, err)
	}
	defer os.RemoveAll(dir)
	if r := w.cfg.RunAs; r != nil {
		if err := os.Chown(dir, int(r.UID), int(r.GID)); err != nil {
			return never(protocol.ExitFailed, err)
		}
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		return never(protocol.ExitFailed, err)
	}
	defer outR.Close()
	resR, resW, err := os.Pipe()
	if err != nil {
		outW.Close()
		return never(protocol.ExitFailed, err)
	}
	defer resR.Close()
	// dir is only the mount point of the job's private root; it works in a fresh directory of the same name in its
	// own /tmp.
	work := "/tmp/" + filepath.Base(dir)
	cmd := exec.Command(jb.fn.Command[0], jb.fn.Command[1:]...)
	cmd.Dir, cmd.Env = work, jobEnv(jb.fn, work, id)
	cmd.Stdin = bytes.NewReader(append(append([]byte(nil), jb.req.Args...), '\n'))
	cmd.Stdout, cmd.ExtraFiles = outW, []*os.File{resW}
	egress, _ := egressFor(w.cfg.Egress, jb.req.Net) // Admit refused what the host cannot enforce
	sb, err := w.isolate(cmd, w.cfg, plan{root: dir, fn: jb.fn, limits: jb.limits, egress: egress})
	if err != nil {
		outW.Close()
		resW.Close()
		return never(protocol.ExitFailed, err)
	}
	defer sb.close()

	w.mu.Lock()
	if jb.reason == "" && time.Since(jb.received) >= jb.ttl {
		jb.reason = protocol.ExitTTL
	}
	reason = jb.reason
	var start time.Time
	if reason == "" {
		err = cmd.Start()
		start = time.Now()
		if err == nil {
			jb.proc, jb.startedMS = cmd.Process, start.UnixMilli()
		}
	}
	w.mu.Unlock()
	outW.Close()
	resW.Close()
	sb.started()
	if reason != "" {
		return never(reason, nil)
	}
	if err != nil {
		return never(protocol.ExitFailed, err)
	}
	pid := cmd.Process.Pid
	// job_started only once the function runs confined and RLIMIT_CPU is set (both while the sandbox init waits to
	// execute it, so it cannot have exited yet); otherwise the job is killed and ends failed (or as it was killed
	// meanwhile) without it.
	err = sb.ready(pid)
	if err == nil {
		err = limitCPU(pid, jb.limits.CPUMS)
	}
	if err == nil {
		err = sb.run()
	}
	if err != nil {
		w.mu.Lock()
		killGroup(cmd.Process)
		jb.proc = nil
		reason = jb.reason
		w.mu.Unlock()
		_ = cmd.Wait()
		if reason != "" {
			return never(reason, nil)
		}
		return never(protocol.ExitFailed, fmt.Errorf("it could not run confined: %w", err))
	}
	w.mu.Lock()
	jb.started = true
	w.mu.Unlock()
	w.send(protocol.JobStarted{ID: id, StartedMS: jb.startedMS})

	stop, watched := make(chan struct{}), make(chan accounting, 1)
	go func() { watched <- w.watch(jb, pid, start, stop) }()
	var result []byte
	var readers sync.WaitGroup
	readers.Add(2)
	go func() { defer readers.Done(); w.pump(jb, outR) }()
	go func() { defer readers.Done(); result = w.readResult(jb, resR) }()
	readers.Wait()
	_ = cmd.Wait()
	wall := time.Since(start)
	close(stop)
	acc := <-watched

	w.mu.Lock()
	reason = jb.reason
	jb.proc = nil
	w.mu.Unlock()
	code, cpuLimit := exitStatus(cmd.ProcessState)
	if reason == "" {
		reason = protocol.ExitExited
		if cpuLimit || sb.oomKilled() {
			reason = protocol.ExitLimit
		}
	}
	ex := protocol.JobExit{ID: id, Reason: reason, Code: code, Result: json.RawMessage("null")}
	if r := bytes.TrimSpace(result); reason == protocol.ExitExited && code != nil && *code == 0 && len(r) > 0 && json.Valid(r) {
		ex.Result = json.RawMessage(r)
	}
	// wall_ms is at least (n+1) s for every usage second n already sent, as platform.job-frame@1 requires.
	wallMS := max(wall.Milliseconds(), acc.seconds*1000)
	cpuMS := max(acc.cpuMS, (cmd.ProcessState.UserTime() + cmd.ProcessState.SystemTime()).Milliseconds())
	mem := max(acc.memPeak, maxRSS(cmd.ProcessState))
	started := jb.startedMS
	ex.Usage = protocol.JobExitUsage{StartedMS: &started, WallMS: wallMS, CPUMS: &cpuMS, MemPeakBytes: &mem}
	return ex
}

// accounting is what watch measured: usage seconds sent, and the CPU time and peak resident memory sampled.
type accounting struct{ seconds, cpuMS, memPeak int64 }

// watch is a job's watchdog until stop closes: it kills the job at its ttl (from receipt), at wall_ms (from start)
// and when sampled CPU time or resident memory exceed the limits, and sends job_usage for every wall-clock second
// once it has fully elapsed.
func (w *Worker) watch(jb *job, pid int, start time.Time, stop <-chan struct{}) accounting {
	var acc accounting
	ttl := time.NewTimer(time.Until(jb.received.Add(jb.ttl)))
	defer ttl.Stop()
	wall := time.NewTimer(time.Until(start.Add(ms(jb.limits.WallMS))))
	defer wall.Stop()
	usage := time.NewTimer(time.Until(start.Add(time.Second)))
	defer usage.Stop()
	tick := time.NewTicker(sampleEvery)
	defer tick.Stop()
	var ns string
	var lastCPU int64
	sample := func() {
		if ns == "" {
			ns, _ = pidNS(pid)
		}
		cpu, rss, err := sampleProcs(pid, ns)
		if err != nil {
			return
		}
		acc.cpuMS, acc.memPeak = max(acc.cpuMS, cpu), max(acc.memPeak, rss)
		if acc.cpuMS > jb.limits.CPUMS || rss > jb.limits.MemBytes {
			w.kill(jb, protocol.ExitLimit)
		}
	}
	sample()
	for {
		select {
		case <-stop:
			return acc
		case <-ttl.C:
			w.kill(jb, protocol.ExitTTL)
		case <-wall.C:
			w.kill(jb, protocol.ExitLimit)
		case <-tick.C:
			sample()
		case <-usage.C:
			for time.Since(start) >= time.Duration(acc.seconds+1)*time.Second {
				cpu := acc.cpuMS - lastCPU
				lastCPU = acc.cpuMS
				w.send(protocol.JobUsage{ID: jb.req.ID, StartedMS: jb.startedMS, Second: acc.seconds, CPUMS: &cpu})
				acc.seconds++
			}
			usage.Reset(time.Until(start.Add(time.Duration(acc.seconds+1) * time.Second)))
		}
	}
}

// pump sends the job's stdout as job_stdout chunks, never splitting a UTF-8 sequence, and kills the job (limit) once
// it wrote more than max_output_bytes; the excess is read and dropped. Invalid bytes become U+FFFD first, and both
// bounds count the bytes sent: each chunk is at most chunkBytes and all of them together at most max_output_bytes.
func (w *Worker) pump(jb *job, r io.Reader) {
	buf := make([]byte, chunkBytes)
	var carry []byte
	left := w.caps.MaxOutputBytes
	var seq uint64
	over := false
	// emit sends b's complete runes, as valid UTF-8, in chunks within both bounds; it sets over at the output cap.
	emit := func(b []byte) {
		s := strings.ToValidUTF8(string(b), "�")
		for len(s) > 0 && !over {
			n := min(len(s), chunkBytes)
			if int64(n) > left {
				n, over = int(left), true
			}
			for n < len(s) && n > 0 && !utf8.RuneStart(s[n]) {
				n--
			}
			if n > 0 {
				seq++
				w.send(protocol.JobStdout{ID: jb.req.ID, ChunkSeq: seq, Chunk: s[:n]})
				left -= int64(n)
			}
			s = s[n:]
		}
		if over {
			w.kill(jb, protocol.ExitLimit)
		}
	}
	for {
		n, err := r.Read(buf)
		if n > 0 && !over {
			b := append(carry, buf[:n]...)
			cut := completeRunes(b)
			emit(b[:cut])
			carry = append([]byte(nil), b[cut:]...)
		}
		if err != nil {
			if len(carry) > 0 && !over {
				emit(carry)
			}
			return
		}
	}
}

// completeRunes is how many leading bytes of b end on a rune boundary: a rune cut by the end of a read waits for
// the next read. Invalid bytes count as complete (they become U+FFFD).
func completeRunes(b []byte) int {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if utf8.FullRune(b[i:]) {
				return len(b)
			}
			return i
		}
	}
	return len(b)
}

// readResult reads what the function writes to fd 3, its result; more than resultBytes kills the job (limit).
func (w *Worker) readResult(jb *job, r io.Reader) []byte {
	b, _ := io.ReadAll(io.LimitReader(r, resultBytes+1))
	if len(b) > resultBytes {
		w.kill(jb, protocol.ExitLimit)
		_, _ = io.Copy(io.Discard, r)
		return nil
	}
	return b
}

// jobEnv is the whole environment of a job: nothing is inherited from the Node (no credential, no secrets), only a
// fixed PATH, the working directory as HOME and TMPDIR, the job id, and the function's own env from the config.
func jobEnv(fn config.FunctionConfig, dir, id string) []string {
	env := map[string]string{"PATH": "/usr/local/bin:/usr/bin:/bin", "HOME": dir, "TMPDIR": dir, "LANG": "C.UTF-8",
		"OPENVIBE_JOB_ID": id}
	for k, v := range fn.Env {
		env[k] = v
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

func ms(v int64) time.Duration { return time.Duration(v) * time.Millisecond }
