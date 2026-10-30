// Package memory reads the memory files of the coding tools on this machine and
// turns them into memory items.
//
// It is read-only and local: it opens the files it knows about, parses them and
// redacts secrets before anything is printed. It never writes a file, never
// opens a network connection and never follows a symlink out of the path it was
// given.
package memory

// Kind is what a memory item is about.
type Kind string

const (
	// KindPreference is how the person wants things done.
	KindPreference Kind = "preference"
	// KindFact is a fact about the person or the project.
	KindFact Kind = "fact"
	// KindProjectRule is a rule that holds in a project.
	KindProjectRule Kind = "project_rule"
	// KindProcedure is a way of doing something.
	KindProcedure Kind = "procedure"
	// KindDecision is a decision that was taken, with its reason.
	KindDecision Kind = "decision"
	// KindLesson is something that was learned the hard way.
	KindLesson Kind = "lesson"
)

// Scope is where an item applies: the person (ScopeUser) or a project
// (ScopeProject + the absolute path of the project).
type Scope string

// ScopeUser is the scope of an item that holds for the person everywhere.
const ScopeUser Scope = "user"

// ScopeProject returns the scope of an item that holds in a project.
func ScopeProject(absProjectPath string) Scope {
	return Scope("project:" + absProjectPath)
}

// Item is one piece of memory found in a file.
type Item struct {
	// Text is the item, already redacted.
	Text string `json:"text"`
	// Kind is what the item is about.
	Kind Kind `json:"kind"`
	// Scope is where the item applies.
	Scope Scope `json:"scope"`
	// Source is the id of the adapter that found the item.
	Source string `json:"source"`
	// Path is the file the item came from.
	Path string `json:"path"`
	// Updated is the mtime of that file, RFC 3339.
	Updated string `json:"updated"`
	// Heading is the section the item came from, when the file had one.
	Heading string `json:"heading,omitempty"`
	// Redacted is true when the text held a secret that was removed.
	Redacted bool `json:"redacted,omitempty"`
	// Redactions is how many secrets were removed from the item.
	Redactions int `json:"redactions,omitempty"`
	// Truncated is true when the item was longer than MaxItemRunes.
	Truncated bool `json:"truncated,omitempty"`
}

// Adapter is one source of memory: a tool, where it keeps its memory and how
// to read it.
type Adapter struct {
	// ID is the stable name of the adapter, as used by --tools.
	ID string
	// Name is how the tool is called.
	Name string
	// Docs is the URL of the tool's own documentation for these paths.
	Docs string
	// Locations returns the exact known paths and the narrow globs to look at,
	// for a home directory and the project directories the person gave.
	// It never returns the home directory itself and never a glob wider than
	// the tool's own folders.
	Locations func(home string, projects []string) []string
	// Parse turns one file into items. The items it returns are redacted.
	Parse func(path string, data []byte) []Item
}
