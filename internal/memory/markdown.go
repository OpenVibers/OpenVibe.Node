package memory

import (
	"regexp"
	"strings"
)

// headingLine matches a markdown heading.
var headingLine = regexp.MustCompile("^#{1,6}[ \t]+(.+)$")

// bulletLine matches a list item, ordered or not.
var bulletLine = regexp.MustCompile("^(?:[-*+][ \t]+|[0-9]+[.)][ \t]+)")

// mdItem is a piece of markdown before it becomes an Item.
type mdItem struct {
	Heading string
	Text    string
}

// splitFrontmatter takes the YAML-ish frontmatter off a markdown file and
// returns it as flat keys ("name", "description", "metadata.type", "globs")
// with the rest of the file.
func splitFrontmatter(data []byte) (map[string]string, string) {
	s := strings.ReplaceAll(string(data), "\r\n", "\n")
	s = strings.TrimPrefix(s, "\ufeff")
	if !strings.HasPrefix(s, "---\n") {
		return nil, s
	}
	rest := s[4:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return nil, s
	}
	head := rest[:end]
	tail := strings.TrimPrefix(rest[end+4:], "\n")
	return parseFrontmatter(strings.Split(head, "\n")), tail
}

// parseFrontmatter reads the small subset of YAML these files use: scalar
// keys, one level of nested keys ("metadata:" then "type:") and lists of
// strings.
func parseFrontmatter(lines []string) map[string]string {
	fm := map[string]string{}
	parent := ""
	parentIndent := -1
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if strings.HasPrefix(trimmed, "- ") {
			value := strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "- ")), "\"'")
			if parent != "" {
				if fm[parent] == "" {
					fm[parent] = value
				} else {
					fm[parent] += ", " + value
				}
			}
			continue
		}
		idx := strings.Index(trimmed, ":")
		if idx <= 0 {
			continue
		}
		key := strings.TrimSpace(trimmed[:idx])
		value := strings.Trim(strings.TrimSpace(trimmed[idx+1:]), "\"'")
		if parent != "" && indent > parentIndent {
			fm[parent+"."+key] = value
			continue
		}
		parent, parentIndent = "", -1
		if value == "" {
			parent, parentIndent = key, indent
			if _, ok := fm[key]; !ok {
				fm[key] = ""
			}
			continue
		}
		fm[key] = value
	}
	return fm
}

// markdownItems splits markdown into items: one per top-level bullet and one
// per paragraph under a heading. The heading each item came from is kept.
//
// A bullet is one item; the lines indented under it belong to it. A paragraph
// is the run of text between two bullets.
func markdownItems(body string) []mdItem {
	var out []mdItem
	heading := ""
	var buf []string
	mode := 0 // 0: none, 1: bullet, 2: paragraph

	flush := func() {
		if len(buf) == 0 {
			mode = 0
			return
		}
		text := strings.TrimSpace(strings.Join(buf, "\n"))
		buf = nil
		mode = 0
		if text == "" {
			return
		}
		out = append(out, mdItem{Heading: heading, Text: text})
	}

	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if m := headingLine.FindStringSubmatch(trimmed); m != nil {
			flush()
			heading = strings.TrimSpace(m[1])
			continue
		}
		if trimmed == "" {
			flush()
			continue
		}
		indented := line != trimmed
		if bulletLine.MatchString(trimmed) {
			// A new bullet is a new item.
			flush()
			mode = 1
			buf = append(buf, trimmed)
			continue
		}
		if mode == 1 && indented {
			// A line under the bullet belongs to it.
			buf = append(buf, trimmed)
			continue
		}
		if mode == 1 {
			// Plain text after a bullet starts a new paragraph.
			flush()
		}
		if mode != 2 {
			flush()
			mode = 2
		}
		buf = append(buf, trimmed)
	}
	flush()
	return out
}

// kindFromHeading guesses the kind of an item from the section it came from,
// falling back to the kind of the adapter's file.
func kindFromHeading(heading string, fallback Kind) Kind {
	h := strings.ToLower(heading)
	switch {
	case strings.Contains(h, "preference"), strings.Contains(h, "prefer"):
		return KindPreference
	case strings.Contains(h, "lesson"), strings.Contains(h, "learned"), strings.Contains(h, "mistake"), strings.Contains(h, "gotcha"):
		return KindLesson
	case strings.Contains(h, "decision"), strings.Contains(h, "rationale"), strings.Contains(h, "adr"):
		return KindDecision
	case strings.Contains(h, "procedure"), strings.Contains(h, "workflow"), strings.Contains(h, "how to"), strings.Contains(h, "process"), strings.Contains(h, "steps"):
		return KindProcedure
	case strings.Contains(h, "fact"), strings.Contains(h, "profile"):
		return KindFact
	case strings.Contains(h, "rule"), strings.Contains(h, "convention"), strings.Contains(h, "guideline"), strings.Contains(h, "standard"), strings.Contains(h, "policy"):
		return KindProjectRule
	}
	return fallback
}
