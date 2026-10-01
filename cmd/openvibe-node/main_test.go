package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/OpenVibers/OpenVibe.Node/internal/credentials"
	"github.com/OpenVibers/OpenVibe.Node/internal/fakebot"
	"github.com/OpenVibers/OpenVibe.Node/internal/protocol"
	"github.com/OpenVibers/OpenVibe.Node/internal/safety"
	"github.com/OpenVibers/OpenVibe.Node/internal/video"
)

// With OPENVIBE_NODE_MAIN=1 the test binary is openvibe-node itself.
func TestMain(m *testing.M) {
	if os.Getenv("OPENVIBE_NODE_MAIN") == "1" {
		os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

func cli(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return out.String(), errb.String(), code
}

func shortHome(t *testing.T) string {
	d, err := os.MkdirTemp("", "ovn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func TestHelpAndVersion(t *testing.T) {
	if out, _, code := cli(t, "help"); code != 0 || !strings.Contains(out, "openvibe-node pair <CODE>") {
		t.Fatalf("%d %s", code, out)
	}
	if out, _, code := cli(t, "version"); code != 0 || !strings.Contains(out, runtime.GOOS) {
		t.Fatalf("%d %s", code, out)
	}
	if _, _, code := cli(t, "fly"); code != 2 {
		t.Fatal("unknown command accepted")
	}
}

func pythonPath(t *testing.T) string {
	root, _ := filepath.Abs("../..")
	return filepath.Join(root, "plugins/sdk") + string(os.PathListSeparator) + filepath.Join(root, "plugins/dryrun")
}

func TestPair(t *testing.T) {
	srv := fakebot.New()
	defer srv.Close()
	srv.AddRobotCode(fakebot.RobotID, "ABCD2345")
	home := shortHome(t)
	t.Setenv("PYTHONPATH", pythonPath(t))
	if _, errOut, code := cli(t, "pair", "nope", "--home", home); code == 0 || !strings.Contains(errOut, "8 letters") {
		t.Fatalf("bad code accepted: %s", errOut)
	}
	if _, errOut, code := cli(t, "pair", "abcd-2345", "--robot", "adeept", "--home", home); code == 0 || !strings.Contains(errOut, "rob_") {
		t.Fatalf("kit name taken as a robot id: %s", errOut)
	}
	out, errOut, code := cli(t, "pair", "abcd-2345", "--home", home, "--server", srv.URL(), "--robot", fakebot.RobotID, "--name", "Rover")
	if code != 0 {
		t.Fatalf("pair failed: %s", errOut)
	}
	c, err := credentials.Load(filepath.Join(home, "credential.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, c.DeviceID) || strings.Contains(out+errOut, c.Credential.Reveal()) {
		t.Fatalf("output: %s %s", out, errOut)
	}
	st, _ := os.Stat(filepath.Join(home, "credential.json"))
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(home, "config.json")); err != nil {
		t.Fatal("config not written")
	}
	reqs := srv.PairRequests()
	if len(reqs) != 1 || reqs[0].Code != "ABCD2345" || reqs[0].AgentVersion != version || reqs[0].Robot != fakebot.RobotID || reqs[0].Name != "Rover" {
		t.Fatalf("%+v", reqs)
	}
	if _, err := exec.LookPath("python3"); err == nil && len(reqs[0].Drivers) != 1 {
		t.Fatalf("dry-run plugin not probed: %+v", reqs[0])
	}
	if _, errOut, code := cli(t, "pair", "ABCD2345", "--home", home); code == 0 || !strings.Contains(errOut, "already paired") {
		t.Fatalf("second pair: %s", errOut)
	}
	// status without a running node reads the files and never prints the secret.
	out, _, code = cli(t, "status", "--home", home)
	if code != 0 || !strings.Contains(out, "not running") || !strings.Contains(out, c.DeviceID) || strings.Contains(out, c.Credential.Reveal()) {
		t.Fatalf("%d %s", code, out)
	}
}

// TestCredentialSet stores a rotated credential from stdin without printing it.
func TestCredentialSet(t *testing.T) {
	srv := fakebot.New()
	defer srv.Close()
	srv.AddCode("ROTA2345")
	home := shortHome(t)
	t.Setenv("PYTHONPATH", pythonPath(t))
	if _, errOut, code := cli(t, "pair", "ROTA-2345", "--home", home, "--server", srv.URL()); code != 0 {
		t.Fatal(errOut)
	}
	if reqs := srv.PairRequests(); len(reqs) != 1 || reqs[0].Name == "" {
		t.Fatalf("no default name (the hostname): %+v", reqs)
	}
	file := filepath.Join(home, "credential.json")
	old, _ := credentials.Load(file, nil)
	set := func(in string) (string, string, int) {
		t.Helper()
		stdin = strings.NewReader(in)
		defer func() { stdin = os.Stdin }()
		return cli(t, "credential", "set", "--home", home)
	}
	// The rotate answer as Bot returns it: both secrets are replaced.
	body := srv.Rotate(old.Credential.Reveal(), time.Minute)
	var rot struct {
		Credential string `json:"credential"`
		PublishKey string `json:"publish_key"`
	}
	if err := json.Unmarshal(body, &rot); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := set(string(body))
	c, _ := credentials.Load(file, nil)
	if code != 0 || c.Credential.Reveal() != rot.Credential || c.PublishKey.Reveal() != rot.PublishKey || c.DeviceID != old.DeviceID {
		t.Fatalf("%d %s %s", code, out, errOut)
	}
	if strings.Contains(out+errOut, rot.Credential) || strings.Contains(out+errOut, rot.PublishKey) {
		t.Fatal("a secret was printed")
	}
	if st, _ := os.Stat(file); runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
	// The credential alone keeps the publish key.
	if _, errOut, code := set("plain-credential-0123456789\n"); code != 0 {
		t.Fatal(errOut)
	}
	if c, _ := credentials.Load(file, nil); c.Credential.Reveal() != "plain-credential-0123456789" || c.PublishKey.Reveal() != rot.PublishKey {
		t.Fatal("plain credential not stored")
	}
	if _, errOut, code := set(`{"device":{"id":"dev_other"},"credential":"x"}`); code == 0 || !strings.Contains(errOut, "dev_other") {
		t.Fatalf("another device's credential accepted: %s", errOut)
	}
	if _, errOut, code := set("  \n"); code == 0 || !strings.Contains(errOut, "no credential") {
		t.Fatalf("empty input accepted: %s", errOut)
	}
}

func TestStopWithoutRunningNodeLatchesFile(t *testing.T) {
	home := shortHome(t)
	if out, errOut, code := cli(t, "stop", "--home", home); code != 0 || !strings.Contains(out, "latched") {
		t.Fatalf("%s %s", out, errOut)
	}
	l, _ := safety.OpenLatch(filepath.Join(home, "latch.json"))
	if !l.State().Local {
		t.Fatal("not latched")
	}
	cli(t, "resume", "--home", home)
	l, _ = safety.OpenLatch(filepath.Join(home, "latch.json"))
	if l.State().Stopped() {
		t.Fatal("not resumed")
	}
}

// TestRunDryRun is the acceptance path: `openvibe-node run --dry-run` pairs with the fake server, publishes the test
// pattern, obeys commands, obeys the local kill switch and stops cleanly on SIGTERM.
func TestRunDryRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix only")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	srv := fakebot.New()
	defer srv.Close()
	srv.AddCode("WXYZ6789")
	whip := &video.WHIPReceiver{}
	srv.SetWHIP(whip) // so the pairing answer carries a whip_url (Bot does not send one yet)
	defer whip.Close()
	home := shortHome(t)
	t.Setenv("PYTHONPATH", pythonPath(t))
	if _, errOut, code := cli(t, "pair", "WXYZ-6789", "--home", home, "--server", srv.URL()); code != 0 {
		t.Fatal(errOut)
	}
	c, _ := credentials.Load(filepath.Join(home, "credential.json"), nil)
	whip.PublishKey = c.PublishKey.Reveal()

	exe, _ := os.Executable()
	cmd := exec.Command(exe, "run", "--dry-run", "--home", home)
	cmd.Env = append(os.Environ(), "OPENVIBE_NODE_MAIN=1")
	var logs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	defer func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
		}
	}()

	conn, err := srv.NextConn(20 * time.Second)
	if err != nil {
		t.Fatalf("%v\n%s", err, logs.String())
	}
	if _, err := conn.Expect(protocol.TypeStatus, 20*time.Second, func(m protocol.Message) bool {
		s := m.(protocol.Status)
		return len(s.Drivers) == 1 && s.Drivers[0].State == "ready"
	}); err != nil {
		t.Fatalf("%v\n%s", err, logs.String())
	}
	send := func(id, kind, value string) protocol.Message {
		t.Helper()
		conn.Command(fakebot.Cmd{ID: id, Kind: kind, Value: json.RawMessage(value), Deadline: time.Now().Add(300 * time.Millisecond)})
		r, err := conn.Reply(id, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if _, ok := send("c1", "drive", `{"throttle":0.4}`).(protocol.Ack); !ok {
		t.Fatal("drive not acked")
	}
	out, errOut, code := cli(t, "status", "--home", home)
	if code != 0 || !strings.Contains(out, "Link:     up") || !strings.Contains(out, "dryrun") || strings.Contains(out, c.Credential.Reveal()) {
		t.Fatalf("status: %s %s", out, errOut)
	}
	if out, _, code := cli(t, "stop", "--home", home); code != 0 || !strings.Contains(out, "held stopped") {
		t.Fatalf("stop: %s", out)
	}
	if n, ok := send("c2", "drive", `{"throttle":0.4}`).(protocol.Nack); !ok || n.FaultCode != protocol.FaultLocalStop {
		t.Fatal("drive accepted after the local stop")
	}
	cli(t, "resume", "--home", home)
	if _, ok := send("c3", "drive", `{"throttle":0.4}`).(protocol.Ack); !ok {
		t.Fatal("drive refused after resume")
	}
	deadline := time.Now().Add(20 * time.Second)
	for whip.Packets.Load() < 10 {
		if time.Now().After(deadline) {
			t.Fatalf("no test pattern received\n%s", logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("exit: %v\n%s", err, logs.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("did not stop on SIGTERM")
	}
	if strings.Contains(logs.String(), c.Credential.Reveal()) || strings.Contains(logs.String(), c.PublishKey.Reveal()) {
		t.Fatal("a secret reached the logs")
	}
	if !strings.Contains(logs.String(), "stopping every actuator") {
		t.Fatalf("no shutdown stop in logs:\n%s", logs.String())
	}
}
