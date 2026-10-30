package memoryupload

import (
	"strings"
	"testing"

	"github.com/OpenVibers/OpenVibe.Node/internal/memory"
)

// TestSlugIsTheFolderName checks the name a project is known by.
func TestSlugIsTheFolderName(t *testing.T) {
	cases := map[string]string{
		"/home/you/work/My Project": "my-project",
		"/home/you/work/demo":       "demo",
		"/home/you/work/Ünïcode":    "n-code",
		"/home/you/work/":           "work",
		"/":                         "project",
		"/home/you/....":            "project",
	}
	for path, want := range cases {
		if got := Slug(path); got != want {
			t.Errorf("the slug of %q is %q, want %q", path, got, want)
		}
	}
}

// TestSlugFitsTheStore checks that a slug is what the store accepts: it
// starts with a letter or a digit and is not longer than 64 characters.
func TestSlugFitsTheStore(t *testing.T) {
	long := strings.Repeat("a", 200)
	slug := Slug("/home/you/" + long)
	if len(slug) > MaxSlug {
		t.Errorf("the slug is %d characters long, want at most %d", len(slug), MaxSlug)
	}
	for _, r := range slug {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_') {
			t.Errorf("the slug holds %q, want only letters, digits, dots, dashes and underscores", r)
		}
	}
	if slug[0] == '.' || slug[0] == '-' || slug[0] == '_' {
		t.Errorf("the slug starts with %q, want a letter or a digit", slug[0])
	}
}

// result is a small scan with a user item, a project item and an empty one.
func result() memory.Result {
	return memory.Result{
		Home:     "/home/you",
		Projects: []string{"/home/you/work/My Project"},
		Adapters: []memory.AdapterReport{
			{ID: "claude-code", Name: "Claude Code", Items: 3, Files: []memory.FileReport{
				{Path: "/home/you/.claude/CLAUDE.md", Found: true, Items: 2},
				{Path: "/home/you/work/My Project/CLAUDE.md", Found: true, Items: 1},
			}},
		},
		Items: []memory.Item{
			{Text: "Prefer tabs", Kind: memory.KindPreference, Scope: memory.ScopeUser, Source: "claude-code",
				Path: "/home/you/.claude/CLAUDE.md", Heading: "Setup"},
			{Text: "The build takes 12 minutes", Kind: memory.KindProjectRule, Scope: memory.ScopeProject("/home/you/work/My Project"),
				Source: "claude-code", Path: "/home/you/work/My Project/CLAUDE.md", Heading: "Build"},
			{Text: "   ", Kind: memory.KindFact, Scope: memory.ScopeUser, Source: "claude-code", Path: "/home/you/.claude/CLAUDE.md"},
		},
	}
}

// TestBuildMakesTheScopes checks the scopes of the plan: the person, and a
// project by the slug of its folder name.
func TestBuildMakesTheScopes(t *testing.T) {
	plan := Build(result())
	if len(plan.Items) != 2 {
		t.Fatalf("the plan holds %d items, want 2 (the empty one stays behind)", len(plan.Items))
	}
	if plan.Items[0].Scope != "user" {
		t.Errorf("the first item has the scope %q, want user", plan.Items[0].Scope)
	}
	if plan.Items[1].Scope != "project:my-project" {
		t.Errorf("the second item has the scope %q, want project:my-project", plan.Items[1].Scope)
	}
	if plan.Summary.Skipped != 1 {
		t.Errorf("the plan skipped %d items, want 1", plan.Summary.Skipped)
	}
}

// TestBuildNeverHoldsAnAbsolutePath checks that no path of the machine is in
// the plan: the paths are relative to the folder they were scanned from.
func TestBuildNeverHoldsAnAbsolutePath(t *testing.T) {
	plan := Build(result())
	if plan.Items[0].SourceRef == nil || plan.Items[0].SourceRef.Path != ".claude/CLAUDE.md" {
		t.Errorf("the first item has the path %+v, want .claude/CLAUDE.md", plan.Items[0].SourceRef)
	}
	if plan.Items[1].SourceRef == nil || plan.Items[1].SourceRef.Path != "CLAUDE.md" {
		t.Errorf("the second item has the path %+v, want CLAUDE.md", plan.Items[1].SourceRef)
	}
	if plan.Items[0].SourceRef.Heading != "Setup" {
		t.Errorf("the first item has the heading %q, want Setup", plan.Items[0].SourceRef.Heading)
	}
	for _, item := range plan.Items {
		if strings.Contains(item.SourceRef.Path, "/home/") {
			t.Errorf("the path %q holds the home folder", item.SourceRef.Path)
		}
	}
}

// TestBuildCountsWhatWasRedacted checks the counts of the summary.
func TestBuildCountsWhatWasRedacted(t *testing.T) {
	res := result()
	res.Items[0].Redacted = true
	res.Items[0].Redactions = 2
	plan := Build(res)
	if plan.Summary.Redacted != 1 {
		t.Errorf("the summary counts %d redacted items, want 1", plan.Summary.Redacted)
	}
	if plan.Summary.Redactions != 2 {
		t.Errorf("the summary counts %d secrets, want 2", plan.Summary.Redactions)
	}
	if len(plan.Summary.Tools) != 1 || plan.Summary.Tools[0].Redacted != 1 {
		t.Errorf("the tool of the summary counts %+v, want one redacted item", plan.Summary.Tools)
	}
	if plan.Summary.Items != 2 {
		t.Errorf("the summary counts %d items to send, want 2", plan.Summary.Items)
	}
}

// TestBuildMarksWhatAToolWrote checks that the items a coding tool wrote are
// marked as written by an AI, so the store keeps them as suggestions.
func TestBuildMarksWhatAToolWrote(t *testing.T) {
	plan := Build(result())
	for _, item := range plan.Items {
		if !item.NoLooks {
			t.Errorf("the item %q is not marked as written by a tool", item.Text)
		}
		if item.Confidence != 1 {
			t.Errorf("the item %q has the confidence %v, want 1", item.Text, item.Confidence)
		}
		if item.Source != "claude-code" {
			t.Errorf("the item %q has the source %q, want the adapter id", item.Text, item.Source)
		}
	}
}

// TestBuildSortsTheScopes checks that the scopes of the summary are sorted.
func TestBuildSortsTheScopes(t *testing.T) {
	plan := Build(result())
	want := []string{"project:my-project", "user"}
	if len(plan.Summary.Scopes) != len(want) {
		t.Fatalf("the summary holds the scopes %v, want %v", plan.Summary.Scopes, want)
	}
	for i, scope := range want {
		if plan.Summary.Scopes[i] != scope {
			t.Errorf("the scopes are %v, want %v", plan.Summary.Scopes, want)
		}
	}
}
