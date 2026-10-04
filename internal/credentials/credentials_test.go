package credentials

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/OpenVibers/OpenVibe.Node/internal/protocol"
)

const secret = "s3cr3t-credential-value"

func TestSecretNeverPrints(t *testing.T) {
	c := Credentials{DeviceID: "dev_1", Credential: NewSecret(secret), PublishKey: NewSecret("pk-" + secret)}
	for _, s := range []string{
		fmt.Sprint(c), fmt.Sprintf("%v %+v %#v %s %q", c, c, c, c.Credential, c.Credential),
		fmt.Sprintf("%v", &c), c.Credential.String(),
	} {
		if strings.Contains(s, secret) {
			t.Fatalf("secret printed: %s", s)
		}
	}
	b, _ := json.Marshal(c)
	if strings.Contains(string(b), secret) {
		t.Fatalf("secret in JSON: %s", b)
	}
	if c.Credential.Reveal() != secret {
		t.Fatal("Reveal broken")
	}
}

func TestSaveLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "credential.json")
	if _, err := Load(p, nil); !errors.Is(err, ErrNotPaired) {
		t.Fatalf("want ErrNotPaired, got %v", err)
	}
	in := &Credentials{DeviceID: "dev_1", Credential: NewSecret(secret), PublishKey: NewSecret("pk"), Server: "https://x"}
	if err := Save(p, in); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(p)
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("mode %o", st.Mode().Perm())
		}
	}
	out, err := Load(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Credential.Reveal() != secret || out.PublishKey.Reveal() != "pk" || out.DeviceID != "dev_1" {
		t.Fatalf("%+v", out)
	}
}

func TestLoadTightensMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix modes")
	}
	p := filepath.Join(t.TempDir(), "credential.json")
	if err := Save(p, &Credentials{DeviceID: "d", Credential: NewSecret(secret)}); err != nil {
		t.Fatal(err)
	}
	os.Chmod(p, 0o644)
	var warned string
	if _, err := Load(p, func(s string) { warned = s }); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 || warned == "" || strings.Contains(warned, secret) {
		t.Fatalf("mode %o warn %q", st.Mode().Perm(), warned)
	}
}

func TestCorruptFileErrorHasNoContents(t *testing.T) {
	p := filepath.Join(t.TempDir(), "credential.json")
	os.WriteFile(p, []byte(`{"credential":"`+secret+`" broken`), 0o600)
	_, err := Load(p, nil)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("err %v", err)
	}
}

// A Network pairing stores {principal, node_credential, network, paired_for} with mode 0600, before any device is
// bound; neither secret prints.
func TestSaveLoadNetworkPaired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential.json")
	c := &Credentials{Principal: "nod_01J8Z4M2Q0R7T9YV3K6N8P1W2X", NodeCredential: NewSecret(secret), Network: "https://openvibe.network",
		PairedFor: &protocol.PairedFor{Service: "bot", Ref: "rob_1"}, Server: "https://openvibe.bot"}
	if s := fmt.Sprintf("%v %+v", c, *c); strings.Contains(s, secret) {
		t.Fatalf("secret printed: %s", s)
	}
	if err := Save(path, c); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
	b, _ := os.ReadFile(path)
	var raw map[string]any
	_ = json.Unmarshal(b, &raw)
	if raw["principal"] != c.Principal || raw["node_credential"] != secret || raw["network"] != c.Network || raw["paired_for"] == nil {
		t.Fatalf("file: %s", b)
	}
	if _, ok := raw["credential"]; ok {
		t.Fatalf("a Network pairing has no Bot credential: %s", b)
	}
	got, err := Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !got.NetworkPaired() || got.Bound() || got.NodeCredential.Reveal() != secret || got.PairedFor.Ref != "rob_1" || got.Label() != c.Principal {
		t.Fatalf("%+v", got)
	}
	got.DeviceID, got.PublishKey = "dev_1", NewSecret("pk")
	if err := Save(path, got); err != nil {
		t.Fatal(err)
	}
	if again, _ := Load(path, nil); !again.Bound() || again.PublishKey.Reveal() != "pk" || again.NodeCredential.Reveal() != secret {
		t.Fatalf("%+v", again)
	}
	if err := Save(path, &Credentials{Principal: c.Principal, Network: c.Network}); err == nil {
		t.Fatal("saved a principal without its credential")
	}
}
