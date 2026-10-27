package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/OpenVibers/OpenVibe.Node/internal/config"
	"github.com/OpenVibers/OpenVibe.Node/internal/worker"
)

// listFlag gathers a repeatable flag.
type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

const deskSandboxUsage = `usage: openvibe-node desk-sandbox [--home DIR] [--memory-mb N] [--cpu-millis N] [--disk-mb N] [--pids N]
         [--bind PATH]... [--setenv NAME=VALUE]... [--egress public] -- PROGRAM [ARG]...

Runs one desk's program (its browser) confined, for a desk host that serves people other than its owner: in the
jobs' sandbox (worker.run_as, its own namespaces and cgroup, only the system paths, its /tmp and the read-only binds
under /opt, /usr/local or /etc/fonts), with fds 3 and 4 passed through as its --remote-debugging-pipe. Needs what
jobs need (root with worker.run_as, cgroup v2 delegated); --egress public needs worker.egress public. Exits with the
program's code; 125 when the sandbox could not be set up.
`

// deskPipe answers the desk's two pipe ends (its fds 3 and 4); tests replace it.
var deskPipe = inheritedDeskPipe

// cmdDeskSandbox is `openvibe-node desk-sandbox`; it parses its own flags, up to "--", so the program's flags (a
// browser's) are never read as the Node's.
func cmdDeskSandbox(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("desk-sandbox", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, deskSandboxUsage) }
	home := fs.String("home", "", "config and state directory")
	memMB := fs.Int64("memory-mb", 1024, "memory.max of the desk, in MiB (swap off)")
	cpu := fs.Int64("cpu-millis", 1000, "CPU of the desk, in thousandths of a core (1000 = one core)")
	diskMB := fs.Int64("disk-mb", 512, "the size of the desk's /tmp, where its profile lives, in MiB")
	pids := fs.Int64("pids", 256, "the most processes and threads of the desk")
	egress := fs.String("egress", "", `"public" to reach the public internet (never the host's networks); none by default`)
	var binds, env listFlag
	fs.Var(&binds, "bind", "a read-only path the program needs (repeatable): its install directory, /etc/fonts")
	fs.Var(&env, "setenv", "NAME=VALUE in the program's environment (repeatable); it gets nothing else")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	argv := fs.Args()
	if len(argv) == 0 {
		fs.Usage()
		return 2
	}
	// Started by a plugin, it reads the config of the Node that started it (which names the plugin's environment).
	paths := config.DefaultPaths(*home)
	if c, st := os.Getenv(config.EnvConfigDir), os.Getenv(config.EnvStateDir); *home == "" && c != "" && st != "" {
		paths = config.Paths{ConfigDir: c, StateDir: st}
	}
	cfg, err := config.Load(paths.ConfigFile())
	if err != nil {
		fmt.Fprintln(stderr, "desk-sandbox:", err)
		return 125
	}
	wcfg := cfg.Worker
	wcfg.NodeDirs = []string{paths.ConfigDir, paths.StateDir} // as the Node's own jobs: no desk sees them
	in, out, err := deskPipe()
	if err != nil {
		fmt.Fprintln(stderr, "desk-sandbox:", err)
		return 125
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer stop()
	code, err := worker.RunDesk(ctx, wcfg, worker.DeskSpec{Argv: argv, Env: env, Binds: binds,
		MemBytes: *memMB << 20, CPUMillis: *cpu, DiskBytes: *diskMB << 20, Pids: *pids, Egress: *egress,
		In: in, Out: out, Stderr: os.Stderr})
	if err != nil && ctx.Err() == nil {
		fmt.Fprintln(stderr, "desk-sandbox:", err)
		return 125
	}
	return code
}
