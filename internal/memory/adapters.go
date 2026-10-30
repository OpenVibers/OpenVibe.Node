package memory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Adapters returns every memory adapter, in a fixed order.
func Adapters() []Adapter {
	return []Adapter{
		claudeCodeAdapter(),
		agentsAdapter(),
		geminiAdapter(),
		cursorAdapter(),
		copilotAdapter(),
		windsurfAdapter(),
		clineAdapter(),
		rooAdapter(),
		kiroAdapter(),
		continueAdapter(),
		aiderAdapter(),
		commandCodeAdapter(),
		openCodeAdapter(),
	}
}

// indexFilesOf says which files of an adapter are only an index: they hold
// the names of the memories, not the memories themselves.
func indexFilesOf(ad Adapter) []string {
	if ad.ID == "claude-code" {
		return []string{"MEMORY.md"}
	}
	return nil
}

// parseOptions say how one tool's files are read.
type parseOptions struct {
	// kindFromType reads the kind from the frontmatter "metadata.type".
	kindFromType bool
	// indexFiles are files that are only an index and hold no memory.
	indexFiles []string
}

// parseMarkdown turns a markdown-like file into items.
//
// The frontmatter is read, not turned into items. Every top-level bullet and
// every paragraph under a heading becomes one item. Secrets are removed from
// every item before it leaves the parser, and an item that was nothing but a
// secret is dropped.
func parseMarkdown(opts parseOptions, path string, data []byte) []Item {
	fm, body := splitFrontmatter(data)
	for _, name := range opts.indexFiles {
		if strings.EqualFold(filepath.Base(path), name) {
			return nil
		}
	}
	// The frontmatter is read, not turned into items, but its values may hold
	// a secret: they are scrubbed before any of them is used. The sentinel
	// marker is used here too, so that a secret in the frontmatter is still
	// counted on the item that carries it.
	for k, v := range fm {
		fm[k], _ = redactWith(v, secretMark)
	}
	// The whole body is scrubbed before it is split into items. A secret that
	// spans two items (a key in a fenced block, an .env block) is only seen
	// here. The marker is a sentinel, so that each item can still say how
	// many secrets it holds.
	body, _ = redactWith(body, secretMark)
	var fileKind Kind
	if opts.kindFromType {
		fileKind = kindFromType(fm)
	}
	var items []Item
	for _, md := range markdownItems(body) {
		// The list marker is not part of the memory.
		text := stripListMarker(md.Text)
		heading := md.Heading
		if fm["description"] != "" {
			// The tool's own name for the rule wins: it is what the
			// person wrote for the tool.
			heading = fm["description"]
		}
		// The body was scrubbed before it was split, so the secrets it held
		// are already marked: the item counts them.
		count := strings.Count(text, secretMark) + strings.Count(heading, secretMark)
		text = strings.ReplaceAll(text, secretMark, Redacted)
		heading = strings.ReplaceAll(heading, secretMark, Redacted)
		// The item is scrubbed again on its own: cheap, and it catches a
		// secret that only appears once the body is split.
		var more int
		text, more = Redact(text)
		heading, hmore := Redact(heading)
		count += more + hmore
		if onlySecret(stripListMarker(text)) {
			// The item was nothing but a secret: it is dropped.
			continue
		}
		kind := fileKind
		if kind == "" {
			// The heading of the section gives the kind; when it says
			// nothing, the scope of the file gives the fallback.
			kind = kindFromHeading(heading, "")
		}
		item := Item{Text: text, Kind: kind, Heading: heading}
		if count > 0 {
			item.Redacted = true
			item.Redactions = count
		}
		if len([]rune(item.Text)) > MaxItemRunes {
			item.Text = string([]rune(item.Text)[:MaxItemRunes]) + "\u2026"
			item.Truncated = true
		}
		items = append(items, item)
	}
	return items
}

// stripListMarker takes the list marker off an item, so that an item that
// holds nothing but a secret is seen for what it is.
func stripListMarker(text string) string {
	trimmed := strings.TrimSpace(text)
	if m := bulletLine.FindString(trimmed); m != "" {
		trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, m))
	}
	return trimmed
}

