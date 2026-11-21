package execserver

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// admissionParityRow is one shared fixture for the builder's shell memory
// admission (running_sandbox_budgets_mib in scripts/lib/host-resources.sh)
// and the executor's Go start admission (admissibleMemoryBudget): the same
// declared budget key and enforced expanded limits.memory must yield the
// same counted MiB, or the same refusal, on both sides (idiom 1: admission
// counts the larger of the recorded and the enforced value).
type admissionParityRow struct {
	name     string
	declared string
	enforced string
	want     int64
	refuse   bool
}

var admissionParityRows = []admissionParityRow{
	{name: "declared only", declared: "2048", enforced: "", want: 2048},
	{name: "declared above enforced", declared: "2048", enforced: "1GiB", want: 2048},
	{name: "enforced above declared", declared: "2048", enforced: "48GiB", want: 49152},
	{name: "keyless and limit-free defaults", declared: "", enforced: "", want: 3072},
	// Unsupported spellings fail closed on both sides, with the value named.
	{name: "percentage enforced refuses", declared: "20480", enforced: "50%", refuse: true},
	{name: "percentage enforced without a key refuses", declared: "", enforced: "50%", refuse: true},
	// SI-unit rows: the shell contract counts them; the Go parser is widened
	// to the same grammar in the parallel commit.
	{name: "decimal MB enforced", declared: "2048", enforced: "8GB", want: 7630},
	{name: "TiB enforced within the bound", declared: "2048", enforced: "512GiB", want: 524288},
	// Leading zeros and signed values are outside the shared grammar: both
	// sides refuse them.
	{name: "leading-zero enforced refuses", declared: "2048", enforced: "04GiB", refuse: true},
	{name: "leading-zero plain bytes refuse", declared: "2048", enforced: "007", refuse: true},
	{name: "negative enforced refuses", declared: "2048", enforced: "-4", refuse: true},
	{name: "leading-space enforced refuses", declared: "2048", enforced: " 4GiB", refuse: true},
	{name: "plus-signed enforced refuses", declared: "2048", enforced: "+4", refuse: true},
	// An enforced value that parses above the shared 1048576 MiB bound (or
	// overflows the parser outright) is unsupported on both sides.
	{name: "enforced above the safety bound refuses", declared: "2048", enforced: "2TiB", refuse: true},
	{name: "absurd enforced refuses", declared: "2048", enforced: "9999999999999999TB", refuse: true},
}

func shellAdmissionCommand(t *testing.T, directory, library, body string) *exec.Cmd {
	t.Helper()
	path := filepath.Join(directory, "admission-test.sh")
	script := "#!/bin/sh\n. \"$CHASM_TEST_RESOURCE_LIB\"\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(path)
	command.Env = append(os.Environ(), "CHASM_TEST_RESOURCE_LIB="+library)
	return command
}

