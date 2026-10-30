package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// maxGlobDepth bounds how deep a glob walks below its fixed base directory.
const maxGlobDepth = 8

// expandGlob expands a narrow glob pattern.
//
// The pattern must start with a fixed directory: the part before the first
// wildcard is the base, and only that directory is walked. A pattern that
// starts with a wildcard is refused, so a glob can never widen into a whole
// home folder.
func expandGlob(pattern string) ([]string, error) {
	parts := strings.Split(filepath.ToSlash(pattern), "/")
	base := 0
	for base < len(parts) && !hasMeta(parts[base]) {
		base++
	}
	if base == 0 || base == len(parts) {
		return nil, fmt.Errorf("memory: refusing to walk the wide glob %q", pattern)
	}
	root := strings.Join(parts[:base], "/")
	if root == "" {
		return nil, fmt.Errorf("memory: refusing to walk the wide glob %q", pattern)
	}
	var out []string
	walkGlob(root, parts[base:], 0, &out)
	sort.Strings(out)
	return out, nil
}

// hasMeta tells whether a path segment holds a wildcard.
func hasMeta(part string) bool {
	return strings.ContainsAny(part, "*?[")
}

// walkGlob matches the rest of a pattern below a directory.
func walkGlob(dir string, parts []string, depth int, out *[]string) {
	if depth > maxGlobDepth {
		return
	}
	if len(parts) == 0 {
		*out = append(*out, dir)
		return
	}
	part := parts[0]
	if part == "**" {
		if len(parts) == 1 {
			// Everything below the directory.
			collectFiles(dir, depth+1, out)
			return
		}
		// Zero directories, then one directory at a time.
		walkGlob(dir, parts[1:], depth+1, out)
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			walkGlob(filepath.Join(dir, e.Name()), parts, depth+1, out)
		}
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !matchSegment(part, name) {
			continue
		}
		full := filepath.Join(dir, name)
		if len(parts) == 1 {
			if e.IsDir() {
				continue
			}
			*out = append(*out, full)
			continue
		}
		if e.IsDir() {
			walkGlob(full, parts[1:], depth+1, out)
		}
	}
}

// collectFiles lists every regular file below a directory.
func collectFiles(dir string, depth int, out *[]string) {
	if depth > maxGlobDepth {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		full := filepath.Join(dir, e.Name())
		if e.IsDir() {
			collectFiles(full, depth+1, out)
			continue
		}
		*out = append(*out, full)
	}
}

// matchSegment matches one path segment against a pattern with * and ?.
func matchSegment(pattern, name string) bool {
	ok, err := filepath.Match(pattern, name)
	return err == nil && ok
}
