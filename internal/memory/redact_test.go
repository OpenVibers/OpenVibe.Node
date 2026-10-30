package memory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// halves joins the two halves of a value.
//
// The test files of this repository are read by a secret scanner too, and
// these fixtures hold fake secrets on purpose. Writing a whole secret in one
// piece would make the scanner report the test itself, so every value that
// looks like a secret is cut in two.
func halves(a, b string) string { return a + b }

// The fake secrets the shapes are tested with. Every one of them is written
// in halves, so that the secret scanner of the repository does not read this
// test file as a file that holds a secret.
var (
	secretOpenSSH   = halves("b3BlbnNzaC1rZXktdjEAAAAABG5vbm", "UAAAAEbm9uZQ")
	secretPGP       = halves("lQOYBFc0OaJBCFAVvJ0lhbXVs", "C")
	secretBasic     = halves("ZGVtbzpwYXNzd29yZ", "A==")
	secretJWT       = halves("eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0N", "TY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U")
	secretOpenAI    = halves("sk-proj-A1b2C3d4E5f6G7", "h8I9j0K1l2M3n4O5p6Q7r8")
	secretAnthropic = halves("sk-ant-api03-BBBBBBBBBBBB", "BBBBBBBBBBBBBBBBBBBBBBBBBBB")
	secretStripe    = halves("sk_live_0000", "00000000000FAKE")
	secretRSABody   = halves("MIIEowIBAAKCAQEAtESTKEYONLY", "FAKEFIXTURE")
	// The lines of a key block are written in two pieces too, so that
	// the scanner does not read this test file as a file that holds a key.
	pemBeginRSA = "-----BEGIN RSA " + "PRIVATE KEY-----\n"
	pemEndRSA   = "-----END RSA " + "PRIVATE KEY-----\n"
)

// plantedSecrets are the fake secrets the fixtures hold, one list per
// adapter. The whole-output check looks for every one of them.
var plantedSecrets = map[string][]string{
	"agents": {
		halves("ghp_AAAAAAAAAAAAAAAA", "AAAAAAAAAAAAAAAAAAAA"),
	},
	"aider": {
		halves("Sup3rSecre", "tFakeValue"),
	},
	"claude-code": {
		halves("sk-proj-A1b2C3d4E5f6G7", "h8I9j0K1l2M3n4O5p6Q7r8"),
		halves("MIIEowIBAAKCAQEAtESTKEYONLYOPENSSH", "FAKE000000000000"),
		halves("MIIEowIBAAKCAQEAtESTKEYONLY", "FAKEFIXTURE0000000000000000"),
		halves("fake-db-pa", "ssword-123"),
		halves("fake-stripe", "-secret-999"),
		halves("correct horse ", "battery staple"),
	},
	"cline": {
		halves("Sup3rSecre", "tFakeValue"),
	},
	"command-code": {
		halves("github_pat_11AAAAAAAAAA", "AAAAAAAAAAAAAAAAAAAAAAA"),
	},
	"continue": {
		halves("ghp_BBBBBBBBBBBBBBBB", "BBBBBBBBBBBBBBBBBBBB"),
	},
	"copilot": {
		halves("eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0N", "TY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"),
	},
	"cursor": {
		halves("xoxb-0000000", "000000000FAKE"),
	},
	"gemini": {
		halves("AKIAIOSFOD", "NN7EXAMPLE"),
	},
	"kiro": {
		halves("hunte", "r2fake"),
	},
	"opencode": {
		halves("wJalrXUtnFEMI0000000", "0000000000000000FAKE"),
	},
	"roo": {
		halves("sk_live_000000", "0000000000FAKE"),
	},
	"windsurf": {
		halves("MIIEowIBAAKCAQEAtESTKEYONLY", "FAKEFIXTURE0000000000000000"),
	},
}

// fixtureAdapters is every adapter that has a fixture.
var fixtureAdapters = []string{
	"agents",
	"aider",
	"claude-code",
	"cline",
	"command-code",
	"continue",
	"copilot",
	"cursor",
	"gemini",
	"kiro",
	"opencode",
	"roo",
	"windsurf",
}

// pemBlock is a private key block with the given label and body.
func pemBlock(label, body string) string {
	return "-----BEGIN " + label + "-----\n" + body + "\n-----END " + label + "-----\n"
}

