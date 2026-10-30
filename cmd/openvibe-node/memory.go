package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/OpenVibers/OpenVibe.Node/internal/memory"
)

// memoryUsage is what "openvibe-node memory" prints.
const memoryUsage = "openvibe-node memory scan [--tools a,b] [--project DIR] [--json]\n" +
	"\n" +
	"  Reads the memory files of the coding tools on this machine and prints what it\n" +
	"  found. It is read-only and local: nothing is written and nothing is sent\n" +
	"  anywhere. Secrets are removed before anything is printed.\n" +
	"\n" +
	"  --tools a,b      only these adapters (claude-code, agents, gemini, cursor,\n" +
	"                   copilot, windsurf, cline, roo, kiro, continue, aider,\n" +
	"                   command-code, opencode)\n" +
	"  --project DIR    read this project as well; may be given more than once\n" +
	"  --json           print the items as JSON\n" +
	"  --user-home DIR  read this directory as the person's home (default: yours)\n"

// stringList is a flag that may be given more than once.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			*s = append(*s, part)
		}
	}
	return nil
}

// cmdMemory runs the memory commands.
func cmdMemory(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "scan" {
		fmt.Fprint(stderr, memoryUsage)
		return 2
	}
	fs := flag.NewFlagSet("memory scan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, memoryUsage) }
	var tools stringList
	var projects stringList
	fs.Var(&tools, "tools", "only these adapters")
	fs.Var(&projects, "project", "read this project as well")
	asJSON := fs.Bool("json", false, "print the items as JSON")
	userHome := fs.String("user-home", "", "read this directory as the person's home")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unknown argument %q\n\n", fs.Arg(0))
		fmt.Fprint(stderr, memoryUsage)
		return 2
	}
	home := *userHome
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintln(stderr, "error: cannot find your home directory; pass --user-home")
			return 1
		}
		home = h
	}
	abs, err := filepath.Abs(home)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	res := memory.Scan(memory.Options{
		Home:     abs,
		Projects: projects,
		Tools:    tools,
	})
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		return 0
	}
	printScan(res, stdout)
	return 0
}

// printScan prints what the scan found: every adapter, every path it looked
// at and the items per file.
func printScan(res memory.Result, stdout io.Writer) {
	fmt.Fprintf(stdout, "Memory scan of %s\n", res.Home)
	if len(res.Projects) == 0 {
		fmt.Fprintln(stdout, "No project given: only the user-level files were read.")
	} else {
		fmt.Fprintf(stdout, "Projects: %s\n", strings.Join(res.Projects, ", "))
	}
	for _, ad := range res.Adapters {
		fmt.Fprintf(stdout, "\n%s (%s) — %d items\n", ad.Name, ad.ID, ad.Items)
		fmt.Fprintf(stdout, "  docs: %s\n", ad.Docs)
		if len(ad.Files) == 0 {
			fmt.Fprintln(stdout, "  no path to look at")
			continue
		}
		for _, f := range ad.Files {
			switch {
			case f.Note != "":
				fmt.Fprintf(stdout, "  - %s: %s\n", f.Path, f.Note)
			case f.Found:
				fmt.Fprintf(stdout, "  - %s: %d items\n", f.Path, f.Items)
			default:
				fmt.Fprintf(stdout, "  - %s: not there\n", f.Path)
			}
		}
	}
	fmt.Fprintf(stdout, "\n%d items in total. Nothing was written and nothing was sent.\n", len(res.Items))
}

// memoryToolIDs lists the adapter ids, for the usage text and the tests.
func memoryToolIDs() []string {
	var out []string
	for _, ad := range memory.Adapters() {
		out = append(out, ad.ID)
	}
	sort.Strings(out)
	return out
}
