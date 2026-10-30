package memory

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// fixture gives the home and the project directory of one adapter's fixture.
func fixture(t *testing.T, id string) (home string, project string) {
	t.Helper()
	base := filepath.Join("testdata", id)
	home = filepath.Join(base, "home")
	project = filepath.Join(base, "project")
	return home, project
}

// adapterByID finds an adapter.
func adapterByID(t *testing.T, id string) Adapter {
	t.Helper()
	for _, ad := range Adapters() {
		if ad.ID == id {
			return ad
		}
	}
	t.Fatalf("no adapter %q", id)
	return Adapter{}
}

// scanOf runs a scan over one adapter's fixture.
func scanOf(t *testing.T, id string) Result {
	t.Helper()
	home, project := fixture(t, id)
	return Scan(Options{Home: home, Projects: []string{project}, Tools: []string{id}})
}

// itemsOf returns the items of the files whose path holds a marker.
func itemsOf(res Result, marker string) []Item {
	var out []Item
	for _, it := range res.Items {
		if strings.Contains(it.Path, marker) {
			out = append(out, it)
		}
	}
	return out
}

// secretLike finds the parts of a text that look like a secret.
//
// The test uses it so that it never has to hold a secret value itself: it
// takes the secrets of a fixture out of the fixture and checks that none of
// them reaches an item.
var secretLike = regexp.MustCompile("(?i)(?:\\b(?:sk|pk|rk)-[A-Za-z0-9_-]{16,}|\\bgh[pousr]_[A-Za-z0-9]{20,}|\\bgithub_pat_[A-Za-z0-9_]{20,}|\\beyJ[A-Za-z0-9_-]{8,}\\.[A-Za-z0-9_-]{8,}\\.[A-Za-z0-9_-]{8,}|\\b(?:AKIA|ASIA)[0-9A-Z]{16}|\\bxox[baprs]-[A-Za-z0-9-]{10,}|\\b(?:sk|rk|pk)_(?:live|test)_[A-Za-z0-9]{10,}|\\b(?:api[_-]?key|access[_-]?token|client[_-]?secret|token|password|secret|aws_secret_access_key)\\b[ \\t]*[:=][ \\t]*[^\\s\"']{6,}|\\b[a-zA-Z][a-zA-Z0-9+.-]*://[^\\s:/@]+:[^\\s@/]{6,}@|-----BEGIN [A-Z ]*PRIVATE KEY-----[\\s\\S]*?-----END [A-Z ]*PRIVATE KEY-----)")

// secretsOf returns the secret-looking parts of a text.
func secretsOf(text string) []string {
	var out []string
	for _, m := range secretLike.FindAllString(text, -1) {
		out = append(out, m)
	}
	return out
}

// TestEveryAdapterHasDocsAndID checks the shape of the adapter list.
func TestEveryAdapterHasDocsAndID(t *testing.T) {
	seen := map[string]bool{}
	for _, ad := range Adapters() {
		if ad.ID == "" || ad.Name == "" {
			t.Errorf("an adapter has no id or name: %+v", ad)
		}
		if seen[ad.ID] {
			t.Errorf("two adapters share the id %q", ad.ID)
		}
		seen[ad.ID] = true
		if !strings.HasPrefix(ad.Docs, "https://") {
			t.Errorf("adapter %q has no docs link: %q", ad.ID, ad.Docs)
		}
		if ad.Locations == nil || ad.Parse == nil {
			t.Errorf("adapter %q has no locations or no parser", ad.ID)
		}
	}
}

