//go:build !linux

package worker

import (
	"errors"
	"os"
	"os/exec"

	"github.com/OpenVibers/OpenVibe.Node/internal/config"
)

// errNotLinux: the namespaces the worker isolates jobs with exist on Linux only, so elsewhere no job ever runs.
var errNotLinux = errors.New("the job worker runs on Linux only")

func isolate(*exec.Cmd, config.WorkerConfig) error { return errNotLinux }

func probe(config.WorkerConfig, func(*exec.Cmd, config.WorkerConfig) error) error { return errNotLinux }

func killGroup(p *os.Process) { _ = p.Kill() }

func limitCPU(int, int64) error { return errNotLinux }

func pidNS(int) (string, error) { return "", errNotLinux }

func sampleProcs(int, string) (int64, int64, error) { return 0, 0, errNotLinux }

func exitStatus(ps *os.ProcessState) (*int, bool) {
	if c := ps.ExitCode(); c >= 0 {
		return &c, false
	}
	return nil, false
}

func maxRSS(*os.ProcessState) int64 { return 0 }
