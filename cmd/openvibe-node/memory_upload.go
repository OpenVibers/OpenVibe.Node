package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/OpenVibers/OpenVibe.Node/internal/memory"
	"github.com/OpenVibers/OpenVibe.Node/internal/memorystore"
	"github.com/OpenVibers/OpenVibe.Node/internal/memoryupload"
)

// memoryUploadUsage is what "openvibe-node memory upload" prints.
const memoryUploadUsage = "openvibe-node memory upload [--tools a,b] [--project DIR …] [--dry-run] [--yes]\n" +
	"\n" +
	"  Reads the memory files of the coding tools on this machine and sends the\n" +
	"  items to your own memory store (OpenVibe.AI). It shows what it found and\n" +
	"  what would leave the machine first, and asks before it sends anything.\n" +
	"\n" +
	"  --tools a,b      only these adapters\n" +
	"  --project DIR    read this project as well; may be given more than once\n" +
	"  --dry-run        print the summary and send nothing\n" +
	"  --yes            do not ask before sending\n" +
	"  --endpoint URL   the store (default " + memorystore.DefaultBaseURL + ")\n" +
	"  --token-env NAME the variable the access token is read from (default " + memorystore.DefaultTokenEnv + ")\n" +
	"  --user-home DIR  read this directory as the person's home (default: yours)\n"

// memoryStatusUsage is what "openvibe-node memory status" prints.
const memoryStatusUsage = "openvibe-node memory status [--show]\n" +
	"\n" +
	"  Shows what your own memory store holds: how many items, per source and per\n" +
	"  scope. The text of an item is only printed with --show.\n" +
	"\n" +
	"  --show           print the items themselves\n" +
	"  --endpoint URL   the store (default " + memorystore.DefaultBaseURL + ")\n" +
	"  --token-env NAME the variable the access token is read from (default " + memorystore.DefaultTokenEnv + ")\n"

// cmdMemoryUpload sends the items of the coding tools to the person's own
// memory store.
func cmdMemoryUpload(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("memory upload", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, memoryUploadUsage) }
	var tools stringList
	var projects stringList
	fs.Var(&tools, "tools", "only these adapters")
	fs.Var(&projects, "project", "read this project as well")
	dryRun := fs.Bool("dry-run", false, "print the summary and send nothing")
	assumeYes := fs.Bool("yes", false, "do not ask before sending")
	endpoint := fs.String("endpoint", memorystore.DefaultBaseURL, "the memory store")
	tokenEnv := fs.String("token-env", memorystore.DefaultTokenEnv, "the variable the access token is read from")
	userHome := fs.String("user-home", "", "read this directory as the person's home")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unknown argument %q\n\n", fs.Arg(0))
		fmt.Fprint(stderr, memoryUploadUsage)
		return 2
	}
	home, code := memoryHome(*userHome, stderr)
	if code != 0 {
		return code
	}
	res := memory.Scan(memory.Options{Home: home, Projects: projects, Tools: tools})
	plan := memoryupload.Build(res)

	printUploadSummary(plan, res, stdout)
	if *dryRun {
		fmt.Fprintln(stdout, "\nNothing was sent: this was a dry run.")
		return 0
	}
	if len(plan.Items) == 0 {
		fmt.Fprintln(stdout, "\nNothing to send.")
		return 0
	}
	if !*assumeYes && !confirmed(stdin, stdout, len(plan.Items)) {
		fmt.Fprintln(stdout, "\nNothing was sent.")
		return 0
	}
	token := strings.TrimSpace(os.Getenv(*tokenEnv))
	if token == "" {
		fmt.Fprintf(stderr, "error: %s is not set: it must hold your OpenVibe access token\n", *tokenEnv)
		return 1
	}
	client, err := memorystore.New(memorystore.Options{BaseURL: *endpoint, Token: token})
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	res2, err := client.Upload(context.Background(), plan.Items)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintf(stdout, "\nSent: %d new, %d already there, %d refused.\n", res2.Created, res2.Deduplicated, res2.Refused)
	return 0
}

// cmdMemoryStatus shows what the person's own memory store holds.
func cmdMemoryStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("memory status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, memoryStatusUsage) }
	show := fs.Bool("show", false, "print the items themselves")
	endpoint := fs.String("endpoint", memorystore.DefaultBaseURL, "the memory store")
	tokenEnv := fs.String("token-env", memorystore.DefaultTokenEnv, "the variable the access token is read from")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unknown argument %q\n\n", fs.Arg(0))
		fmt.Fprint(stderr, memoryStatusUsage)
		return 2
	}
	token := strings.TrimSpace(os.Getenv(*tokenEnv))
	if token == "" {
		fmt.Fprintf(stderr, "error: %s is not set: it must hold your OpenVibe access token\n", *tokenEnv)
		return 1
	}
	client, err := memorystore.New(memorystore.Options{BaseURL: *endpoint, Token: token})
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	res, err := client.List(context.Background(), "", "")
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	printMemoryStatus(res, *show, stdout)
	return 0
}

