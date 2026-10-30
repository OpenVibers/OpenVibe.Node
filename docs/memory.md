# Memory: the Node reads what your tools remember

The Node reads the memory files of the coding tools on your machine and tells
you what it found. It is **read-only, local and redacted**:

- it opens the files it knows about and nothing else;
- it never writes a file and never opens a network connection;
- it removes the secrets before anything is printed;
- it never follows a symlink out of the home folder or the project folders it
  was given.

This page is stage M1 of OpenVibe Memory: the readers. The store, the
encryption, the upload and the page come later.

## The command

```sh
openvibe-node memory scan [--tools a,b] [--project DIR …] [--json]
```

| Flag | What it does |
|---|---|
| `--tools a,b` | only these adapters (default: all) |
| `--project DIR` | read this project as well; may be given more than once |
| `--json` | print the items as JSON |
| `--user-home DIR` | read this directory as the person's home (default: yours) |

Without `--project` only the user-level files are read: the files that hold
your own preferences across every project. With `--project` the project files
are read too, and their items carry the scope `project:<absolute path>`.

The command prints every adapter, every path it looked at and whether it was
there, and how many items each file gave:

```
Memory scan of /home/you
No project given: only the user-level files were read.

Claude Code (claude-code) — 3 items
  docs: https://code.claude.com/docs/en/memory
  - /home/you/.claude/CLAUDE.md: 3 items
  - /home/you/.claude/rules/**/*.md: not there
...
```

## The adapters

Each adapter is data: where to look, and how to read what is there.