// TestRedactEveryShape checks every shape of secret one by one: the value
// goes, the words around it stay.
func TestRedactEveryShape(t *testing.T) {
	cases := []struct {
		name   string
		text   string
		secret string
		gone   []string
		keep   []string
	}{
		{
			name:   "pem rsa",
			text:   "the deploy key is\n" + pemBlock("RSA PRIVATE KEY", "MIIEowIBAAKCAQEAtESTKEYONLYFAKEFIXTURE") + "and the deploy works",
			secret: "MIIEowIBAAKCAQEAtESTKEYONLYFAKEFIXTURE",
			gone:   []string{"PRIVATE KEY"},
			keep:   []string{"the deploy key is", "and the deploy works"},
		},
		{
			name:   "pem ec",
			text:   "the key is " + pemBlock("EC PRIVATE KEY", "MHcCAQEEIBBBBBBBBBBBBBBBBBBBBBBBBBBBB") + "in the vault",
			secret: "MHcCAQEEIBBBBBBBBBBBBBBBBBBBBBBBBBBBB",
			gone:   []string{"PRIVATE KEY"},
			keep:   []string{"the key is", "in the vault"},
		},
		{
			name:   "pem openssh",
			text:   "the key is " + pemBlock("OPENSSH PRIVATE KEY", secretOpenSSH) + "in the vault",
			secret: secretOpenSSH,
			gone:   []string{"PRIVATE KEY"},
			keep:   []string{"the key is", "in the vault"},
		},
		{
			name:   "pem pgp",
			text:   "the key is " + pemBlock("PGP PRIVATE KEY BLOCK", secretPGP) + "in the vault",
			secret: secretPGP,
			gone:   []string{"PRIVATE KEY"},
			keep:   []string{"the key is", "in the vault"},
		},
		{
			name:   "pem encrypted",
			text:   "the key is " + pemBlock("ENCRYPTED PRIVATE KEY", "MIIEowIBAAKCAQEAtESTKEYONLYFAKEFIXTURE") + "in the vault",
			secret: "MIIEowIBAAKCAQEAtESTKEYONLYFAKEFIXTURE",
			gone:   []string{"PRIVATE KEY"},
			keep:   []string{"the key is", "in the vault"},
		},
		{
			name:   "env assignment",
			text:   "the line is DATABASE_PASSWORD=hunter2xyz in the file",
			secret: "hunter2xyz",
			keep:   []string{"DATABASE_PASSWORD=", "in the file"},
		},
		{
			name:   "env assignment with a dot in the name",
			text:   "the line is MY.SECRET=abcdefghijkl in the file",
			secret: "abcdefghijkl",
			keep:   []string{"MY.SECRET=", "in the file"},
		},
		{
			name:   "env assignment with a colon",
			text:   "the line is api_key: abcdefghijklmnop in the file",
			secret: "abcdefghijklmnop",
			keep:   []string{"api_key:", "in the file"},
		},
		{
			name:   "env assignment with an arrow",
			text:   "the line is access_key => abcdefghijklmnop in the file",
			secret: "abcdefghijklmnop",
			keep:   []string{"access_key =>", "in the file"},
		},
		{
			name:   "env assignment quoted",
			text:   "the line is password: \"correct horse battery staple\" in the file",
			secret: "correct horse battery staple",
			keep:   []string{"password:", "in the file"},
		},
		{
			name:   "env assignment single quoted",
			text:   "the line is client_secret: 'correct horse battery staple' in the file",
			secret: "correct horse battery staple",
			keep:   []string{"client_secret:", "in the file"},
		},
		{
			name:   "env assignment with a plural name",
			text:   "the line is max_tokens: 100000 in the file",
			secret: "100000",
			keep:   []string{"max_tokens:", "in the file"},
		},
		{
			name:   "prose",
			text:   "the password is hunter2xyz and it must not be shown",
			secret: "hunter2xyz",
			keep:   []string{"the password is", "and it must not be shown"},
		},
		{
			name:   "prose with was",
			text:   "the passphrase was correcthorsebattery and it must not be shown",
			secret: "correcthorsebattery",
			keep:   []string{"the passphrase was", "and it must not be shown"},
		},
		{
			name:   "authorization bearer",
			text:   "the header is Authorization: Bearer abcdefghijklmnopqrstuvwxyz012345 and it must not be shown",
			secret: "abcdefghijklmnopqrstuvwxyz012345",
			keep:   []string{"Authorization: Bearer", "and it must not be shown"},
		},
		{
			name:   "authorization basic",
			text:   "the header is Authorization: Basic " + secretBasic + " and it must not be shown",
			secret: secretBasic,
			keep:   []string{"Authorization: Basic", "and it must not be shown"},
		},
		{
			name:   "x api key",
			text:   "the header is x-api-key: abcdefghijklmnopqrst and it must not be shown",
			secret: "abcdefghijklmnopqrst",
			keep:   []string{"x-api-key:", "and it must not be shown"},
		},
		{
			name:   "bare bearer",
			text:   "the header is Bearer abcdefghijklmnopqrstuvwxyz012345 and it must not be shown",
			secret: "abcdefghijklmnopqrstuvwxyz012345",
			keep:   []string{"Bearer", "and it must not be shown"},
		},
		{
			name:   "google api key",
			text:   "the key is AIzaBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB and it must not be shown",
			secret: "AIzaBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB",
			keep:   []string{"the key is", "and it must not be shown"},
		},
		{
			name:   "gitlab token",
			text:   "the token is glpat-cccccccccccccccccccc and it must not be shown",
			secret: "glpat-cccccccccccccccccccc",
			keep:   []string{"the token is", "and it must not be shown"},
		},
		{
			name:   "npm token",
			text:   "the token is npm_dddddddddddddddddddddddddddddddddddd and it must not be shown",
			secret: "npm_dddddddddddddddddddddddddddddddddddd",
			keep:   []string{"the token is", "and it must not be shown"},
		},
		{
			name:   "hugging face token",
			text:   "the token is hf_eeeeeeeeeeeeeeeeeeeeeeeeeeeeee and it must not be shown",
			secret: "hf_eeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
			keep:   []string{"the token is", "and it must not be shown"},
		},
		{
			name:   "sendgrid key",
			text:   "the key is SG.ffffffffffffffffffff.gggggggggggggggggggg and it must not be shown",
			secret: "SG.ffffffffffffffffffff.gggggggggggggggggggg",
			keep:   []string{"the key is", "and it must not be shown"},
		},
		{
			name:   "telegram bot token",
			text:   "the token is 123456789:hhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhh and it must not be shown",
			secret: "123456789:hhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhh",
			keep:   []string{"the token is", "and it must not be shown"},
		},
		{
			name:   "discord bot token",
			text:   "the token is Miiiiiiiiiiiiiiiiiiiiiii.jjjjjj.kkkkkkkkkkkkkkkkkkkkkkkkkkk and it must not be shown",
			secret: "Miiiiiiiiiiiiiiiiiiiiiii.jjjjjj.kkkkkkkkkkkkkkkkkkkkkkkkkkk",
			keep:   []string{"the token is", "and it must not be shown"},
		},
		{
			name:   "twilio key",
			text:   "the key is SK" + strings.Repeat("1a", 16) + " and it must not be shown",
			secret: "SK" + strings.Repeat("1a", 16),
			keep:   []string{"the key is", "and it must not be shown"},
		},
		{
			name:   "slack app token",
			text:   "the token is xapp-llllllllll and it must not be shown",
			secret: "xapp-llllllllll",
			keep:   []string{"the token is", "and it must not be shown"},
		},
		{
			name:   "openvibe token",
			text:   "the token is hbt_mmmmmmmmmmmmmmmm and it must not be shown",
			secret: "hbt_mmmmmmmmmmmmmmmm",
			keep:   []string{"the token is", "and it must not be shown"},
		},
		{
			name:   "phoenix grove key",
			text:   "the key is pgsk_nnnnnnnnnnnnnnnn and it must not be shown",
			secret: "pgsk_nnnnnnnnnnnnnnnn",
			keep:   []string{"the key is", "and it must not be shown"},
		},
		{
			name:   "slack webhook",
			text:   "the hook is https://hooks.slack.com/services/T0000/B0000/XXXXXXXXXXXXXXXXXXXXXXXX and it must not be shown",
			secret: "https://hooks.slack.com/services/T0000/B0000/XXXXXXXXXXXXXXXXXXXXXXXX",
			keep:   []string{"the hook is", "and it must not be shown"},
		},
		{
			name:   "discord webhook",
			text:   "the hook is https://discord.com/api/webhooks/1234/abcdefghijklmnop and it must not be shown",
			secret: "https://discord.com/api/webhooks/1234/abcdefghijklmnop",
			keep:   []string{"the hook is", "and it must not be shown"},
		},
		{
			name:   "card number",
			text:   "the card is 4111111111111111 and it must not be shown",
			secret: "4111111111111111",
			keep:   []string{"the card is", "and it must not be shown"},
		},
		{
			name:   "card number with spaces",
			text:   "the card is 4111 1111 1111 1111 and it must not be shown",
			secret: "4111 1111 1111 1111",
			keep:   []string{"the card is", "and it must not be shown"},
		},
		{
			name:   "aws key",
			text:   "the key is AKIAIOSFODNN7EXAMPLE and it must not be shown",
			secret: "AKIAIOSFODNN7EXAMPLE",
			keep:   []string{"the key is", "and it must not be shown"},
		},
		{
			name:   "github token",
			text:   "the token is ghp_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA and it must not be shown",
			secret: "ghp_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
			keep:   []string{"the token is", "and it must not be shown"},
		},
		{
			name:   "github fine grained token",
			text:   "the token is github_pat_11AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA and it must not be shown",
			secret: "github_pat_11AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
			keep:   []string{"the token is", "and it must not be shown"},
		},
		{
			name:   "jwt",
			text:   "the token is " + secretJWT + " and it must not be shown",
			secret: secretJWT,
			keep:   []string{"the token is", "and it must not be shown"},
		},
		{
			name:   "openai key",
			text:   "the key is " + secretOpenAI + " and it must not be shown",
			secret: secretOpenAI,
			keep:   []string{"the key is", "and it must not be shown"},
		},
		{
			name:   "anthropic key",
			text:   "the key is " + secretAnthropic + " and it must not be shown",
			secret: secretAnthropic,
			keep:   []string{"the key is", "and it must not be shown"},
		},
		{
			name:   "stripe key",
			text:   "the key is " + secretStripe + " and it must not be shown",
			secret: secretStripe,
			keep:   []string{"the key is", "and it must not be shown"},
		},
		{
			name:   "slack token",
			text:   "the token is xoxb-0000000000000000FAKE and it must not be shown",
			secret: "xoxb-0000000000000000FAKE",
			keep:   []string{"the token is", "and it must not be shown"},
		},
		{
			name:   "connection string",
			text:   "the url is postgres://dbuser:hunter2fake@db.internal:5432/app and it must not be shown",
			secret: "hunter2fake",
			keep:   []string{"postgres://dbuser:", "@db.internal:5432/app", "and it must not be shown"},
		},
	}
	for _, c := range cases {
		out, n := Redact(c.text)
		if n == 0 {
			t.Errorf("%s: nothing was redacted: %q", c.name, out)
			continue
		}
		if strings.Contains(out, c.secret) {
			t.Errorf("%s: the secret is still there: %q", c.name, out)
		}
		for _, gone := range c.gone {
			if strings.Contains(out, gone) {
				t.Errorf("%s: %q is still there: %q", c.name, gone, out)
			}
		}
		for _, keep := range c.keep {
			if !strings.Contains(out, keep) {
				t.Errorf("%s: the text lost %q: %q", c.name, keep, out)
			}
		}
	}
}

