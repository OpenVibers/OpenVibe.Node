//go:build linux && !amd64 && !arm64

package worker

// No seccomp allowlist for this architecture: the probe fails, so the class is not advertised and no job runs.
const (
	auditArch      = 0
	x32ABI         = false
	sysClone       = 0
	sysClone3      = 0
	sysSocket      = 0
	sysMemfdCreate = 0 // never called: policy refuses this architecture first
)

var allowedSyscalls []uint32