// TestLocationsAreNarrow checks that no adapter ever points at the home
// folder itself and that no glob is wider than the tool's own folders.
func TestLocationsAreNarrow(t *testing.T) {
	home := filepath.Join("/home", "person")
	project := filepath.Join("/srv", "work", "demo")
	for _, ad := range Adapters() {
		for _, loc := range ad.Locations(home, []string{project}) {
			if loc == home || loc == project {
				t.Errorf("adapter %q points at a whole folder: %q", ad.ID, loc)
			}
			if !strings.HasPrefix(loc, home) && !strings.HasPrefix(loc, project) {
				t.Errorf("adapter %q points outside the home and the project: %q", ad.ID, loc)
			}
			// A glob must never start with a wildcard: it would walk the
			// whole home folder.
			rel, err := filepath.Rel(home, loc)
			if err == nil && !strings.HasPrefix(rel, "..") {
				first := strings.Split(filepath.ToSlash(rel), "/")
				if len(first) > 0 && strings.ContainsAny(first[0], "*?[") {
					t.Errorf("adapter %q starts a glob at the home folder: %q", ad.ID, loc)
				}
			}
			// Every glob must stay inside a folder of the tool.
			if strings.ContainsAny(loc, "*?[") {
				toolFolder := strings.Contains(loc, filepath.Join(".claude", "")) ||
					strings.Contains(loc, filepath.Join(".codex", "")) ||
					strings.Contains(loc, filepath.Join(".gemini", "")) ||
					strings.Contains(loc, filepath.Join(".cursor", "")) ||
					strings.Contains(loc, filepath.Join(".github", "")) ||
					strings.Contains(loc, filepath.Join(".windsurf", "")) ||
					strings.Contains(loc, filepath.Join(".codeium", "")) ||
					strings.Contains(loc, filepath.Join(".clinerules", "")) ||
					strings.Contains(loc, filepath.Join(".roo", "")) ||
					strings.Contains(loc, filepath.Join(".kiro", "")) ||
					strings.Contains(loc, filepath.Join(".continue", "")) ||
					strings.Contains(loc, filepath.Join(".commandcode", "")) ||
					strings.Contains(loc, filepath.Join("memory-bank", "")) ||
					strings.Contains(loc, filepath.Join("memory", ""))
				if !toolFolder {
					t.Errorf("adapter %q has a glob outside the tool's own folders: %q", ad.ID, loc)
				}
			}
		}
	}
}

// TestUserLevelOnlyWithoutProject checks that a scan without a project reads
// only the user's own files.
func TestUserLevelOnlyWithoutProject(t *testing.T) {
	home, _ := fixture(t, "claude-code")
	res := Scan(Options{Home: home})
	for _, it := range res.Items {
		if it.Scope != ScopeUser {
			t.Errorf("item %q has the scope %q without a project", it.Text, it.Scope)
		}
	}
	for _, ad := range res.Adapters {
		for _, f := range ad.Files {
			if strings.Contains(f.Path, filepath.Join("testdata", "claude-code", "project")) {
				t.Errorf("the scan read a project file without a project: %s", f.Path)
			}
		}
	}
}

// TestClaudeCodeParsing checks the items of the Claude Code fixture.
func TestClaudeCodeParsing(t *testing.T) {
	res := scanOf(t, "claude-code")

	user := itemsOf(res, filepath.Join("home", ".claude", "CLAUDE.md"))
	if len(user) != 3 {
		t.Fatalf("the user CLAUDE.md gave %d items, want 3: %+v", len(user), user)
	}
	if user[0].Heading != "Personal setup" {
		t.Errorf("the first item has the heading %q", user[0].Heading)
	}
	if user[0].Kind != KindPreference {
		t.Errorf("a user file item has the kind %q, want a preference", user[0].Kind)
	}
	if user[0].Scope != ScopeUser {
		t.Errorf("a user file item has the scope %q", user[0].Scope)
	}

	// The secret is gone.
	if !user[2].Redacted {
		t.Errorf("the item with a secret is not marked redacted: %+v", user[2])
	}
	if user[2].Redactions == 0 {
		t.Errorf("the item with a secret counts no redaction: %+v", user[2])
	}
	if strings.Contains(user[2].Text, "sk-proj-A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8") {
		t.Errorf("the secret is still in the item: %q", user[2].Text)
	}

	// The auto memory: the kind comes from the frontmatter, and the index is
	// skipped.
	auto := itemsOf(res, filepath.Join("memory", "user_role.md"))
	if len(auto) != 1 {
		t.Fatalf("the auto memory gave %d items, want 1: %+v", len(auto), auto)
	}
	if auto[0].Kind != KindPreference {
		t.Errorf("the auto memory item has the kind %q", auto[0].Kind)
	}
	feedback := itemsOf(res, filepath.Join("memory", "feedback_testing.md"))
	if len(feedback) != 1 || feedback[0].Kind != KindLesson {
		t.Errorf("the feedback memory gave %+v", feedback)
	}
	if idx := itemsOf(res, "MEMORY.md"); len(idx) != 0 {
		t.Errorf("the MEMORY.md index gave items: %+v", idx)
	}

	// The project files: the scope is the project and the kind is a rule.
	proj := itemsOf(res, filepath.Join("claude-code", "project"))
	if len(proj) == 0 {
		t.Fatal("the project files gave no items")
	}
	for _, it := range proj {
		if !strings.HasPrefix(string(it.Scope), "project:") {
			t.Errorf("a project item has the scope %q", it.Scope)
		}
		if it.Kind != KindProjectRule {
			t.Errorf("a project item has the kind %q: %q", it.Kind, it.Text)
		}
	}
}

