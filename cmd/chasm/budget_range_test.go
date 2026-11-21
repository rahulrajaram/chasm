package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/rahulrajaram/chasm/internal/protocol"
)

// TestCreateHelpRendersTheProtocolBudgetRange pins the help text's budget
// floor, default, and CPU budget to the protocol constants, so the two
// surfaces cannot drift apart silently.
func TestCreateHelpRendersTheProtocolBudgetRange(t *testing.T) {
	for _, expected := range []string{
		fmt.Sprintf("floor %d,", protocol.MinSandboxBudgetMemoryMiB),
		fmt.Sprintf("default %d;", protocol.DefaultSandboxBudgetMemoryMiB),
		fmt.Sprintf("(floor %d, default %d)", protocol.MinSandboxBudgetCPU, protocol.DefaultSandboxBudgetCPU),
	} {
		if !strings.Contains(createHelp, expected) {
			t.Fatalf("create help lacks %q:\n%s", expected, createHelp)
		}
	}
}

// TestLaunchScriptBudgetBoundsMatchProtocol parses the numeric bounds of the
// launch script's memory-budget precheck and asserts they equal the protocol
// floor and arithmetic-safety bound: the shell precheck is a user-visible
// surface that cannot import the Go values, so this test is what keeps it
// honest. There is deliberately no fixed product cap here; the launcher
// additionally enforces the host-derived cap from /proc/meminfo.
func TestLaunchScriptBudgetBoundsMatchProtocol(t *testing.T) {
	script, err := os.ReadFile(filepath.Join(moduleRoot(), "scripts", "launch-sandbox-vm.sh"))
	if err != nil {
		t.Fatal(err)
	}
	pattern := regexp.MustCompile(`-ge ([0-9]+) \] && \[ "\$BUDGET_MEMORY_MIB" -le ([0-9]+) \]`)
	match := pattern.FindSubmatch(script)
	if match == nil {
		t.Fatalf("launch script no longer carries the budget bounds precheck")
	}
	lower, err := strconv.Atoi(string(match[1]))
	if err != nil {
		t.Fatal(err)
	}
	upper, err := strconv.Atoi(string(match[2]))
	if err != nil {
		t.Fatal(err)
	}
	if lower != protocol.MinSandboxBudgetMemoryMiB || upper != protocol.SafetyBoundSandboxBudgetMemoryMiB {
		t.Fatalf("launch script budget bounds %d-%d do not match the protocol floor and safety bound %d-%d",
			lower, upper, protocol.MinSandboxBudgetMemoryMiB, protocol.SafetyBoundSandboxBudgetMemoryMiB)
	}
}