// memoryHome gives the home directory to read, and the code to return when
// there is none.
func memoryHome(userHome string, stderr io.Writer) (string, int) {
	home := userHome
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintln(stderr, "error: cannot find your home directory; pass --user-home")
			return "", 1
		}
		home = h
	}
	abs, err := filepath.Abs(home)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return "", 1
	}
	return abs, 0
}

// confirmed asks the person whether the items may be sent. Anything but a
// yes means no.
func confirmed(stdin io.Reader, stdout io.Writer, items int) bool {
	fmt.Fprintf(stdout, "\nSend these %d items to your memory store? [y/N] ", items)
	reader := bufio.NewReader(stdin)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		fmt.Fprintln(stdout)
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

// printUploadSummary shows what the scan found and what would leave the
// machine: every tool, every file, the items it gave, what was redacted and
// the scopes the items go to.
func printUploadSummary(plan memoryupload.Plan, res memory.Result, stdout io.Writer) {
	fmt.Fprintf(stdout, "Memory upload of %s\n", res.Home)
	if len(res.Projects) == 0 {
		fmt.Fprintln(stdout, "No project given: only the user-level files were read.")
	} else {
		fmt.Fprintf(stdout, "Projects: %s\n", strings.Join(res.Projects, ", "))
	}
	for _, ts := range plan.Summary.Tools {
		fmt.Fprintf(stdout, "\n%s (%s) — %d items\n", ts.Name, ts.ID, ts.Items)
		if ts.Items == 0 {
			// The tool was looked at and gave nothing: its paths are noise here.
			continue
		}
		for _, f := range ts.Files {
			switch {
			case f.Note != "":
				fmt.Fprintf(stdout, "  - %s: %s\n", relativePath(f.Path, res), f.Note)
			case f.Found:
				fmt.Fprintf(stdout, "  - %s: %d items\n", relativePath(f.Path, res), f.Items)
			default:
				fmt.Fprintf(stdout, "  - %s: not there\n", relativePath(f.Path, res))
			}
		}
		if ts.Redacted > 0 {
			fmt.Fprintf(stdout, "  %d items redacted (%d secrets removed)\n", ts.Redacted, ts.Redactions)
		}
	}
	fmt.Fprintf(stdout, "\nScopes: %s\n", strings.Join(plan.Summary.Scopes, ", "))
	fmt.Fprintf(stdout, "%d items to send", plan.Summary.Items)
	if plan.Summary.Redacted > 0 {
		fmt.Fprintf(stdout, ", %d redacted (%d secrets removed)", plan.Summary.Redacted, plan.Summary.Redactions)
	}
	if plan.Summary.Skipped > 0 {
		fmt.Fprintf(stdout, ", %d skipped (empty)", plan.Summary.Skipped)
	}
	fmt.Fprintln(stdout, ".")
}

// relativePath gives a path as the summary shows it: relative to the folder
// it was scanned from, so the summary holds no absolute path.
func relativePath(path string, res memory.Result) string {
	root := res.Home
	for _, p := range res.Projects {
		if path == p || strings.HasPrefix(path, p+string(filepath.Separator)) {
			root = p
			break
		}
	}
	if root == "" {
		return path
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}
	return filepath.ToSlash(rel)
}

// printMemoryStatus shows what the store holds: the counts per source and per
// scope, and the items themselves only when they are asked for.
func printMemoryStatus(res memorystore.ListResult, show bool, stdout io.Writer) {
	bySource := map[string]int{}
	byScope := map[string]int{}
	for _, it := range res.Items {
		for _, s := range strings.Split(it.Source, ",") {
			if s = strings.TrimSpace(s); s != "" {
				bySource[s]++
			}
		}
		byScope[it.Scope]++
	}
	fmt.Fprintf(stdout, "Your memory store holds %d items.\n", res.Count)
	fmt.Fprintln(stdout, "\nPer source:")
	for _, s := range sortedKeys(bySource) {
		fmt.Fprintf(stdout, "  %s: %d\n", s, bySource[s])
	}
	fmt.Fprintln(stdout, "\nPer scope:")
	for _, s := range sortedKeys(byScope) {
		fmt.Fprintf(stdout, "  %s: %d\n", s, byScope[s])
	}
	if !show {
		fmt.Fprintln(stdout, "\nThe text of the items is not shown; use --show to read them.")
		return
	}
	fmt.Fprintln(stdout, "\nThe items:")
	for _, it := range res.Items {
		fmt.Fprintf(stdout, "  - [%s] %s (%s)\n", it.Scope, it.Kind, it.Source)
		fmt.Fprintf(stdout, "    %s\n", strings.TrimSpace(it.Text))
	}
}

// sortedKeys gives the keys of a count map, sorted.
func sortedKeys(counts map[string]int) []string {
	out := make([]string, 0, len(counts))
	for k := range counts {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