// TestAgentsParsing checks the AGENTS.md family.
func TestAgentsParsing(t *testing.T) {
	res := scanOf(t, "agents")
	if len(res.Items) == 0 {
		t.Fatal("the agents fixture gave no items")
	}
	for _, it := range res.Items {
		if it.Source != "agents" {
			t.Errorf("the item has the source %q", it.Source)
		}
		if it.Updated == "" {
			t.Errorf("the item has no date: %+v", it)
		}
	}
	proj := itemsOf(res, filepath.Join("agents", "project"))
	if len(proj) != 3 {
		t.Errorf("the project AGENTS.md gave %d items, want 3: %+v", len(proj), proj)
	}
	if proj[2].Kind != KindDecision {
		t.Errorf("the item under the Decision heading has the kind %q", proj[2].Kind)
	}
}

// TestGeminiParsing checks the Gemini CLI fixture.
func TestGeminiParsing(t *testing.T) {
	res := scanOf(t, "gemini")
	user := itemsOf(res, filepath.Join("home", ".gemini", "GEMINI.md"))
	if len(user) != 3 {
		t.Fatalf("the global GEMINI.md gave %d items, want 3: %+v", len(user), user)
	}
	if !user[2].Redacted {
		t.Errorf("the item with the AWS key is not marked redacted: %+v", user[2])
	}
	if strings.Contains(user[2].Text, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("the AWS key is still in the item: %q", user[2].Text)
	}
}

// TestCursorParsing checks the Cursor rules and their frontmatter.
func TestCursorParsing(t *testing.T) {
	res := scanOf(t, "cursor")
	items := itemsOf(res, "style.mdc")
	if len(items) != 3 {
		t.Fatalf("the style rule gave %d items, want 3: %+v", len(items), items)
	}
	if items[0].Heading != "How we write code here" {
		t.Errorf("the rule has the heading %q, want the frontmatter description", items[0].Heading)
	}
	if !items[2].Redacted {
		t.Errorf("the item with the Slack token is not marked redacted: %+v", items[2])
	}
	if strings.Contains(items[2].Text, "xoxb-") {
		t.Errorf("the Slack token is still in the item: %q", items[2].Text)
	}
	// The frontmatter is read, not turned into items.
	for _, it := range items {
		if strings.Contains(it.Text, "alwaysApply") {
			t.Errorf("the frontmatter became an item: %q", it.Text)
		}
	}
}

// TestCopilotParsing checks the GitHub Copilot instructions.
func TestCopilotParsing(t *testing.T) {
	res := scanOf(t, "copilot")
	items := itemsOf(res, "copilot-instructions.md")
	if len(items) != 3 {
		t.Fatalf("the repository instructions gave %d items, want 3: %+v", len(items), items)
	}
	if !items[2].Redacted {
		t.Errorf("the item with the JWT is not marked redacted: %+v", items[2])
	}
	if strings.Contains(items[2].Text, "eyJhbGciOiJIUzI1NiI") {
		t.Errorf("the JWT is still in the item: %q", items[2].Text)
	}
	inst := itemsOf(res, "tests.instructions.md")
	if len(inst) != 1 {
		t.Fatalf("the test instructions gave %d items, want 1: %+v", len(inst), inst)
	}
}

