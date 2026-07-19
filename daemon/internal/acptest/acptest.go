// Package acptest builds the fakecopilot binary used by tests to exercise
// the full ACP session flow without a real Copilot CLI.
package acptest

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Build compiles fakecopilot into a temp dir and returns the binary path.
func Build(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fakecopilot")
	cmd := exec.Command("go", "build", "-o", bin,
		"github.com/heisenberg-alt/wingman/daemon/internal/acptest/fakecopilot")
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fakecopilot: %v\n%s", err, out)
	}
	return bin
}