| Adapter | id | What it reads | Docs |
|---|---|---|---|
| Claude Code | `claude-code` | `~/.claude/CLAUDE.md`, `~/.claude/rules/**/*.md`, `~/.claude/projects/*/memory/*.md` (the `MEMORY.md` index is skipped), and in a project `CLAUDE.md`, `.claude/CLAUDE.md`, `CLAUDE.local.md`, `.claude/rules/**/*.md` | [code.claude.com/docs/en/memory](https://code.claude.com/docs/en/memory) |
| AGENTS.md | `agents` | `~/.codex/AGENTS.md` and the project's `AGENTS.md` | [agents.md](https://agents.md/) |
| Gemini CLI | `gemini` | `~/.gemini/GEMINI.md` and the project's `GEMINI.md` or `.gemini/GEMINI.md` | [geminicli.com/docs/cli/gemini-md](https://geminicli.com/docs/cli/gemini-md/) |
| Cursor | `cursor` | the project's `.cursor/rules/*.mdc` | [cursor.com/docs/context/rules](https://cursor.com/docs/context/rules) |
| GitHub Copilot | `copilot` | the project's `.github/copilot-instructions.md` and `.github/instructions/*.instructions.md` | [docs.github.com](https://docs.github.com/en/copilot/customizing-copilot/adding-repository-custom-instructions-for-github-copilot) |
| Windsurf | `windsurf` | `~/.codeium/windsurf/memories/**` and the project's `.windsurf/rules/**` | [docs.windsurf.com](https://docs.windsurf.com/windsurf/cascade/memories) |
| Cline | `cline` | the project's `.clinerules`, `.clinerules/**` and `memory-bank/*.md` | [docs.cline.bot/features/cline-rules](https://docs.cline.bot/features/cline-rules) |
| Roo Code | `roo` | the project's `.roo/rules/**` and `memory-bank/*.md` | [docs.roocode.com](https://docs.roocode.com/features/custom-instructions) |
| Kiro | `kiro` | `~/.kiro/steering/*.md` and the project's `.kiro/steering/*.md` | [kiro.dev/docs/steering](https://kiro.dev/docs/steering/) |
| Continue | `continue` | `~/.continue/rules/**` and the project's `.continue/rules/**` | [docs.continue.dev/customize/rules](https://docs.continue.dev/customize/rules) |
| Aider | `aider` | the project's `CONVENTIONS.md` and the files its `.aider.conf.yml` names under `read:` | [aider.chat/docs/usage/conventions](https://aider.chat/docs/usage/conventions.html) |
| Command Code | `command-code` | `~/.commandcode/**` and the project's `.commandcode/**` | [commandcode.ai/docs/memory](https://commandcode.ai/docs/memory) |
| OpenCode | `opencode` | `~/.config/opencode/AGENTS.md`, the project's `AGENTS.md` and the files its `opencode.json` names under `instructions` | [opencode.ai/docs/rules](https://opencode.ai/docs/rules/) |

### What is never read

- The chat history of any tool (Aider's `.aider.chat.history.md`, Claude's
  transcripts, Continue's sessions).
- The settings, the hooks, the skills and the plugins of any tool.
- Any folder that is not one of the paths above. The home folder itself is
  never read as a whole: each adapter names the exact files and the narrow
  globs of its own tool, and a glob always starts at a fixed folder.
- A file over 256 KB (it is skipped with a note) and a binary file.

## How a file becomes items

- The frontmatter is read, not turned into items.
- A markdown file is split by its headings: every top-level bullet is one
  item, and every paragraph under a heading is one item.
- An item is at most 2,000 characters; a longer one is cut and marked
  `truncated`.
- An item that is only a secret is dropped.

## The kinds

| Kind | What it is |
|---|---|
| `preference` | how the person wants things done |
| `fact` | a fact about the person or the project |
| `project_rule` | a rule that holds in a project |
| `procedure` | a way of doing something |
| `decision` | a decision that was taken, with its reason |
| `lesson` | something that was learned the hard way |

The kind comes from the tool when it says it (Claude Code's auto memory writes
a `metadata.type` in its frontmatter: `user`, `feedback`, `project` or
`reference`), otherwise from the heading the item came from, otherwise from the
scope: a file of the person is about them, a file of a project holds that
project's rules.

## The secrets

Every item is scrubbed before it leaves the parser. The patterns are the ones
of OpenVibe.Codes' harness (`harness/events.js`, `SECRET_PATTERNS`) plus the
common gitleaks shapes:

- OpenAI- and Anthropic-style keys (`sk-…`), Stripe keys (`sk_live_…`);
- GitHub tokens (`ghp_…`, `gho_…`, `github_pat_…`);
- AWS keys (`AKIA…`, `ASIA…`) and `aws_secret_access_key`;
- Slack tokens (`xoxb-…`);
- JWTs;
- PEM private keys;
- `password=`, `token=`, `secret=`, `api_key=` assignments;
- connection strings that carry a password.

Each match becomes `[redacted]`. The item is marked `redacted` and keeps a
count of how many secrets were removed; the values are never kept. An item
that held nothing but a secret is dropped.

Every adapter's fixture in `internal/memory/testdata` holds one secret of a
different shape, and the test proves that none of them reaches an item.

## The JSON

```json
{
  "home": "/home/you",
  "projects": ["/srv/work/demo"],
  "adapters": [
    {
      "id": "claude-code",
      "name": "Claude Code",
      "docs": "https://code.claude.com/docs/en/memory",
      "files": [
        {
          "path": "/home/you/.claude/CLAUDE.md",
          "found": true,
          "items": 3
        },
        {
          "path": "/home/you/.claude/rules/**/*.md",
          "found": false
        }
      ],
      "items": 3
    }
  ],
  "items": [
    {
      "text": "Prefer tabs in Go files",
      "kind": "preference",
      "scope": "user",
      "source": "claude-code",
      "path": "/home/you/.claude/CLAUDE.md",
      "updated": "2026-10-30T15:06:16Z",
      "heading": "Personal setup"
    }
  ]
}
```

An item may also carry:

| Field | Meaning |
|---|---|
| `redacted` | the item held a secret that was removed |
| `redactions` | how many secrets were removed |
| `truncated` | the item was longer than 2,000 characters |

A file that was looked at but could not be used carries a `note`:
`skipped: larger than 256 KB`, `skipped: a binary file`,
`skipped: link leaves the scanned paths`.

## The safety tests

`internal/memory` has a test for each of these:

- no adapter ever points at the home folder, and no glob is wider than the
  tool's own folders;
- a scan without a project reads only the user-level files;
- a symlink out of the scanned paths is not followed;
- a file over 256 KB and a binary file are skipped;
- the package does not import `net/http`: it never opens a network
  connection.