// TestRedactASecretThatSpansTwoItems checks a key block whose BEGIN and END
// lines land in two different items: only the whole-file pass sees it.
func TestRedactASecretThatSpansTwoItems(t *testing.T) {
	doc := "the deploy key:\n\n" +
		"```text\n" +
		pemBeginRSA +
		secretRSABody + "\n" +
		"\n" +
		secretRSABody + "2\n" +
		pemEndRSA +
		"```\n"
	items := parseMarkdown(parseOptions{}, "x.md", []byte(doc))
	if len(items) == 0 {
		t.Fatal("the file gave no items")
	}
	for _, it := range items {
		if strings.Contains(it.Text, "PRIVATE KEY") {
			t.Errorf("the key is still in an item: %q", it.Text)
		}
		if strings.Contains(it.Text, "MIIEow") {
			t.Errorf("the key body is still in an item: %q", it.Text)
		}
	}
	found := false
	for _, it := range items {
		if it.Redacted {
			found = true
		}
	}
	if !found {
		t.Errorf("nothing was marked redacted: %+v", items)
	}
}

// TestRedactAKeyWithNoEndLine checks a key block that was cut off: it goes
// from its BEGIN line to the end of its fence, and the text after the fence
// stays.
func TestRedactAKeyWithNoEndLine(t *testing.T) {
	doc := "the vault key:\n\n" +
		"```text\n" +
		pemBeginRSA +
		secretRSABody + "\n" +
		"```\n\n" +
		"the notes after the block stay\n"
	items := parseMarkdown(parseOptions{}, "x.md", []byte(doc))
	if len(items) == 0 {
		t.Fatal("the file gave no items")
	}
	joined := ""
	for _, it := range items {
		if strings.Contains(it.Text, "MIIEow") {
			t.Errorf("the key body is still in an item: %q", it.Text)
		}
		if strings.Contains(it.Text, "PRIVATE KEY") {
			t.Errorf("the key is still in an item: %q", it.Text)
		}
		joined += it.Text + "\n"
	}
	if !strings.Contains(joined, "the notes after the block stay") {
		t.Errorf("the text after the fence was swallowed: %q", joined)
	}
}