// kindFromType reads the kind of a memory from the frontmatter of an
// auto-memory file. Claude Code writes one of user, feedback, project or
// reference; other tools may already write a kind of ours.
func kindFromType(fm map[string]string) Kind {
	t := strings.ToLower(strings.TrimSpace(fm["metadata.type"]))
	switch t {
	case "":
		return ""
	case "user", "preference", "preferences":
		return KindPreference
	case "feedback", "lesson", "lessons":
		return KindLesson
	case "project", "decision", "decisions":
		return KindDecision
	case "reference", "fact", "facts":
		return KindFact
	case "procedure", "skill":
		return KindProcedure
	case "project_rule":
		return KindProjectRule
	}
	return ""
}

// claudeCodeAdapter reads what Claude Code remembers.
//
// Docs: https://code.claude.com/docs/en/memory
func claudeCodeAdapter() Adapter {
	return Adapter{
		ID:   "claude-code",
		Name: "Claude Code",
		Docs: "https://code.claude.com/docs/en/memory",
		Locations: func(home string, projects []string) []string {
			out := []string{
				filepath.Join(home, ".claude", "CLAUDE.md"),
				filepath.Join(home, ".claude", "rules", "**", "*.md"),
				filepath.Join(home, ".claude", "projects", "*", "memory", "*.md"),
			}
			for _, p := range projects {
				out = append(out,
					filepath.Join(p, "CLAUDE.md"),
					filepath.Join(p, ".claude", "CLAUDE.md"),
					filepath.Join(p, "CLAUDE.local.md"),
					filepath.Join(p, ".claude", "rules", "**", "*.md"),
				)
			}
			return out
		},
		Parse: func(path string, data []byte) []Item {
			return parseMarkdown(parseOptions{kindFromType: true, indexFiles: []string{"MEMORY.md"}}, path, data)
		},
	}
}

// agentsAdapter reads the AGENTS.md family.
//
// Docs: https://agents.md/
func agentsAdapter() Adapter {
	return Adapter{
		ID:   "agents",
		Name: "AGENTS.md (Codex and friends)",
		Docs: "https://agents.md/",
		Locations: func(home string, projects []string) []string {
			out := []string{filepath.Join(home, ".codex", "AGENTS.md")}
			for _, p := range projects {
				out = append(out, filepath.Join(p, "AGENTS.md"))
			}
			return out
		},
		Parse: func(path string, data []byte) []Item {
			return parseMarkdown(parseOptions{}, path, data)
		},
	}
}

// geminiAdapter reads what Gemini CLI remembers.
//
// Docs: https://geminicli.com/docs/cli/gemini-md/
func geminiAdapter() Adapter {
	return Adapter{
		ID:   "gemini",
		Name: "Gemini CLI",
		Docs: "https://geminicli.com/docs/cli/gemini-md/",
		Locations: func(home string, projects []string) []string {
			out := []string{filepath.Join(home, ".gemini", "GEMINI.md")}
			for _, p := range projects {
				out = append(out,
					filepath.Join(p, "GEMINI.md"),
					filepath.Join(p, ".gemini", "GEMINI.md"),
				)
			}
			return out
		},
		Parse: func(path string, data []byte) []Item {
			return parseMarkdown(parseOptions{}, path, data)
		},
	}
}

// cursorAdapter reads the Cursor rules.
//
// Docs: https://cursor.com/docs/context/rules
func cursorAdapter() Adapter {
	return Adapter{
		ID:   "cursor",
		Name: "Cursor",
		Docs: "https://cursor.com/docs/context/rules",
		Locations: func(home string, projects []string) []string {
			out := []string{}
			for _, p := range projects {
				out = append(out, filepath.Join(p, ".cursor", "rules", "*.mdc"))
			}
			return out
		},
		Parse: func(path string, data []byte) []Item {
			return parseMarkdown(parseOptions{}, path, data)
		},
	}
}

