//go:build !linux

package worker

import (
	"context"
	"os"

	"github.com/OpenVibers/OpenVibe.Node/internal/config"
)

// DeskSpec is one desk's program as RunDesk runs it; off Linux a desk never runs sandboxed (desk_linux.go).
type DeskSpec struct {
	Argv                                 []string
	Env                                  []string
	Binds                                []string
	MemBytes, CPUMillis, DiskBytes, Pids int64
	Egress                               string
	In, Out, Stderr                      *os.File
}

// CheckDeskBinds refuses every bind off Linux: there is no sandbox to bind into.
func CheckDeskBinds(nodeDirs, binds []string) ([]string, error) { return nil, errNotLinux }

// RunDesk: the namespaces a desk is confined with exist on Linux only.
func RunDesk(context.Context, config.WorkerConfig, DeskSpec) (int, error) { return 0, errNotLinux }