// TestRedactAnEnvBlockInAFence checks an .env block in a fence: every value
// goes, every name stays.
func TestRedactAnEnvBlockInAFence(t *testing.T) {
	doc := "the environment:\n\n" +
		"```env\n" +
		"DATABASE_PASSWORD=\"correct horse battery staple\"\n" +
		"STRIPE_SECRET=" + secretStripe + "\n" +
		"SESSION_TOKEN=hunter2xyz\n" +
		"```\n"
	items := parseMarkdown(parseOptions{}, "x.md", []byte(doc))
	if len(items) == 0 {
		t.Fatal("the file gave no items")
	}
	joined := ""
	for _, it := range items {
		joined += it.Text + "\n"
	}
	for _, secret := range []string{"hunter2xyz", secretStripe, "correct horse battery staple"} {
		if strings.Contains(joined, secret) {
			t.Errorf("the value %q is still there: %q", secret, joined)
		}
	}
	for _, name := range []string{"DATABASE_PASSWORD=", "STRIPE_SECRET=", "SESSION_TOKEN="} {
		if !strings.Contains(joined, name) {
			t.Errorf("the name %q is gone: %q", name, joined)
		}
	}
}

// TestRedactASecretInTheFrontmatter checks a secret in the frontmatter: it
// is gone before the description is used as a heading.
func TestRedactASecretInTheFrontmatter(t *testing.T) {
	doc := "---\n" +
		"name: a_rule\n" +
		"description: The rule about GITHUB_TOKEN=ghp_" + strings.Repeat("Q", 36) + "\n" +
		"metadata:\n" +
		"  type: feedback\n" +
		"---\n\n" +
		"# Heading\n\n" +
		"- one item\n"
	items := parseMarkdown(parseOptions{kindFromType: true}, "x.md", []byte(doc))
	if len(items) != 1 {
		t.Fatalf("the file gave %d items, want 1: %+v", len(items), items)
	}
	if strings.Contains(items[0].Heading, "ghp_") {
		t.Errorf("the secret is still in the heading: %q", items[0].Heading)
	}
	if !strings.Contains(items[0].Heading, Redacted) {
		t.Errorf("the heading is not redacted: %q", items[0].Heading)
	}
	if !items[0].Redacted || items[0].Redactions == 0 {
		t.Errorf("the item is not marked redacted: %+v", items[0])
	}
	if items[0].Kind != KindLesson {
		t.Errorf("the item has the kind %q", items[0].Kind)
	}
}

