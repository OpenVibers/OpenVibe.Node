// Package memoryupload turns a memory scan into the items that go to the
// person's own memory store.
//
// The scan found the items on the machine; this package decides what may leave
// it. Only the text of an item, its kind, its scope, the adapter it came from
// and where it was written (relative to the scanned root) ever leave. The
// absolute path of the file, the name of the machine and the name of the
// person stay behind.
package memoryupload

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/OpenVibers/OpenVibe.Node/internal/memory"
	"github.com/OpenVibers/OpenVibe.Node/internal/memorystore"
)

// MaxSlug is the length of a project slug. The store takes at most 64.
const MaxSlug = 64

// FileSummary says what one file of a tool gave.
type FileSummary struct {
	// Path is the file, as the scan found it.
	Path string
	// Items is how many items it gave.
	Items int
	// Note says why the file was skipped, when it was.
	Note string
	// Found is true when the file was there.
	Found bool
}

// ToolSummary says what one tool gave.
type ToolSummary struct {
	// ID is the id of the adapter.
	ID string
	// Name is how the tool is called.
	Name string
	// Files are the files that were looked at.
	Files []FileSummary
	// Items is how many items the tool gave.
	Items int
	// Redacted is how many of them held a secret.
	Redacted int
	// Redactions is how many secrets were removed.
	Redactions int
}

// Summary is what the person is shown before anything is sent.
type Summary struct {
	// Tools are the tools that were read, in the order of the scan.
	Tools []ToolSummary
	// Scopes are the scopes the items go to, sorted.
	Scopes []string
	// Items is how many items will be sent.
	Items int
	// Redacted is how many items held a secret.
	Redacted int
	// Redactions is how many secrets were removed.
	Redactions int
	// Skipped is how many items stay behind because they are empty.
	Skipped int
}

// Plan is what will be sent, and what the person is shown first.
type Plan struct {
	// Items are the items to send, in the order of the scan.
	Items []memorystore.Item
	// Summary is what the person is shown.
	Summary Summary
}

// Build turns a scan into a plan.
//
// An item whose text is empty after the redaction never leaves the machine, so
// it is not in the plan and is counted as skipped. The scope of a project item
// is the slug of the project's folder name: never an absolute path.
func Build(res memory.Result) Plan {
	plan := Plan{Items: []memorystore.Item{}, Summary: Summary{Tools: []ToolSummary{}}}
	scopes := map[string]bool{}
	redactedBySource := map[string]int{}
	redactionsBySource := map[string]int{}

	for _, ad := range res.Adapters {
		ts := ToolSummary{ID: ad.ID, Name: ad.Name, Files: []FileSummary{}}
		for _, f := range ad.Files {
			ts.Files = append(ts.Files, FileSummary{Path: f.Path, Items: f.Items, Note: f.Note, Found: f.Found})
		}
		ts.Items = ad.Items
		plan.Summary.Tools = append(plan.Summary.Tools, ts)
	}

	for _, it := range res.Items {
		if it.Redacted {
			redactedBySource[it.Source]++
			redactionsBySource[it.Source] += it.Redactions
		}
		text := strings.TrimSpace(it.Text)
		if text == "" {
			plan.Summary.Skipped++
			continue
		}
		scope, root := scopeOf(it, res)
		scopes[scope] = true
		kind := string(it.Kind)
		if kind == "" {
			kind = string(memory.KindFact)
		}
		item := memorystore.Item{
			Scope:      scope,
			Kind:       kind,
			Text:       text,
			Source:     it.Source,
			Confidence: 1,
			// What a coding tool wrote is a suggestion until the person accepts it.
			NoLooks: true,
		}
		if ref := sourceRef(it, root); ref != nil {
			item.SourceRef = ref
		}
		plan.Items = append(plan.Items, item)
	}

	for i := range plan.Summary.Tools {
		plan.Summary.Tools[i].Redacted = redactedBySource[plan.Summary.Tools[i].ID]
		plan.Summary.Tools[i].Redactions = redactionsBySource[plan.Summary.Tools[i].ID]
	}
	plan.Summary.Items = len(plan.Items)
	for _, ts := range plan.Summary.Tools {
		plan.Summary.Redacted += ts.Redacted
		plan.Summary.Redactions += ts.Redactions
	}
	for scope := range scopes {
		plan.Summary.Scopes = append(plan.Summary.Scopes, scope)
	}
	sort.Strings(plan.Summary.Scopes)
	return plan
}

// scopeOf gives the scope an item goes to and the root its path is relative
// to: the home for the person, the project folder for a project.
func scopeOf(it memory.Item, res memory.Result) (string, string) {
	scope := string(it.Scope)
	if scope == "" || scope == string(memory.ScopeUser) {
		return string(memory.ScopeUser), res.Home
	}
	if abs, ok := strings.CutPrefix(scope, "project:"); ok {
		return "project:" + Slug(abs), abs
	}
	return string(memory.ScopeUser), res.Home
}

// sourceRef says where the item was written: the path relative to the scanned
// root and the heading it was under. A path that stays inside the root is
// never absolute.
func sourceRef(it memory.Item, root string) *memorystore.SourceRef {
	ref := &memorystore.SourceRef{Heading: it.Heading}
	if root != "" && it.Path != "" {
		if rel, err := filepath.Rel(root, it.Path); err == nil && !strings.HasPrefix(rel, "..") {
			ref.Path = filepath.ToSlash(rel)
		}
	}
	if ref.Path == "" && ref.Heading == "" {
		return nil
	}
	return ref
}

// Slug is the name a project is known by in the store: the folder's name,
// lowercased, with everything that is not a letter, a digit, a dot, a dash or
// an underscore turned into a dash. It is never an absolute path.
func Slug(absPath string) string {
	base := filepath.Base(filepath.Clean(absPath))
	var b strings.Builder
	for _, r := range strings.ToLower(base) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	slug := b.String()
	// The store wants a slug that starts with a letter or a digit.
	slug = strings.TrimLeft(slug, "._-")
	if slug == "" {
		return "project"
	}
	if len(slug) > MaxSlug {
		slug = slug[:MaxSlug]
		slug = strings.TrimRight(slug, "._-")
	}
	return slug
}