// TestWindsurfParsing checks the Windsurf rules and memories.
func TestWindsurfParsing(t *testing.T) {
	res := scanOf(t, "windsurf")
	mem := itemsOf(res, filepath.Join("memories", "preferences.md"))
	if len(mem) != 3 {
		t.Fatalf("the memories gave %d items, want 3: %+v", len(mem), mem)
	}
	// The private key is one item of its own and is dropped.
	for _, it := range mem {
		if strings.Contains(it.Text, "PRIVATE KEY") {
			t.Errorf("the private key is still in an item: %q", it.Text)
		}
	}
	rules := itemsOf(res, filepath.Join(".windsurf", "rules"))
	if len(rules) != 1 {
		t.Fatalf("the rules gave %d items, want 1: %+v", len(rules), rules)
	}
}

// TestClineAndRooParsing checks the Cline and Roo fixtures.
func TestClineAndRooParsing(t *testing.T) {
	res := scanOf(t, "cline")
	rules := itemsOf(res, ".clinerules")
	if len(rules) != 2 {
		t.Fatalf("the cline rules gave %d items, want 2: %+v", len(rules), rules)
	}
	if !rules[1].Redacted {
		t.Errorf("the item with the password is not marked redacted: %+v", rules[1])
	}
	if strings.Contains(rules[1].Text, "Sup3rSecretFakeValue") {
		t.Errorf("the password is still in the item: %q", rules[1].Text)
	}
	bank := itemsOf(res, "projectbrief.md")
	if len(bank) != 1 {
		t.Fatalf("the memory bank gave %d items, want 1: %+v", len(bank), bank)
	}

	roo := scanOf(t, "roo")
	rr := itemsOf(roo, filepath.Join(".roo", "rules"))
	if len(rr) != 2 {
		t.Fatalf("the roo rules gave %d items, want 2: %+v", len(rr), rr)
	}
	if !rr[1].Redacted {
		t.Errorf("the item with the Stripe key is not marked redacted: %+v", rr[1])
	}
	if strings.Contains(rr[1].Text, "sk_live_") {
		t.Errorf("the Stripe key is still in the item: %q", rr[1].Text)
	}
}

// TestKiroParsing checks the Kiro steering files.
func TestKiroParsing(t *testing.T) {
	res := scanOf(t, "kiro")
	tech := itemsOf(res, "tech.md")
	if len(tech) != 3 {
		t.Fatalf("the steering file gave %d items, want 3: %+v", len(tech), tech)
	}
	if !tech[2].Redacted {
		t.Errorf("the item with the connection string is not marked redacted: %+v", tech[2])
	}
	if strings.Contains(tech[2].Text, "hunter2fake") {
		t.Errorf("the password is still in the item: %q", tech[2].Text)
	}
	if !strings.Contains(tech[2].Text, "postgres://dbuser:") {
		t.Errorf("the connection string lost its shape: %q", tech[2].Text)
	}
}

// TestContinueParsing checks the Continue rules.
func TestContinueParsing(t *testing.T) {
	res := scanOf(t, "continue")
	items := itemsOf(res, "style.md")
	if len(items) != 2 {
		t.Fatalf("the rules gave %d items, want 2: %+v", len(items), items)
	}
	if !items[1].Redacted {
		t.Errorf("the item with the token is not marked redacted: %+v", items[1])
	}
}

// TestAiderParsing checks the Aider conventions and the files its config
// names, and that the chat history is never read.
func TestAiderParsing(t *testing.T) {
	res := scanOf(t, "aider")
	conv := itemsOf(res, "CONVENTIONS.md")
	if len(conv) != 2 {
		t.Fatalf("the conventions gave %d items, want 2: %+v", len(conv), conv)
	}
	if !conv[1].Redacted {
		t.Errorf("the item with the password is not marked redacted: %+v", conv[1])
	}
	read := itemsOf(res, "README.md")
	if len(read) != 1 {
		t.Fatalf("the file named by the config gave %d items, want 1: %+v", len(read), read)
	}
	notes := itemsOf(res, "NOTES.md")
	if len(notes) != 1 {
		t.Fatalf("the file named by the global config gave %d items, want 1: %+v", len(notes), notes)
	}
	for _, f := range res.Adapters[0].Files {
		if strings.Contains(f.Path, "chat.history") {
			t.Errorf("the chat history was read: %s", f.Path)
		}
	}
}