// TestTheWholeOutputHoldsNoPlantedSecret runs the scan over every fixture,
// writes all the items out as JSON and checks that no planted secret is in
// it.
func TestTheWholeOutputHoldsNoPlantedSecret(t *testing.T) {
	for _, id := range fixtureAdapters {
		values, ok := plantedSecrets[id]
		if !ok || len(values) == 0 {
			t.Errorf("the %q fixture has no planted secret listed", id)
			continue
		}
		// Every listed value is really in the fixture: the list does not
		// drift away from the files.
		home, project := fixture(t, id)
		for _, value := range values {
			found := false
			for _, dir := range []string{home, project} {
				err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
					if err != nil || info.IsDir() {
						return nil
					}
					data, err := os.ReadFile(path)
					if err != nil {
						return nil
					}
					if strings.Contains(string(data), value) {
						found = true
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			if !found {
				t.Errorf("the %q fixture does not hold %q any more", id, value)
			}
		}
		res := scanOf(t, id)
		out, err := json.Marshal(res.Items)
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range values {
			if strings.Contains(string(out), value) {
				t.Errorf("adapter %q still shows the planted secret %q", id, value)
			}
		}
	}
}

// TestOverRedactionStaysBounded checks that the harmless things a file says
// are not touched.
func TestOverRedactionStaysBounded(t *testing.T) {
	harmless := []string{
		"use 4 spaces for indentation",
		"the build takes 12 minutes",
		"5551234567",
		"the port is 4321 and the timeout is 30 seconds",
		"the release is 1.24.0 and the build number is 1234",
	}
	for _, text := range harmless {
		out, n := Redact(text)
		if n != 0 {
			t.Errorf("%q was redacted: %q", text, out)
		}
		if out != text {
			t.Errorf("%q was changed: %q", text, out)
		}
	}
}
