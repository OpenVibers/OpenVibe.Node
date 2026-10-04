package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/OpenVibers/OpenVibe.Node/internal/config"
	"github.com/OpenVibers/OpenVibe.Node/internal/credentials"
	"github.com/OpenVibers/OpenVibe.Node/internal/fakebot"
	"github.com/OpenVibers/OpenVibe.Node/internal/fakenetwork"
	"github.com/OpenVibers/OpenVibe.Node/internal/link"
	"github.com/OpenVibers/OpenVibe.Node/internal/service"
)

func fakes(t *testing.T) (*fakebot.Server, *fakenetwork.Server) {
	t.Helper()
	bot := fakebot.New()
	t.Cleanup(bot.Close)
	net := fakenetwork.New(bot)
	t.Cleanup(net.Close)
	net.AddCode("ABCD2345")
	return bot, net
}

// noSecrets fails when the code, the node credential or a node token appears in any of outs.
func noSecrets(t *testing.T, net *fakenetwork.Server, outs ...string) {
	t.Helper()
	all := strings.Join(outs, "\n")
	for _, s := range append(net.Tokens(), net.Credential(), "ABCD2345", "ABCD-2345", "abcd-2345") {
		if s != "" && strings.Contains(all, s) {
			t.Fatalf("secret %q in output: %s", s, all)
		}
	}
}

// The installer's Network command: pair on Network, store the node credential (0600), bind on Bot.
func TestPairNetwork(t *testing.T) {
	bot, net := fakes(t)
	home := shortHome(t)
	if _, errOut, code := cli(t, "pair", "--network", net.URL(), "--pairing", "pair_bad", "--code", "abcd-2345", "--home", home); code == 0 || !strings.Contains(errOut, "pair_") {
		t.Fatalf("bad pairing id accepted: %s", errOut)
	}
	out, errOut, code := cli(t, "pair", "--network", net.URL(), "--pairing", fakenetwork.PairingID, "--code", "abcd-2345", "--name", "Rover",
		"--home", home, "--server", bot.URL())
	if code != 0 {
		t.Fatalf("pair failed: %s", errOut)
	}
	path := filepath.Join(home, "credential.json")
	if st, _ := os.Stat(path); runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
	b, _ := os.ReadFile(path)
	var raw map[string]any
	_ = json.Unmarshal(b, &raw)
	if raw["principal"] != fakenetwork.Principal || raw["node_credential"] != net.Credential() || raw["network"] != net.URL() ||
		raw["paired_for"] == nil || raw["device_id"] != fakenetwork.DeviceID || raw["publish_key"] == "" || raw["server"] != bot.URL() {
		t.Fatalf("credential file: %s", b)
	}
	if !strings.Contains(out, fakenetwork.Principal) || !strings.Contains(out, fakenetwork.DeviceID) {
		t.Fatalf("output: %s", out)
	}
	if p := net.Pairings(); len(p) != 1 || p[0].Code != "ABCD2345" || p[0].Pairing != fakenetwork.PairingID || p[0].Name != "Rover" {
		t.Fatalf("%+v", p)
	}
	if bot.Binds() != 1 || len(bot.PairRequests()) != 0 {
		t.Fatalf("binds %d, legacy pairs %d", bot.Binds(), len(bot.PairRequests()))
	}
	status, _, _ := cli(t, "status", "--home", home)
	if !strings.Contains(status, fakenetwork.DeviceID+" ("+fakenetwork.Principal+")") {
		t.Fatalf("status: %s", status)
	}
	again, errAgain, code := cli(t, "pair", "--network", net.URL(), "--code", "abcd-2345", "--home", home)
	if code == 0 || !strings.Contains(errAgain, "already paired as "+fakenetwork.DeviceID) {
		t.Fatalf("second pair: %s", errAgain)
	}
	noSecrets(t, net, out, errOut, status, again, errAgain)
}

// `--pairing CODE` without --code is the code itself.
func TestPairNetworkCodeAsPairing(t *testing.T) {
	bot, net := fakes(t)
	home := shortHome(t)
	out, errOut, code := cli(t, "pair", "--network", net.URL(), "--pairing", "ABCD-2345", "--home", home, "--server", bot.URL())
	if code != 0 || !strings.Contains(out, fakenetwork.DeviceID) {
		t.Fatalf("%d %s %s", code, out, errOut)
	}
	if p := net.Pairings(); len(p) != 1 || p[0].Code != "ABCD2345" || p[0].Pairing != "" {
		t.Fatalf("%+v", p)
	}
}

// A bind that fails at pairing keeps the node credential; the next start binds and saves the device record.
func TestBindOnStartWhenDeviceMissing(t *testing.T) {
	bot, net := fakes(t)
	home := shortHome(t)
	bot.RefuseBind(http.StatusServiceUnavailable, "bot.unavailable", "try later")
	out, errOut, code := cli(t, "pair", "--network", net.URL(), "--code", "ABCD-2345", "--home", home, "--server", bot.URL())
	if code != 0 || !strings.Contains(out, "Could not bind") {
		t.Fatalf("%d %s %s", code, out, errOut)
	}
	path := filepath.Join(home, "credential.json")
	creds, err := credentials.Load(path, nil)
	if err != nil || creds.Bound() || !creds.NetworkPaired() {
		t.Fatalf("%v %+v", err, creds)
	}
	var logs bytes.Buffer
	g := &globals{paths: config.DefaultPaths(home), log: slog.New(slog.NewTextHandler(&logs, nil))}
	bound, err := bindOnStart(g, creds, link.NewTokenSource(creds, nil, "t"), bot.URL())
	if err != nil || bound.DeviceID != fakenetwork.DeviceID {
		t.Fatalf("%v %+v", err, bound)
	}
	if saved, _ := credentials.Load(path, nil); saved.DeviceID != fakenetwork.DeviceID || saved.PublishKey.IsZero() {
		t.Fatalf("not saved: %+v", saved)
	}
	// Bound now: no second bind (each one would issue a new publish key).
	if _, err := bindOnStart(g, bound, link.NewTokenSource(bound, nil, "t"), bot.URL()); err != nil || bot.Binds() != 2 {
		t.Fatalf("%v binds %d", err, bot.Binds())
	}
	noSecrets(t, net, out, errOut, logs.String())
}

// A revoked node credential stops `run` at once with the re-pair message and the exit status the service unit does
// not restart.
func TestRunStopsWhenNodeCredentialRevoked(t *testing.T) {
	bot, net := fakes(t)
	home := shortHome(t)
	bot.RefuseBind(http.StatusServiceUnavailable, "bot.unavailable", "try later")
	if _, errOut, code := cli(t, "pair", "--network", net.URL(), "--code", "ABCD-2345", "--home", home, "--server", bot.URL()); code != 0 {
		t.Fatal(errOut)
	}
	net.Revoke()
	before := len(net.TokenForms())
	out, errOut, code := cli(t, "run", "--home", home)
	if code != service.ExitPairAgain || !strings.Contains(errOut, "pair again") {
		t.Fatalf("%d %s %s", code, out, errOut)
	}
	if n := len(net.TokenForms()) - before; n != 1 {
		t.Fatalf("%d token requests after the revoke, want 1", n)
	}
	noSecrets(t, net, out, errOut)
}
