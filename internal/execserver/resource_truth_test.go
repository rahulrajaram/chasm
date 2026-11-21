package execserver

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rahulrajaram/chasm/internal/protocol"
	"github.com/rahulrajaram/chasm/internal/siteconfig"
)

// This file pins idiom 1 (one truth per resource: admission counts the
// larger of declared and enforced, and recorded writes are read back) and
// idiom 4 (caps come from the host, and refusals name the cap's source).

func mustBindDefaultGuestSite(t *testing.T) {
	t.Helper()
	site, err := siteconfig.Load(testSitePath(t, "testsite"))
	if err != nil {
		t.Fatal(err)
	}
	site.DefaultSandbox = "demo"
	if err := siteconfig.Set(site); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxMemoryBudgetCountsTheLargerOfDeclaredAndEnforced(t *testing.T) {
	mustBindDefaultGuestSite(t)
	site := siteconfig.Active()
	summary := managedSummaryForSite("sandbox-a", "Stopped")
	summary.Config[site.MetadataKey("budget-memory-mib")] = "2048"
	summary.ExpandedConfig = map[string]string{"limits.memory": "8GiB"}
	budget, err := sandboxMemoryBudget(summary)
	if err != nil {
		t.Fatal(err)
	}
	if !budget.drifted() {
		t.Fatal("a declared 2048 MiB beside an enforced 8GiB must be reported as drift")
	}
	if got := budget.effective(protocol.DefaultSandboxBudgetMemoryMiB); got != 8192 {
		t.Fatalf("admission must count the larger side: got %d MiB, want 8192 MiB", got)
	}
	// The same rule drives start admission: the target's enforced 8 GiB
	// exceeds this host's cap even though its declared key would fit.
	peer := managedSummaryForSite("sandbox-b", "Running")
	peer.Config[site.MetadataKey("budget-memory-mib")] = "1024"
	peer.ExpandedConfig = map[string]string{"limits.memory": "2048MiB"}
	_, err = admitSandboxStartWith("sandbox-a", []incusInstanceSummary{summary, peer}, 8192)
	var protocolErr *ProtocolError
	if !errors.As(err, &protocolErr) || protocolErr.Code != "resource_exhausted" {
		t.Fatalf("admission must refuse on the enforced value: err=%v", err)
	}
}

// TestAdmissionCountsEnforcedMemoryWhenTheDeclaredKeyIsAbsent
// (below) covers the larger-side rule; this one pins the fallback rule:
// the 3072 default is used only when neither side is present. Mutation
// checked: starting from the fallback and only replacing it upward (the
// old effective()) fails the 1024 case.
func TestEffectiveFallsBackOnlyWhenNeitherSideIsPresent(t *testing.T) {
	declaredAbsentEnforcedLow := sandboxResourceBudget{enforced: 1024, enforcedPresent: true}
	if got := declaredAbsentEnforcedLow.effective(protocol.DefaultSandboxBudgetMemoryMiB); got != 1024 {
		t.Fatalf("declared absent, enforced 1024: effective = %d, want 1024", got)
	}
	neither := sandboxResourceBudget{}
	if got := neither.effective(3072); got != 3072 {
		t.Fatalf("neither side: effective = %d, want the 3072 fallback", got)
	}
	bothPresent := sandboxResourceBudget{declared: 2048, declaredPresent: true, enforced: 1024, enforcedPresent: true}
	if got := bothPresent.effective(3072); got != 2048 {
		t.Fatalf("both sides: effective = %d, want the larger 2048", got)
	}
}

func TestAdmissionCountsEnforcedMemoryWhenTheDeclaredKeyIsAbsent(t *testing.T) {
	mustBindDefaultGuestSite(t)
	summary := managedSummaryForSite("sandbox-a", "Stopped")
	summary.ExpandedConfig = map[string]string{"limits.memory": "9GiB"}
	budget, err := sandboxMemoryBudget(summary)
	if err != nil {
		t.Fatal(err)
	}
	if budget.declaredPresent || !budget.enforcedPresent || budget.drifted() {
		t.Fatalf("declared=%v enforced=%v drift=%v", budget.declaredPresent, budget.enforcedPresent, budget.drifted())
	}
	if got := budget.effective(protocol.DefaultSandboxBudgetMemoryMiB); got != 9216 {
		t.Fatalf("admission must fall back to the enforced ceiling: got %d MiB, want 9216 MiB", got)
	}
}

func TestSandboxMemoryBudgetTreatsMissingExpandedConfigAsAbsent(t *testing.T) {
	mustBindDefaultGuestSite(t)
	summary := managedSummaryForSite("sandbox-a", "Stopped")
	summary.Config[siteconfig.Active().MetadataKey("budget-memory-mib")] = "4096"
	budget, err := sandboxMemoryBudget(summary)
	if err != nil {
		t.Fatal(err)
	}
	if !budget.declaredPresent || budget.enforcedPresent || budget.drifted() {
		t.Fatalf("declared=%v enforced=%v drift=%v", budget.declaredPresent, budget.enforcedPresent, budget.drifted())
	}
}

// TestSandboxMemoryBudgetMarksUnsupportedEnforcedValue pins the parse
// contract: a percentage is not a supported size, the budget carries the
// reason naming the value, and admission still fails closed on it.
// Mutation checked: accepting percentages (or dropping the reason's value)
// fails the reason assertion; making admissibleMemoryBudget admit the
// keyless fallback fails the refusal assertion.
func TestSandboxMemoryBudgetMarksUnsupportedEnforcedValue(t *testing.T) {
	mustBindDefaultGuestSite(t)
	summary := managedSummaryForSite("sandbox-a", "Stopped")
	summary.ExpandedConfig = map[string]string{"limits.memory": "50%"}
	budget, err := sandboxMemoryBudget(summary)
	if err != nil {
		t.Fatalf("the collector must not fail on an unsupported value: %v", err)
	}
	if budget.enforcedPresent || budget.enforcedUnsupported == "" {
		t.Fatalf("an unsupported enforced value must be marked unsupported: %+v", budget)
	}
	if budget.enforcedUnsupported != "limits.memory '50%' is not a supported size" {
		t.Fatalf("the reason must name the value: %q", budget.enforcedUnsupported)
	}
	if _, err := admissibleMemoryBudget(summary); err == nil {
		t.Fatal("admission must still fail closed on an unsupported enforced value")
	}
}

// TestParseMemoryLimitBytesGrammar pins the exact accepted grammar: plain
// bytes, binary suffixes (KiB/MiB/GiB/TiB, 1024-based), and decimal
// suffixes (kB/KB/MB/GB/TB, 1000-based) as Incus accepts; a percentage or
// anything else is unsupported.
func TestParseMemoryLimitBytesGrammar(t *testing.T) {
	tests := []struct {
		value string
		want  int64
		fails bool
	}{
		{value: "1024", want: 1024},
		{value: "3GiB", want: 3 << 30},
		{value: "2MiB", want: 2 << 20},
		{value: "5KiB", want: 5 << 10},
		{value: "1TiB", want: 1 << 40},
		{value: "1GB", want: 1000 * 1000 * 1000},
		{value: "1kB", want: 1000},
		{value: "1KB", want: 1000},
		{value: "1MB", want: 1000 * 1000},
		{value: "1TB", want: 1000 * 1000 * 1000 * 1000},
		{value: "50%", fails: true},
		{value: "3g", fails: true},
		{value: "GiB", fails: true},
		{value: "", fails: true},
		{value: "-4MiB", fails: true},
	}
	for _, testCase := range tests {
		got, err := parseMemoryLimitBytes(testCase.value)
		if testCase.fails {
			if err == nil {
				t.Fatalf("%q must be unsupported, got %d", testCase.value, got)
			}
			continue
		}
		if err != nil || got != testCase.want {
			t.Fatalf("%q: got %d err=%v want %d", testCase.value, got, err, testCase.want)
		}
	}
}

func TestParseCpuLimitCount(t *testing.T) {
	tests := []struct {
		limit string
		want  int64
		fails bool
	}{
		{limit: "2", want: 2},
		{limit: "8", want: 8},
		{limit: "0-3", want: 4},
		{limit: "0,2", want: 2},
		{limit: "1,2,3", want: 3},
		{limit: "50%", fails: true},
		{limit: "2-1", fails: true},
		{limit: "", fails: true},
	}
	for _, testCase := range tests {
		got, err := parseCpuLimitCount(testCase.limit)
		if testCase.fails {
			if err == nil {
				t.Fatalf("limit %q must be unparseable, got %d", testCase.limit, got)
			}
			continue
		}
		if err != nil || got != testCase.want {
			t.Fatalf("limit %q: got %d err=%v want %d", testCase.limit, got, err, testCase.want)
		}
	}
}

func TestSandboxCpuBudgetDerivesDeclaredAndEnforced(t *testing.T) {
	mustBindDefaultGuestSite(t)
	site := siteconfig.Active()
	summary := managedSummaryForSite("sandbox-a", "Stopped")
	summary.Config[site.MetadataKey("budget-cpu")] = "2"
	summary.ExpandedConfig = map[string]string{"limits.cpu": "4"}
	budget, err := sandboxCpuBudget(summary)
	if err != nil {
		t.Fatal(err)
	}
	if !budget.drifted() {
		t.Fatal("a declared 2 beside an enforced 4 must be reported as CPU drift")
	}
	if budget.effective(0) != 4 {
		t.Fatalf("the larger side must win for CPU too: got %d", budget.effective(0))
	}
	bad := managedSummaryForSite("sandbox-b", "Stopped")
	bad.ExpandedConfig = map[string]string{"limits.cpu": "50%"}
	budget, err = sandboxCpuBudget(bad)
	if err != nil {
		t.Fatal(err)
	}
	if budget.enforcedPresent || budget.enforcedUnsupported != "limits.cpu '50%' is not a supported CPU count" {
		t.Fatalf("an unsupported enforced CPU value must be marked with its reason: %+v", budget)
	}
}

// TestAdmissionRefusesOnADriftedRunningPeerAlone pins the peer rule: a
// running peer counted at the larger of declared and enforced is what
// pushes the sum over the cap — its declared key alone would admit.
// Mutation checked: counting peers at the declared key only (the old
// sandboxBudgetFromConfig) makes the refusing case admit while the mirror
// case still passes.
func TestAdmissionRefusesOnADriftedRunningPeerAlone(t *testing.T) {
	mustBindDefaultGuestSite(t)
	site := siteconfig.Active()
	target := managedSummaryForSite("sandbox-a", "Stopped")
	target.Config[site.MetadataKey("budget-memory-mib")] = "4096"
	target.ExpandedConfig = map[string]string{"limits.memory": "4096MiB"}
	driftedPeer := managedSummaryForSite("sandbox-b", "Running")
	driftedPeer.Config[site.MetadataKey("budget-memory-mib")] = "1024"
	driftedPeer.ExpandedConfig = map[string]string{"limits.memory": "4096MiB"}
	_, err := admitSandboxStartWith("sandbox-a", []incusInstanceSummary{target, driftedPeer}, 8192)
	var protocolErr *ProtocolError
	if !errors.As(err, &protocolErr) || protocolErr.Code != "resource_exhausted" {
		t.Fatalf("the drifted peer alone must push the sum over the cap: %v", err)
	}
	if !strings.Contains(protocolErr.Message, "running sandbox budgets 4096 MiB + requested 4096 MiB") {
		t.Fatalf("the refusal must name the counted peer budget: %q", protocolErr.Message)
	}
	// The mirror case: an undrifted peer with the same declared key admits.
	honestPeer := managedSummaryForSite("sandbox-b", "Running")
	honestPeer.Config[site.MetadataKey("budget-memory-mib")] = "1024"
	honestPeer.ExpandedConfig = map[string]string{"limits.memory": "1024MiB"}
	if _, err := admitSandboxStartWith("sandbox-a", []incusInstanceSummary{target, honestPeer}, 8192); err != nil {
		t.Fatalf("the honest peer must admit: %v", err)
	}
}

func TestHostCapRefusalNamesTheCapSource(t *testing.T) {
	err := hostCapRefusalError(70000, 62896, 63920, 1024)
	var protocolErr *ProtocolError
	if !errors.As(err, &protocolErr) || protocolErr.Code != "resource_exhausted" {
		t.Fatalf("err=%v is not resource_exhausted", err)
	}
	for _, expected := range []string{
		"budget 70000 MiB exceeds this host's cap 62896 MiB",
		"MemTotal 63920 MiB",
		"host_reserve_mib 1024",
	} {
		if !strings.Contains(protocolErr.Message, expected) {
			t.Fatalf("refusal %q lacks %q", protocolErr.Message, expected)
		}
	}
	if err := hostCapRefusalError(70000, 70000, 63920, 1024); err != nil {
		t.Fatalf("a budget at the cap fits: err=%v", err)
	}
}

func TestRefuseCpuBudgetAboveHostCapNamesTheSource(t *testing.T) {
	if err := refuseCpuBudgetAboveHostCap(0); err == nil {
		t.Fatal("CPU budget 0 is not a valid budget")
	}
	if err := refuseCpuBudgetAboveHostCap(700); err == nil {
		t.Fatal("an absurd CPU budget must be refused")
	} else {
		var protocolErr *ProtocolError
		if !errors.As(err, &protocolErr) || !strings.Contains(protocolErr.Message, "host CPU count") {
			t.Fatalf("CPU refusal must name its source: %v", err)
		}
	}
	if err := refuseCpuBudgetAboveHostCap(1); err != nil {
		t.Fatalf("one CPU fits every host: err=%v", err)
	}
}

func TestSetSandboxBudgetReadsBackWhatItWrote(t *testing.T) {
	mustBindDefaultGuestSite(t)
	site := siteconfig.Active()
	original := runIncus
	t.Cleanup(func() { runIncus = original })
	written := ""
	disagreed := false
	runIncus = func(arguments []string, _ io.Reader, stdout io.Writer, _ []*os.File, _ time.Duration) (int, error) {
		switch {
		case len(arguments) > 4 && arguments[0] == "config" && arguments[1] == "set" && arguments[3] == site.MetadataKey("budget-memory-mib"):
			written = arguments[4]
		case len(arguments) > 3 && arguments[0] == "config" && arguments[1] == "get" && arguments[3] == site.MetadataKey("budget-memory-mib"):
			value := written
			if disagreed {
				value = "999"
			}
			if value == "" {
				return 1, nil
			}
			fmt.Fprint(stdout, value+"\n")
		}
		return 0, nil
	}
	if err := setSandboxBudget("sandbox-a", 2048); err != nil {
		t.Fatalf("a matching read back must succeed: err=%v", err)
	}
	if written != "2048" {
		t.Fatalf("the key was not written: got %q", written)
	}
	disagreed = true
	if err := setSandboxBudget("sandbox-a", 2048); err == nil {
		t.Fatal("an unproven budget write must fail closed")
	} else {
		var protocolErr *ProtocolError
		if !errors.As(err, &protocolErr) || protocolErr.Code != "postcondition_failed" {
			t.Fatalf("refusal must be the budget postcondition failure: %v", err)
		}
	}
}

// TestSetSandboxCpuBudgetReadsBackWhatItWrote pins the CPU read-back
// proof: a matching read-back succeeds; a disagreeing one is a
// postcondition failure; an empty one is an operation failure.
// Mutation checked: deleting the postcondition check or the empty-read
// check in setSandboxCpuBudget fails exactly one case.
func TestSetSandboxCpuBudgetReadsBackWhatItWrote(t *testing.T) {
	mustBindDefaultGuestSite(t)
	site := siteconfig.Active()
	original := runIncus
	t.Cleanup(func() { runIncus = original })
	written := ""
	disagreed := false
	emptyRead := false
	runIncus = func(arguments []string, _ io.Reader, stdout io.Writer, _ []*os.File, _ time.Duration) (int, error) {
		if emptyRead {
			return 0, nil
		}
		switch {
		case len(arguments) > 4 && arguments[0] == "config" && arguments[1] == "set" && arguments[3] == site.MetadataKey("budget-cpu"):
			written = arguments[4]
		case len(arguments) > 3 && arguments[0] == "config" && arguments[1] == "get" && arguments[3] == site.MetadataKey("budget-cpu"):
			value := written
			if disagreed {
				value = "7"
			}
			if value == "" {
				return 0, nil
			}
			fmt.Fprint(stdout, value+"\n")
		}
		return 0, nil
	}
	if err := setSandboxCpuBudget("sandbox-a", 2); err != nil {
		t.Fatalf("a matching read back must succeed: err=%v", err)
	}
	if written != "2" {
		t.Fatalf("the CPU key was not written: got %q", written)
	}
	disagreed = true
	if err := setSandboxCpuBudget("sandbox-a", 2); err == nil {
		t.Fatal("an unproven CPU budget write must fail closed")
	} else {
		var protocolErr *ProtocolError
		if !errors.As(err, &protocolErr) || protocolErr.Code != "postcondition_failed" {
			t.Fatalf("a disagreeing read back must be the postcondition failure: %v", err)
		}
	}
	disagreed = false
	emptyRead = true
	if err := setSandboxCpuBudget("sandbox-a", 2); err == nil {
		t.Fatal("an empty CPU read back must fail closed")
	} else {
		var protocolErr *ProtocolError
		if !errors.As(err, &protocolErr) || protocolErr.Code != "operation_failed" {
			t.Fatalf("an empty read back must be the operation failure: %v", err)
		}
	}
}

// TestAdoptBudgetValuePreservesTheEnforcedTruth pins the adopt/re-create
// rule: with no declared key and no requested value, the ENFORCED value is
// recorded as the budget and the limit is left unchanged; only when
// nothing is enforced is the default used. Mutation checked: recording the
// default instead of the enforced value (the old behavior that shrank a
// limits.cpu 14 sandbox back to 2) fails the preserved cases.
func TestAdoptBudgetValuePreservesTheEnforcedTruth(t *testing.T) {
	// limits.cpu 14 with no key => 14 preserved.
	if got := adoptBudgetValue(0, 14, true, protocol.ValidCpuBudget, protocol.DefaultSandboxBudgetCPU); got != 14 {
		t.Fatalf("cpu 14 enforced with no key: adopted %d, want 14", got)
	}
	// limits.memory 8GiB with no key => 8192 preserved.
	if got := adoptBudgetValue(0, 8192, true, protocol.ValidMemoryBudget, protocol.DefaultSandboxBudgetMemoryMiB); got != 8192 {
		t.Fatalf("memory 8192 enforced with no key: adopted %d, want 8192", got)
	}
	// A requested value still wins.
	if got := adoptBudgetValue(2048, 8192, true, protocol.ValidMemoryBudget, protocol.DefaultSandboxBudgetMemoryMiB); got != 2048 {
		t.Fatalf("requested value must win: adopted %d, want 2048", got)
	}
	// Nothing enforced: the default.
	if got := adoptBudgetValue(0, 0, false, protocol.ValidMemoryBudget, protocol.DefaultSandboxBudgetMemoryMiB); got != protocol.DefaultSandboxBudgetMemoryMiB {
		t.Fatalf("nothing enforced must use the default: adopted %d", got)
	}
	// An enforced value outside the valid range falls back to the default.
	if got := adoptBudgetValue(0, 100, true, protocol.ValidMemoryBudget, protocol.DefaultSandboxBudgetMemoryMiB); got != protocol.DefaultSandboxBudgetMemoryMiB {
		t.Fatalf("an out-of-range enforced value must use the default: adopted %d", got)
	}
}

// TestCreateSandboxRefusesCPUAboveTheInjectedHostCount pins the create-side
// wiring of the host CPU-count cap: with the host count injected at 16, a
// create with --cpu 64 (between the cap and the protocol bound of 1024) is
// refused before any Incus call, naming "host CPU count". Mutation checked:
// deleting the refuseCpuBudgetAboveHostCap call in createSandbox fails this
// test because the create proceeds to its inventory read instead.
func TestCreateSandboxRefusesCPUAboveTheInjectedHostCount(t *testing.T) {
	mustBindDefaultGuestSite(t)
	original := hostCpuCount
	t.Cleanup(func() { hostCpuCount = original })
	hostCpuCount = func() int64 { return 16 }
	_, err := createSandbox("sandbox-research", 0, 64)
	var protocolErr *ProtocolError
	if !errors.As(err, &protocolErr) || protocolErr.Code != "resource_exhausted" || !strings.Contains(protocolErr.Message, "host CPU count") {
		t.Fatalf("create must refuse a CPU budget above the host count naming its source: %v", err)
	}
}

// TestCreateSandboxAdoptPathRecordsTheEnforcedTruth drives the real
// createSandbox over an existing managed sandbox whose budget keys are
// absent while Incus enforces limits.cpu 14 and limits.memory 8GiB: the
// adopt path must record 14 / 8192 (the enforced truth), apply exactly
// those limits, and never write the protocol defaults. Mutation checked:
// recording the default instead of the enforced value (3072 / 2) fails the
// recorded-value assertions.
func TestCreateSandboxAdoptPathRecordsTheEnforcedTruth(t *testing.T) {
	mustBindDefaultGuestSite(t)
	site := siteconfig.Active()
	summary := managedSummaryForSite("sandbox-research", "Stopped")
	summary.ExpandedConfig = map[string]string{"limits.memory": "8GiB", "limits.cpu": "14"}

	originalList, originalCheck, originalInit, originalInspect := listInstanceStatuses, requireManagedSandbox, sandboxInitialized, inspectSandbox
	t.Cleanup(func() {
		listInstanceStatuses, requireManagedSandbox, sandboxInitialized, inspectSandbox = originalList, originalCheck, originalInit, originalInspect
	})
	listInstanceStatuses = func() ([]incusInstanceSummary, error) {
		return []incusInstanceSummary{summary}, nil
	}
	requireManagedSandbox = func(string) error { return nil }
	sandboxInitialized = func(string) bool { return true }
	inspectSandbox = func(string) (map[string]interface{}, error) {
		return map[string]interface{}{"instance": "sandbox-research"}, nil
	}

	original := runIncus
	t.Cleanup(func() { runIncus = original })
	config := map[string]string{}
	var recorded [][]string
	runIncus = func(arguments []string, _ io.Reader, stdout io.Writer, _ []*os.File, _ time.Duration) (int, error) {
		switch {
		case len(arguments) == 5 && arguments[0] == "config" && arguments[1] == "set":
			config[arguments[3]] = arguments[4]
			recorded = append(recorded, append([]string(nil), arguments...))
		case len(arguments) == 4 && arguments[0] == "config" && arguments[1] == "get":
			if _, ok := config[arguments[3]]; !ok {
				return 1, nil
			}
			_, _ = stdout.Write([]byte(config[arguments[3]] + "\n"))
		default:
			t.Fatalf("unexpected incus argv: %v", arguments)
		}
		return 0, nil
	}

	result, err := createSandbox("sandbox-research", 0, 0)
	if err != nil {
		t.Fatalf("the adopt path must record the enforced truth: %v", err)
	}
	// The recorded budget keys carry the enforced values, never the
	// protocol defaults (3072 / 2): no recorded write may carry either
	// default value. Mutation-checked alongside the assertions above.
	for _, argv := range recorded {
		value := argv[len(argv)-1]
		if value == "3072" || value == "2" {
			t.Fatalf("the adopt path recorded the protocol default %q: %v", value, argv)
		}
	}
	if got := config[site.MetadataKey("budget-memory-mib")]; got != "8192" {
		t.Fatalf("recorded memory budget = %q, want the enforced 8192", got)
	}
	if got := config[site.MetadataKey("budget-cpu")]; got != "14" {
		t.Fatalf("recorded CPU budget = %q, want the enforced 14", got)
	}
	// The enforced limits stay exactly what Incus already applied.
	if got := config["limits.memory"]; got != "8192MiB" {
		t.Fatalf("applied limits.memory = %q, want 8192MiB", got)
	}
	if got := config["limits.cpu"]; got != "14" {
		t.Fatalf("applied limits.cpu = %q, want 14", got)
	}
	if result["created"] != false {
		t.Fatalf("an adopted sandbox must report created=false: %v", result)
	}
}
