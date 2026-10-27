package plugins

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The Python SDK's own tests, on every OS a CI job runs go test on (Linux, macOS, Windows): the runtime behaves the
// same everywhere a driver runs (owner, 2026-10-27: "work more in harmony and unified across all operating systems").
func TestPythonSDKSuiteOnThisOS(t *testing.T) {
	py := pythonForTest(t)
	dir, _ := filepath.Abs(filepath.Join("..", "..", "plugins", "sdk", "tests"))
	cmd := exec.Command(py, "-m", "unittest", "-v", "test_runtime", "test_strict_timing")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the Python SDK's tests failed on %s:\n%s", runtime.GOOS, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	t.Logf("%s: %s", runtime.GOOS, lines[len(lines)-1])
}