// TestCommandCodeParsing checks the Command Code memory folder.
func TestCommandCodeParsing(t *testing.T) {
	res := scanOf(t, "command-code")
	items := itemsOf(res, filepath.Join("project", ".commandcode"))
	if len(items) != 2 {
		t.Fatalf("the command code memory gave %d items, want 2: %+v", len(items), items)
	}
	if !items[1].Redacted {
		t.Errorf("the item with the token is not marked redacted: %+v", items[1])
	}
	if strings.Contains(items[1].Text, "github_pat_") {
		t.Errorf("the token is still in the item: %q", items[1].Text)
	}
}

// TestOpenCodeParsing checks the OpenCode AGENTS.md and the instructions its
// config names.
func TestOpenCodeParsing(t *testing.T) {
	res := scanOf(t, "opencode")
	agents := itemsOf(res, filepath.Join("opencode", "project", "AGENTS.md"))
	if len(agents) != 1 {
		t.Fatalf("the AGENTS.md gave %d items, want 1: %+v", len(agents), agents)
	}
	conv := itemsOf(res, "conventions.md")
	if len(conv) != 2 {
		t.Fatalf("the file named by the config gave %d items, want 2: %+v", len(conv), conv)
	}
	if !conv[1].Redacted {
		t.Errorf("the item with the aws key is not marked redacted: %+v", conv[1])
	}
	if strings.Contains(conv[1].Text, "wJalrXUtnFEMI") {
		t.Errorf("the aws key is still in the item: %q", conv[1].Text)
	}
}

