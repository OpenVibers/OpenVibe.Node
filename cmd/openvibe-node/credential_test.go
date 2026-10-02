package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/OpenVibers/OpenVibe.Node/internal/credentials"
)

func importCLI(t *testing.T, home, stdin string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	code := runWithStdin([]string{"credential", "import", "--home", home}, strings.NewReader(stdin), &out, &errb)
	return out.String(), errb.String(), code
}

// rotateJSON is OpenVibe.Bot's POST /api/v1/devices/:id/rotate answer.
func rotateJSON(deviceID, cred, pk string) string {
	return `{"device":{"id":"` + deviceID + `","name":"Rover"},"credential":"` + cred + `","publish_key":"` + pk + `"}`
}

func TestCredentialImport(t *testing.T) {
	home := shortHome(t)
	path := filepath.Join(home, "credential.json")
	old := &credentials.Credentials{
		DeviceID: "dev_1", RobotID: "rob_1", Credential: credentials.NewSecret("old-cred"), PublishKey: credentials.NewSecret("old-pk"),
		Server: "https://openvibe.bot", DeviceURL: "wss://openvibe.bot/device", WHIPURL: "https://ingest.openre.stream/whip/old",
	}
	if err := credentials.Save(path, old); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := importCLI(t, home, rotateJSON("dev_1", "new-cred", "new-pk"))
	if code != 0 {
		t.Fatalf("code %d: %s %s", code, out, errOut)
	}
	if !strings.Contains(out, "dev_1") || !strings.Contains(out, "restart") {
		t.Fatalf("success line: %q", out)
	}
	if all := out + errOut; strings.Contains(all, "new-cred") || strings.Contains(all, "new-pk") || strings.Contains(all, "old-cred") || strings.Contains(all, "old-pk") {
		t.Fatalf("a secret reached the output: %q %q", out, errOut)
	}
	got, err := credentials.Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Credential.Reveal() != "new-cred" || got.PublishKey.Reveal() != "new-pk" {
		t.Fatalf("secrets not replaced: %+v", got)
	}
	// Everything else is kept.
	if got.DeviceID != "dev_1" || got.RobotID != "rob_1" || got.Server != old.Server || got.DeviceURL != old.DeviceURL || got.WHIPURL != old.WHIPURL {
		t.Fatalf("a stored field was lost: %+v", got)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("mode %o", st.Mode().Perm())
		}
	}
}

func TestCredentialImportRefusesOtherDevice(t *testing.T) {
	home := shortHome(t)
	path := filepath.Join(home, "credential.json")
	if err := credentials.Save(path, &credentials.Credentials{DeviceID: "dev_1", Credential: credentials.NewSecret("old-cred"), PublishKey: credentials.NewSecret("old-pk")}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out, errOut, code := importCLI(t, home, rotateJSON("dev_2", "new-cred", "new-pk"))
	if code == 0 || !strings.Contains(errOut, "dev_2") || !strings.Contains(errOut, "dev_1") {
		t.Fatalf("code %d: %s %s", code, out, errOut)
	}
	if all := out + errOut; strings.Contains(all, "new-cred") || strings.Contains(all, "new-pk") {
		t.Fatalf("a secret reached the output: %q %q", out, errOut)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("the credential file changed on a refused import")
	}
}

func TestCredentialImportRefusesWhenNotPaired(t *testing.T) {
	home := shortHome(t)
	out, errOut, code := importCLI(t, home, rotateJSON("dev_1", "new-cred", "new-pk"))
	if code == 0 || !strings.Contains(errOut, "pair") {
		t.Fatalf("code %d: %s %s", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(home, "credential.json")); !os.IsNotExist(err) {
		t.Fatalf("a credential file appeared: %v", err)
	}
	if strings.Contains(out+errOut, "new-cred") || strings.Contains(out+errOut, "new-pk") {
		t.Fatalf("a secret reached the output: %q %q", out, errOut)
	}
}

func TestCredentialImportRefusesIncompleteResponse(t *testing.T) {
	home := shortHome(t)
	path := filepath.Join(home, "credential.json")
	if err := credentials.Save(path, &credentials.Credentials{DeviceID: "dev_1", Credential: credentials.NewSecret("old-cred"), PublishKey: credentials.NewSecret("old-pk")}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cases := []string{
		// no publish_key
		`{"device":{"id":"dev_1"},"credential":"new-cred"}`,
		// no credential
		`{"device":{"id":"dev_1"},"publish_key":"new-pk"}`,
		// no device.id
		`{"credential":"new-cred","publish_key":"new-pk"}`,
		`not json`,
		``,
		// over the read cap
		strings.Repeat("a", maxRotateJSON+1),
	}
	for _, in := range cases {
		out, errOut, code := importCLI(t, home, in)
		if code == 0 {
			t.Fatalf("accepted %q: %s", in, out)
		}
		if all := out + errOut; strings.Contains(all, "new-cred") || strings.Contains(all, "new-pk") {
			t.Fatalf("a secret reached the output for %q: %q %q", in, out, errOut)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatalf("the credential file changed on %q", in)
		}
	}
}

func TestCredentialImportSubcommandUsage(t *testing.T) {
	if _, errOut, code := cli(t, "credential"); code != 2 || !strings.Contains(errOut, "credential import") {
		t.Fatalf("bare credential: %d %s", code, errOut)
	}
	if _, _, code := cli(t, "credential", "bogus"); code != 2 {
		t.Fatal("unknown subcommand accepted")
	}
}