// shellSandboxBudgetMib runs one row through the real shell admission
// library with a fake incus on PATH, exactly as the provision harness does,
// and returns the counted MiB; refused reports a fail-closed run.
func shellSandboxBudgetMib(t *testing.T, row admissionParityRow) (mib int64, refused bool) {
	t.Helper()
	work := t.TempDir()
	bin := filepath.Join(work, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	var expandedKeys string
	if row.declared != "" {
		// Real Incus expands instance config into expanded_config, where
		// both readers look for the declared key; the CSV -c columns read
		// the same expanded values.
		expandedKeys = row.declared
	}
	// The CSV row is name,status,managed,budget,limits.memory (values
	// contain no commas), with empty fields for absent sides.
	csvRow := "sandbox-demo,Running,true," + expandedKeys + "," + row.enforced
	// The fake inventory is one Running managed sandbox carrying exactly the
	// row's declared key and enforced limit, served as the flat CSV the
	// admission reads. The fake refuses any list argv other than the exact
	// `list -c n,s,user.chasm.managed,user.chasm.budget-memory-mib,
	// limits.memory --format csv`, so a mutated column list — the
	// limits.memory column included — cannot pass silently.
	incus := "#!/bin/sh\n" +
		`if [ "$#" -eq 5 ] && [ "$1" = list ] && [ "$2" = -c ] && [ "$3" = "n,s,user.chasm.managed,user.chasm.budget-memory-mib,limits.memory" ] && [ "$4" = --format ] && [ "$5" = csv ]; then` + "\n" +
		"printf '%s\\n' '" + csvRow + "'\n" +
		"    exit 0\nfi\nexit 9\n"
	script := filepath.Join(bin, "incus")
	if err := os.WriteFile(script, []byte(incus), 0o755); err != nil {
		t.Fatal(err)
	}
	lib := filepath.Join(moduleRoot(), "scripts", "lib", "host-resources.sh")
	if _, err := os.Stat(lib); err != nil {
		t.Fatal(err)
	}
	cmd := shellAdmissionCommand(t, work, lib, "running_sandbox_budgets_mib chasm")
	cmd.Env = append(cmd.Env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	// stdout carries the counted sum; the protocol-default note and the
	// refusal reasons go to stderr and must not be parsed as a number.
	out, err := cmd.Output()
	if err != nil {
		return 0, true
	}
	value, parseErr := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if parseErr != nil {
		t.Fatalf("shell admission printed %q for row %q", string(out), row.name)
	}
	return value, false
}

// TestAdmissionShellInventoryArgvCarriesLimitsMemory pins the exact argv
// of the shell admission's inventory read: one `incus list -c n,s,<managed
// key>,<budget key>,limits.memory --format csv` call. Mutation checked:
// dropping the limits.memory column (or the -c/--format flags) from the
// library's argv fails the fake's refusal and this test.
func TestAdmissionShellInventoryArgvCarriesLimitsMemory(t *testing.T) {
	work := t.TempDir()
	bin := filepath.Join(work, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(work, "argv")
	incus := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + log + "\n" +
		`if [ "$#" -eq 5 ] && [ "$1" = list ] && [ "$2" = -c ] && [ "$3" = "n,s,user.chasm.managed,user.chasm.budget-memory-mib,limits.memory" ] && [ "$4" = --format ] && [ "$5" = csv ]; then` + "\n" +
		"    exit 0\nfi\nexit 9\n"
	script := filepath.Join(bin, "incus")
	if err := os.WriteFile(script, []byte(incus), 0o755); err != nil {
		t.Fatal(err)
	}
	lib := filepath.Join(moduleRoot(), "scripts", "lib", "host-resources.sh")
	cmd := shellAdmissionCommand(t, work, lib, "running_sandbox_budgets_mib chasm >/dev/null 2>&1")
	cmd.Env = append(cmd.Env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := cmd.Run(); err != nil {
		t.Fatalf("the shell admission must run the pinned inventory argv: %v", err)
	}
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "list ") {
			want := "list -c n,s,user.chasm.managed,user.chasm.budget-memory-mib,limits.memory --format csv"
			if line != want {
				t.Fatalf("inventory argv = %q, want %q", line, want)
			}
			return
		}
	}
	t.Fatalf("the admission issued no incus list call; argv log: %q", string(calls))
}

// TestAdmissionCountParityWithShellBuilder runs every shared fixture row
// through both admission implementations and refuses any disagreement: the
// template builder and the executor start gate must count the same inventory
// identically, or refuse it identically.
func TestAdmissionCountParityWithShellBuilder(t *testing.T) {
	for _, row := range admissionParityRows {
		row := row
		t.Run(row.name, func(t *testing.T) {
			summary := incusInstanceSummary{
				Name:   "sandbox-demo",
				Status: "Running",
				Config: map[string]string{"user.chasm.managed": "true"},
				ExpandedConfig: map[string]string{
					"user.chasm.managed": "true",
				},
			}
			if row.declared != "" {
				summary.Config[budgetMetadataKey()] = row.declared
			}
			if row.enforced != "" {
				summary.ExpandedConfig["limits.memory"] = row.enforced
			}
			goBudget, goErr := admissibleMemoryBudget(summary)
			shellBudget, shellRefused := shellSandboxBudgetMib(t, row)

			if row.refuse {
				if goErr == nil {
					t.Fatalf("Go admission counted %d MiB for row %q; want refusal", goBudget, row.name)
				}
				if !shellRefused {
					t.Fatalf("shell admission counted %d MiB for row %q; want refusal", shellBudget, row.name)
				}
				return
			}
			if goErr != nil {
				t.Fatalf("Go admission refused row %q: %v", row.name, goErr)
			}
			if shellRefused {
				t.Fatalf("shell admission refused row %q; want %d MiB", row.name, row.want)
			}
			if goBudget != row.want || shellBudget != row.want {
				t.Fatalf("row %q: Go admission counted %d MiB, shell counted %d MiB, want %d MiB",
					row.name, goBudget, shellBudget, row.want)
			}
		})
	}
}
