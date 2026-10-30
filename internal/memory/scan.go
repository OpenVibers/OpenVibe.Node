package memory

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Limits of the scan.
const (
	// MaxFileBytes is the size a file may have before it is skipped.
	MaxFileBytes = 256 * 1024
	// MaxItemRunes is the length of an item before it is cut.
	MaxItemRunes = 2000
)

// Options say what to scan.
type Options struct {
	// Home is the home directory of the person whose memory is read.
	Home string
	// Projects are the project directories to read, in addition to the
	// user-level paths.
	Projects []string
	// Tools are the adapter ids to read. Empty means every adapter.
	Tools []string
}

// FileReport says what a scan found in one file.
type FileReport struct {
	// Path is the file that was looked at.
	Path string `json:"path"`
	// Found is true when the file was there.
	Found bool `json:"found"`
	// Items is how many items came out of the file.
	Items int `json:"items"`
	// Note says why a file was skipped, when it was.
	Note string `json:"note,omitempty"`
}

// AdapterReport says what a scan found for one tool.
type AdapterReport struct {
	ID    string       `json:"id"`
	Name  string       `json:"name"`
	Docs  string       `json:"docs"`
	Files []FileReport `json:"files"`
	Items int          `json:"items"`
}

// Result is what a scan found.
type Result struct {
	Home     string          `json:"home"`
	Projects []string        `json:"projects"`
	Adapters []AdapterReport `json:"adapters"`
	Items    []Item          `json:"items"`
}

// Scan reads the memory of the tools the person uses, locally and read-only.
//
// It never writes a file, never opens a network connection and never follows
// a symlink out of the home directory or the project directories it was given.
func Scan(opts Options) Result {
	res := Result{Home: opts.Home, Adapters: []AdapterReport{}, Items: []Item{}}
	projects := make([]string, 0, len(opts.Projects))
	for _, p := range opts.Projects {
		abs, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		projects = append(projects, abs)
	}
	sort.Strings(projects)
	res.Projects = projects

	want := map[string]bool{}
	for _, t := range opts.Tools {
		want[strings.ToLower(strings.TrimSpace(t))] = true
	}

	for _, ad := range Adapters() {
		if len(want) > 0 && !want[ad.ID] {
			continue
		}
		report := AdapterReport{ID: ad.ID, Name: ad.Name, Docs: ad.Docs, Files: []FileReport{}}
		for _, loc := range ad.Locations(opts.Home, projects) {
			paths := resolveLocation(loc, opts.Home, projects)
			if len(paths) == 0 && strings.ContainsAny(loc, "*?[") {
				// A glob that matched nothing is still a path we looked at.
				report.Files = append(report.Files, FileReport{Path: loc})
				continue
			}
			for _, path := range paths {
				fr := readFile(ad, path, opts.Home, projects, &res.Items)
				report.Files = append(report.Files, fr)
				report.Items += fr.Items
			}
		}
		res.Adapters = append(res.Adapters, report)
	}
	return res
}

// resolveLocation turns a location into the files it stands for: one file
// for an exact path, the matches of a glob for a pattern.
func resolveLocation(loc, home string, projects []string) []string {
	if !strings.ContainsAny(loc, "*?[") {
		return []string{loc}
	}
	matches, err := expandGlob(loc)
	if err != nil {
		return nil
	}
	allowed := allowedRoots(home, projects)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if !withinAny(m, allowed) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// allowedRoots are the directories a file may live in.
func allowedRoots(home string, projects []string) []string {
	roots := make([]string, 0, len(projects)+1)
	if home != "" {
		roots = append(roots, home)
	}
	roots = append(roots, projects...)
	return roots
}

// withinAny tells whether a path is inside one of the roots.
func withinAny(path string, roots []string) bool {
	for _, root := range roots {
		if root == "" {
			continue
		}
		if path == root || strings.HasPrefix(path, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// readFile reads one file and turns it into items.
func readFile(ad Adapter, path, home string, projects []string, items *[]Item) FileReport {
	fr := FileReport{Path: path}
	info, err := os.Lstat(path)
	if err != nil {
		return fr
	}
	if info.Mode()&os.ModeSymlink != 0 {
		// A link is only followed when it stays inside the paths we were given.
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || !withinAny(resolved, allowedRoots(home, projects)) {
			fr.Note = "skipped: link leaves the scanned paths"
			return fr
		}
		info, err = os.Stat(resolved)
		if err != nil {
			fr.Note = "skipped: link cannot be read"
			return fr
		}
		path = resolved
	}
	if info.IsDir() {
		fr.Note = "skipped: a directory"
		return fr
	}
	if info.Size() > MaxFileBytes {
		fr.Note = "skipped: larger than 256 KB"
		return fr
	}
	data, err := os.ReadFile(path)
	if err != nil {
		fr.Note = "skipped: cannot be read"
		return fr
	}
	if isBinary(data) {
		fr.Note = "skipped: a binary file"
		return fr
	}
	fr.Found = true
	for _, name := range indexFilesOf(ad) {
		if strings.EqualFold(filepath.Base(path), name) {
			fr.Note = "skipped: an index of the memories, not a memory"
			return fr
		}
	}
	updated := info.ModTime().UTC().Format(time.RFC3339)
	scope := scopeFor(path, projects)
	for _, it := range ad.Parse(path, data) {
		it.Source = ad.ID
		it.Path = path
		it.Updated = updated
		it.Scope = scope
		if it.Kind == "" {
			// A file of the person's own is about them; a file of a project
			// holds that project's rules.
			if scope == ScopeUser {
				it.Kind = KindPreference
			} else {
				it.Kind = KindProjectRule
			}
		}
		*items = append(*items, it)
		fr.Items++
	}
	return fr
}

// scopeFor gives the project a file belongs to, or the person when it belongs
// to none.
func scopeFor(path string, projects []string) Scope {
	for _, p := range projects {
		if path == p || strings.HasPrefix(path, p+string(filepath.Separator)) {
			return ScopeProject(p)
		}
	}
	return ScopeUser
}

// isBinary tells whether a file holds binary data.
func isBinary(data []byte) bool {
	head := data
	if len(head) > 8000 {
		head = head[:8000]
	}
	for _, b := range head {
		if b == 0 {
			return true
		}
	}
	return false
}