// copilotAdapter reads the GitHub Copilot instructions.
//
// Docs: https://docs.github.com/en/copilot/customizing-copilot/adding-repository-custom-instructions-for-github-copilot
func copilotAdapter() Adapter {
	return Adapter{
		ID:   "copilot",
		Name: "GitHub Copilot",
		Docs: "https://docs.github.com/en/copilot/customizing-copilot/adding-repository-custom-instructions-for-github-copilot",
		Locations: func(home string, projects []string) []string {
			out := []string{}
			for _, p := range projects {
				out = append(out,
					filepath.Join(p, ".github", "copilot-instructions.md"),
					filepath.Join(p, ".github", "instructions", "*.instructions.md"),
				)
			}
			return out
		},
		Parse: func(path string, data []byte) []Item {
			return parseMarkdown(parseOptions{}, path, data)
		},
	}
}

// windsurfAdapter reads the Windsurf rules and memories.
//
// Docs: https://docs.windsurf.com/windsurf/cascade/memories
func windsurfAdapter() Adapter {
	return Adapter{
		ID:   "windsurf",
		Name: "Windsurf",
		Docs: "https://docs.windsurf.com/windsurf/cascade/memories",
		Locations: func(home string, projects []string) []string {
			out := []string{filepath.Join(home, ".codeium", "windsurf", "memories", "**")}
			for _, p := range projects {
				out = append(out, filepath.Join(p, ".windsurf", "rules", "**"))
			}
			return out
		},
		Parse: func(path string, data []byte) []Item {
			return parseMarkdown(parseOptions{}, path, data)
		},
	}
}

// clineAdapter reads the Cline rules and the memory bank.
//
// Docs: https://docs.cline.bot/features/cline-rules
func clineAdapter() Adapter {
	return Adapter{
		ID:   "cline",
		Name: "Cline",
		Docs: "https://docs.cline.bot/features/cline-rules",
		Locations: func(home string, projects []string) []string {
			out := []string{}
			for _, p := range projects {
				out = append(out,
					filepath.Join(p, ".clinerules"),
					filepath.Join(p, ".clinerules", "**"),
					filepath.Join(p, "memory-bank", "*.md"),
				)
			}
			return out
		},
		Parse: func(path string, data []byte) []Item {
			return parseMarkdown(parseOptions{}, path, data)
		},
	}
}

// rooAdapter reads the Roo rules and the memory bank.
//
// Docs: https://docs.roocode.com/features/custom-instructions
func rooAdapter() Adapter {
	return Adapter{
		ID:   "roo",
		Name: "Roo Code",
		Docs: "https://docs.roocode.com/features/custom-instructions",
		Locations: func(home string, projects []string) []string {
			out := []string{}
			for _, p := range projects {
				out = append(out,
					filepath.Join(p, ".roo", "rules", "**"),
					filepath.Join(p, "memory-bank", "*.md"),
				)
			}
			return out
		},
		Parse: func(path string, data []byte) []Item {
			return parseMarkdown(parseOptions{}, path, data)
		},
	}
}

// kiroAdapter reads the Kiro steering files.
//
// Docs: https://kiro.dev/docs/steering/
func kiroAdapter() Adapter {
	return Adapter{
		ID:   "kiro",
		Name: "Kiro",
		Docs: "https://kiro.dev/docs/steering/",
		Locations: func(home string, projects []string) []string {
			out := []string{filepath.Join(home, ".kiro", "steering", "*.md")}
			for _, p := range projects {
				out = append(out, filepath.Join(p, ".kiro", "steering", "*.md"))
			}
			return out
		},
		Parse: func(path string, data []byte) []Item {
			return parseMarkdown(parseOptions{}, path, data)
		},
	}
}

// continueAdapter reads the Continue rules.
//
// Docs: https://docs.continue.dev/customize/rules
func continueAdapter() Adapter {
	return Adapter{
		ID:   "continue",
		Name: "Continue",
		Docs: "https://docs.continue.dev/customize/rules",
		Locations: func(home string, projects []string) []string {
			out := []string{filepath.Join(home, ".continue", "rules", "**")}
			for _, p := range projects {
				out = append(out, filepath.Join(p, ".continue", "rules", "**"))
			}
			return out
		},
		Parse: func(path string, data []byte) []Item {
			return parseMarkdown(parseOptions{}, path, data)
		},
	}
}

