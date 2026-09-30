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
