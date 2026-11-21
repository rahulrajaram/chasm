package execserver

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"github.com/rahulrajaram/chasm/internal/protocol"
)

// TestBuilderShellDefaultBudgetMatchesProtocol greps the shell admission
// library's default budget constant and asserts it equals
// protocol.DefaultSandboxBudgetMemoryMiB: a running managed sandbox without
// user.<prefix>.budget-memory-mib must be accounted at the same default by
// the template builder's memory admission (scripts/lib/host-resources.sh,
// which cannot import the Go constant) and by the executor's
// sandboxBudgetFromConfig, so a template rebuild beside a keyless legacy
// sandbox never disagrees with the start admission that keeps it admissible.
func TestBuilderShellDefaultBudgetMatchesProtocol(t *testing.T) {
	script, err := os.ReadFile(filepath.Join(moduleRoot(), "scripts", "lib", "host-resources.sh"))
	if err != nil {
		t.Fatal(err)
	}
	pattern := regexp.MustCompile(`(?m)^HR_DEFAULT_SANDBOX_BUDGET_MIB=([0-9]+)\s*$`)
	match := pattern.FindSubmatch(script)
	if match == nil {
		t.Fatalf("host-resources.sh no longer carries the HR_DEFAULT_SANDBOX_BUDGET_MIB constant")
	}
	shellDefault, err := strconv.ParseInt(string(match[1]), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if shellDefault != protocol.DefaultSandboxBudgetMemoryMiB {
		t.Fatalf("shell builder default budget %d does not match protocol.DefaultSandboxBudgetMemoryMiB %d",
			shellDefault, protocol.DefaultSandboxBudgetMemoryMiB)
	}
}
