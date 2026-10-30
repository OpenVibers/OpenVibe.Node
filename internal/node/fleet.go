package node

import "github.com/OpenVibers/OpenVibe.Node/internal/config"

// codesProbeCommand builds the fleet probe (Codes C9c) from the worker's declared OpenVibe.Codes entry: the same
// interpreter and script the jobs run, asked for `logins --json` instead of `job`. Nil when the machine has no such
// entry — a Node that never installed the Codes runner has no coding agents to report.
func codesProbeCommand(fns []config.FunctionConfig) []string {
	for _, f := range fns {
		if f.Name != "openvibe-codes" || f.EffectiveClass() != config.ClassCode || len(f.Command) == 0 {
			continue
		}
		argv := append([]string(nil), f.Command...)
		// The installer declares the entry as `node …/openvibe-codes.js job`: the same command with another verb.
		if len(argv) > 1 && argv[len(argv)-1] == "job" {
			argv = argv[:len(argv)-1]
		}
		return append(argv, "logins", "--json")
	}
	return nil
}