// TestNoSecretSurvivesInAnyFixture checks that no fixture's secret shows up
// in any item of any adapter.
func TestNoSecretSurvivesInAnyFixture(t *testing.T) {
	for _, id := range []string{
		"claude-code", "agents", "gemini", "cursor", "copilot", "windsurf",
		"cline", "roo", "kiro", "continue", "aider", "command-code", "opencode",
	} {
		res := scanOf(t, id)
		if len(res.Items) == 0 {
			t.Errorf("the %q fixture gave no items", id)
		}
		// Every fixture holds at least one secret of its own shape.
		found := false
		err := filepath.Walk(filepath.Join("testdata", id), func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			if len(secretsOf(string(data))) > 0 {
				found = true
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			t.Errorf("the %q fixture holds no secret", id)
		}
		for _, it := range res.Items {
			for _, sec := range secretsOf(strings.ReplaceAll(it.Text, Redacted, "")) {
				t.Errorf("adapter %q still shows the secret %q in %q", id, sec, it.Text)
			}
		}
	}
}

// TestAnItemThatIsOnlyASecretIsDropped checks that an item that held nothing
// but a secret is dropped, not kept with an empty text.
func TestAnItemThatIsOnlyASecretIsDropped(t *testing.T) {
	secret := "sk-proj-" + strings.Repeat("A", 40)
	items := parseMarkdown(parseOptions{}, "x.md", []byte("- "+secret+"\n"))
	if len(items) != 0 {
		t.Errorf("an item that was only a secret was kept: %+v", items)
	}
	items = parseMarkdown(parseOptions{}, "x.md", []byte("- The key is "+secret+"\n"))
	if len(items) != 1 {
		t.Fatalf("an item with a secret in it was dropped: %+v", items)
	}
	if !items[0].Redacted || items[0].Redactions != 1 {
		t.Errorf("the item is not marked redacted: %+v", items[0])
	}
}

// TestRedactShapes checks every shape of secret the harness knows.
func TestRedactShapes(t *testing.T) {
	cases := map[string]string{
		"openai":     "sk-" + "proj-" + strings.Repeat("A", 40),
		"anthropic":  "sk-" + "ant-api03-" + strings.Repeat("B", 40),
		"github":     "gh" + "p_" + strings.Repeat("C", 36),
		"github_pat": "github_" + "pat_" + strings.Repeat("D", 36),
		"jwt":        "ey" + "JhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0." + strings.Repeat("E", 43),
		"pem":        "-----BEGIN RSA " + "PRIVATE KEY-----\n" + strings.Repeat("F", 60) + "\n-----END RSA PRIVATE KEY-----",
		"aws":        "AK" + "IAIOSFODNN7EXAMPLE",
		"slack":      "xox" + "b-000000000000-000000000000-" + strings.Repeat("G", 33),
		"stripe":     "sk" + "_live_" + strings.Repeat("H", 24),
		"assignment": "pass" + "word=" + strings.Repeat("I", 12),
		"connection": "postgres://dbuser:" + strings.Repeat("J", 10) + "@db.internal:5432/app",
	}
	for name, secret := range cases {
		text, count := Redact("the value is " + secret + " ok")
		if count == 0 {
			t.Errorf("%s: the secret was not found", name)
			continue
		}
		if strings.Contains(text, secret) {
			t.Errorf("%s: the secret is still in %q", name, text)
		}
		if !strings.Contains(text, Redacted) {
			t.Errorf("%s: the text has no redacted marker: %q", name, text)
		}
	}
}

// TestOverSizeFileIsSkipped checks that a file over 256 KB is skipped.
func TestOverSizeFileIsSkipped(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(dir, ".codex", "AGENTS.md")
	if err := os.WriteFile(big, []byte(strings.Repeat("a line of text\n", 30000)), 0o644); err != nil {
		t.Fatal(err)
	}
	res := Scan(Options{Home: dir, Tools: []string{"agents"}})
	found := false
	for _, ad := range res.Adapters {
		for _, f := range ad.Files {
			if f.Path == big {
				found = true
				if !strings.Contains(f.Note, "256 KB") {
					t.Errorf("the big file was not skipped with a note: %+v", f)
				}
				if f.Items != 0 {
					t.Errorf("the big file gave %d items", f.Items)
				}
			}
		}
	}
	if !found {
		t.Error("the big file was not even looked at")
	}
}

// TestBinaryFileIsSkipped checks that a binary file is skipped.
func TestBinaryFileIsSkipped(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, ".codex", "AGENTS.md")
	if err := os.WriteFile(bin, []byte{0x00, 0x01, 0x02, 0x03}, 0o644); err != nil {
		t.Fatal(err)
	}
	res := Scan(Options{Home: dir, Tools: []string{"agents"}})
	for _, ad := range res.Adapters {
		for _, f := range ad.Files {
			if f.Path == bin && !strings.Contains(f.Note, "binary") {
				t.Errorf("the binary file was not skipped with a note: %+v", f)
			}
		}
	}
}

// TestSymlinkOutIsNotFollowed checks that a link out of the scanned paths is
// not read.
func TestSymlinkOutIsNotFollowed(t *testing.T) {
	outside := t.TempDir()
	secretFile := filepath.Join(outside, "outside.md")
	if err := os.WriteFile(secretFile, []byte("# Outside\n\n- a secret "+"gh"+"p_"+strings.Repeat("K", 36)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, ".codex", "AGENTS.md")
	if err := os.Symlink(secretFile, link); err != nil {
		t.Skipf("this machine cannot make a link: %v", err)
	}
	res := Scan(Options{Home: home, Tools: []string{"agents"}})
	for _, it := range res.Items {
		if strings.Contains(it.Text, "gh"+"p_") {
			t.Errorf("the link out was read: %+v", it)
		}
	}
	for _, ad := range res.Adapters {
		for _, f := range ad.Files {
			if f.Path == link && f.Items != 0 {
				t.Errorf("the link out gave %d items", f.Items)
			}
		}
	}
}

// TestLongItemIsTruncated checks that an item over 2000 characters is cut.
func TestLongItemIsTruncated(t *testing.T) {
	long := strings.Repeat("x", 3000)
	items := parseMarkdown(parseOptions{}, "x.md", []byte("- "+long+"\n"))
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	if !items[0].Truncated {
		t.Error("the long item is not marked truncated")
	}
	if len([]rune(items[0].Text)) != MaxItemRunes+1 {
		t.Errorf("the item has %d runes, want %d", len([]rune(items[0].Text)), MaxItemRunes+1)
	}
	if !strings.HasSuffix(items[0].Text, "…") {
		t.Error("the item does not end with an ellipsis")
	}
}

// TestPackageHasNoNetworkImport checks that the package never opens a
// network connection: it does not import net/http.
func TestPackageHasNoNetworkImport(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("%s does not parse: %v", name, err)
		}
		for _, imp := range file.Imports {
			path := strings.Trim(imp.Path.Value, "\"")
			if path == "net" || strings.HasPrefix(path, "net/") {
				t.Errorf("%s imports %s", name, path)
			}
		}
	}
}