// aiderAdapter reads the Aider conventions file and the files its config
// names. It never reads the chat history.
//
// Docs: https://aider.chat/docs/usage/conventions.html
func aiderAdapter() Adapter {
	return Adapter{
		ID:   "aider",
		Name: "Aider",
		Docs: "https://aider.chat/docs/usage/conventions.html",
		Locations: func(home string, projects []string) []string {
			out := []string{}
			for _, p := range projects {
				out = append(out,
					filepath.Join(p, "CONVENTIONS.md"),
					filepath.Join(p, ".aider.conf.yml"),
				)
				out = append(out, readListFromConfig(filepath.Join(p, ".aider.conf.yml"), p)...)
			}
			out = append(out,
				filepath.Join(home, "CONVENTIONS.md"),
				filepath.Join(home, ".aider.conf.yml"),
			)
			out = append(out, readListFromConfig(filepath.Join(home, ".aider.conf.yml"), home)...)
			return out
		},
		Parse: func(path string, data []byte) []Item {
			if strings.HasSuffix(path, ".yml") || strings.HasSuffix(path, ".yaml") {
				// The config itself is not memory; only the files it names are.
				return nil
			}
			return parseMarkdown(parseOptions{}, path, data)
		},
	}
}

// commandCodeAdapter reads the Command Code memory folder.
//
// Docs: https://commandcode.ai/docs/memory
func commandCodeAdapter() Adapter {
	return Adapter{
		ID:   "command-code",
		Name: "Command Code",
		Docs: "https://commandcode.ai/docs/memory",
		Locations: func(home string, projects []string) []string {
			out := []string{filepath.Join(home, ".commandcode", "**")}
			for _, p := range projects {
				out = append(out, filepath.Join(p, ".commandcode", "**"))
			}
			return out
		},
		Parse: func(path string, data []byte) []Item {
			return parseMarkdown(parseOptions{}, path, data)
		},
	}
}

// openCodeAdapter reads the OpenCode AGENTS.md and the instructions its
// config names.
//
// Docs: https://opencode.ai/docs/rules/
func openCodeAdapter() Adapter {
	return Adapter{
		ID:   "opencode",
		Name: "OpenCode",
		Docs: "https://opencode.ai/docs/rules/",
		Locations: func(home string, projects []string) []string {
			out := []string{filepath.Join(home, ".config", "opencode", "AGENTS.md")}
			for _, p := range projects {
				out = append(out,
					filepath.Join(p, "AGENTS.md"),
					filepath.Join(p, "opencode.json"),
				)
				out = append(out, instructionsFromConfig(filepath.Join(p, "opencode.json"), p)...)
			}
			globalConfig := filepath.Join(home, ".config", "opencode", "opencode.json")
			out = append(out, globalConfig)
			out = append(out, instructionsFromConfig(globalConfig, filepath.Dir(globalConfig))...)
			return out
		},
		Parse: func(path string, data []byte) []Item {
			if strings.HasSuffix(path, ".json") {
				// The config itself is not memory; only the files it names are.
				return nil
			}
			return parseMarkdown(parseOptions{}, path, data)
		},
	}
}

// readListFromConfig returns the files an Aider config names under "read:".
func readListFromConfig(configPath, base string) []string {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil
	}
	var out []string
	inRead := false
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indented := strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")
		if !indented {
			// A new key: only "read:" names memory files.
			inRead = strings.HasPrefix(trimmed, "read:")
			continue
		}
		if !inRead || !strings.HasPrefix(trimmed, "- ") {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))
		value = strings.Trim(value, "\"'")
		if value == "" {
			continue
		}
		if !filepath.IsAbs(value) {
			value = filepath.Join(base, value)
		}
		out = append(out, filepath.Clean(value))
	}
	return out
}

// instructionsFromConfig returns the instruction files an OpenCode config
// names under "instructions".
func instructionsFromConfig(configPath, base string) []string {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil
	}
	var cfg struct {
		Instructions []string `json:"instructions"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil
	}
	var out []string
	for _, v := range cfg.Instructions {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if !filepath.IsAbs(v) {
			v = filepath.Join(base, v)
		}
		out = append(out, filepath.Clean(v))
	}
	return out
}