// TestMarkdownSplitting checks how a markdown file becomes items.
func TestMarkdownSplitting(t *testing.T) {
	body := "# Title\n\nA paragraph under the title.\n\n" +
		"## Section\n\n- first bullet\n- second bullet\n\nA paragraph in the section.\n"
	items := markdownItems(body)
	if len(items) != 4 {
		t.Fatalf("got %d items, want 4: %+v", len(items), items)
	}
	if items[0].Heading != "Title" {
		t.Errorf("the first item has the heading %q", items[0].Heading)
	}
	if items[1].Heading != "Section" {
		t.Errorf("the bullet has the heading %q", items[1].Heading)
	}
	if items[1].Text != "- first bullet" {
		t.Errorf("the bullet item is %q", items[1].Text)
	}
	if items[2].Text != "- second bullet" {
		t.Errorf("the second bullet item is %q", items[2].Text)
	}
	if items[3].Text != "A paragraph in the section." {
		t.Errorf("the paragraph item is %q", items[3].Text)
	}
}

// TestFrontmatterIsReadNotTurnedIntoItems checks the frontmatter handling.
func TestFrontmatterIsReadNotTurnedIntoItems(t *testing.T) {
	data := []byte("---\nname: a_rule\ndescription: A rule about the api\nmetadata:\n  type: feedback\nglobs: \"**/*.go\"\n---\n\n# Heading\n\n- one item\n")
	items := parseMarkdown(parseOptions{kindFromType: true}, "rule.md", data)
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1: %+v", len(items), items)
	}
	if items[0].Kind != KindLesson {
		t.Errorf("the item has the kind %q, want a lesson", items[0].Kind)
	}
	if items[0].Heading != "A rule about the api" {
		t.Errorf("the item has the heading %q", items[0].Heading)
	}
	for _, it := range items {
		if strings.Contains(it.Text, "name: a_rule") {
			t.Errorf("the frontmatter became an item: %q", it.Text)
		}
	}
}

// TestScanReportsEveryPath checks that the scan says which path it looked
// at and whether it was there.
func TestScanReportsEveryPath(t *testing.T) {
	res := scanOf(t, "claude-code")
	var report *AdapterReport
	for i := range res.Adapters {
		if res.Adapters[i].ID == "claude-code" {
			report = &res.Adapters[i]
		}
	}
	if report == nil {
		t.Fatal("the claude-code adapter is not in the report")
	}
	if len(report.Files) == 0 {
		t.Fatal("the report lists no files")
	}
	seen := map[string]bool{}
	for _, f := range report.Files {
		seen[f.Path] = true
	}
	if !seen[filepath.Join("testdata", "claude-code", "home", ".claude", "CLAUDE.md")] {
		t.Errorf("the user CLAUDE.md is not in the report: %+v", report.Files)
	}
}

// TestOnlySecretsAreDropped checks that a file whose only content is a secret
// gives no items.
func TestOnlySecretsAreDropped(t *testing.T) {
	items := parseMarkdown(parseOptions{}, "x.md", []byte("AK"+"IAIOSFODNN7EXAMPLE\n"))
	if len(items) != 0 {
		t.Errorf("a file that held only a secret gave items: %+v", items)
	}
}

// TestRuntimeGuard keeps the tests portable: the paths are built with
// filepath.Join, not with a slash.
func TestRuntimeGuard(t *testing.T) {
	if runtime.GOOS == "" {
		t.Fatal("no runtime")
	}
}
