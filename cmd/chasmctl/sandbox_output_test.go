package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHumanSandboxStatusRendersDetailsAndServiceHealth(t *testing.T) {
	result := map[string]interface{}{
		"instance": "sandbox-demo", "instance_status": "Running", "instance_pid": float64(55684),
		"services": []interface{}{
			map[string]interface{}{"name": "incus.service", "active_state": "active", "result": "success", "n_restarts": float64(0)},
		},
	}
	var output bytes.Buffer
	handled, err := renderHumanResult("sandbox-status", result, &output, humanOutputStyle{Width: 100})
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	text := output.String()
	for _, expected := range []string{"sandbox-demo", "● running", "55684", "SERVICES", "incus.service", "1 service · 1 healthy"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("output lacks %q:\n%s", expected, text)
		}
	}
	if strings.HasPrefix(text, "{") {
		t.Fatalf("human output leaked JSON:\n%s", text)
	}
}

func TestHumanSandboxDetailRendersManagedMetadata(t *testing.T) {
	result := map[string]interface{}{
		"instance": "sandbox-research", "logical_name": "research", "instance_status": "Stopped", "instance_pid": float64(0),
		"description": "LLM evaluation", "workspace": "/workspace", "shared": "/shared",
	}
	var output bytes.Buffer
	handled, err := renderHumanResult("inspect-sandbox", result, &output, humanOutputStyle{Width: 100})
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	text := output.String()
	for _, expected := range []string{"research", "○ stopped", "managed", "LLM evaluation"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("output lacks %q:\n%s", expected, text)
		}
	}
	if strings.Contains(text, "PID") {
		t.Fatalf("stopped sandbox should not display a zero PID:\n%s", text)
	}
}

func TestHumanSandboxDetailRendersPausedForFrozenInstance(t *testing.T) {
	// A FROZEN instance (after `chasm pause`) must render as paused, never
	// as stopped: show, status, and list all share styledState.
	result := map[string]interface{}{
		"instance": "sandbox-hold", "logical_name": "hold", "instance_status": "Frozen", "instance_pid": float64(0),
		"description": "", "workspace": "/workspace", "shared": "/shared",
	}
	var output bytes.Buffer
	handled, err := renderHumanResult("inspect-sandbox", result, &output, humanOutputStyle{Width: 100})
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	text := output.String()
	if !strings.Contains(text, "hold") || !strings.Contains(text, "◐ paused") {
		t.Fatalf("output lacks the paused rendering:\n%s", text)
	}
	if strings.Contains(text, "stopped") || strings.Contains(text, "PID") {
		t.Fatalf("frozen sandbox rendered as stopped or leaked a PID:\n%s", text)
	}
}

func TestHumanSandboxLayoutRendersObservation(t *testing.T) {
	result := map[string]interface{}{
		"instance": "sandbox-research", "logical_name": "research", "root": "/workspace", "depth": float64(3),
		"freshness": "live", "observed_at": "2026-09-02T23:00:00Z", "complete": true,
		"entries": []interface{}{
			map[string]interface{}{"path": "/workspace/example-app", "type": "directory"},
			map[string]interface{}{"path": "/workspace/README.md", "type": "file"},
		},
	}
	var output bytes.Buffer
	handled, err := renderHumanResult("sandbox-layout", result, &output, humanOutputStyle{Width: 100})
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	text := output.String()
	for _, expected := range []string{"research", "Freshness", "live", "Observed", "Root", "/workspace", "TYPE", "/workspace/example-app", "2 entries · live"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("output lacks %q:\n%s", expected, text)
		}
	}
}

func TestHumanSandboxLayoutExplainsUnobservedState(t *testing.T) {
	result := map[string]interface{}{
		"instance": "sandbox-research", "logical_name": "research", "root": "/workspace", "depth": float64(3),
		"freshness": "unobserved", "observed_at": "", "complete": false, "entries": []interface{}{},
	}
	var output bytes.Buffer
	handled, err := renderHumanResult("sandbox-layout", result, &output, humanOutputStyle{Width: 100})
	if err != nil || !handled || !strings.Contains(output.String(), "No filesystem observation is available.") ||
		!strings.Contains(output.String(), "0 entries · unobserved") {
		t.Fatalf("handled=%v err=%v output=%q", handled, err, output.String())
	}
}

func TestHumanSandboxQueriesRejectMalformedResponses(t *testing.T) {
	tests := []struct {
		command string
		result  map[string]interface{}
	}{
		{"sandbox-status", map[string]interface{}{"instance": "sandbox-demo", "instance_status": "Running", "instance_pid": float64(0), "services": []interface{}{}}},
		{"inspect-sandbox", map[string]interface{}{"logical_name": "research", "instance_status": "Unknown", "instance_pid": float64(0), "description": ""}},
		{"inspect-sandbox", map[string]interface{}{"logical_name": "research", "instance_status": "Stopped", "instance_pid": float64(0), "description": "", "workspace": "/wrong", "shared": "/shared"}},
		{"inspect-sandbox", map[string]interface{}{"logical_name": "research", "instance_status": "Stopped", "instance_pid": "zero", "description": "", "workspace": "/workspace", "shared": "/shared"}},
		{"sandbox-layout", map[string]interface{}{"logical_name": "research", "root": "/workspace", "depth": float64(3), "freshness": "unobserved", "observed_at": "", "complete": true, "entries": []interface{}{}}},
	}
	for _, testCase := range tests {
		var output bytes.Buffer
		handled, err := renderHumanResult(testCase.command, testCase.result, &output, humanOutputStyle{Width: 100})
		if !handled || err == nil || output.Len() != 0 {
			t.Fatalf("command=%s handled=%v err=%v output=%q", testCase.command, handled, err, output.String())
		}
	}
}

// goldenSandboxResult loads the shared golden fixture — a JSON round-trip
// of sandboxShowViewsWith output for the canonical drift scenario, committed
// at testdata/golden/sandbox-show-resources.json and pinned on the producer
// side by TestGoldenShowResourcesFixtureMatchesProducer — and wraps it the
// way an inspect result carries it. Because the fixture is produced by the
// collector, renderer drift from the producer's field names fails here.
func goldenSandboxResult(t *testing.T) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "golden", "sandbox-show-resources.json"))
	if err != nil {
		t.Fatal(err)
	}
	var views map[string]interface{}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&views); err != nil {
		t.Fatal(err)
	}
	return map[string]interface{}{
		"instance": "sandbox-development", "logical_name": "development", "instance_status": "Running", "instance_pid": int64(4242),
		"description": "LLM evaluation", "workspace": "/workspace", "shared": "/shared",
		"resources": views,
	}
}

// TestHumanSandboxDetailRendersResourcesAndDrift is the golden rendering:
// the Resources section lists each view with its source, the drift line
// names the resize fix, and the host warning prints. Mutation checked:
// dropping the Resources section, the drift line, or the warning loop each
// leave a distinct assertion failing.
func TestHumanSandboxDetailRendersResourcesAndDrift(t *testing.T) {
	var output bytes.Buffer
	handled, err := renderHumanResult("inspect-sandbox", goldenSandboxResult(t), &output, humanOutputStyle{Width: 200})
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	text := output.String()
	expected := []string{
		"RESOURCES",
		"Memory       declared 20480 MiB · enforced 49152 MiB · guest 47.0 GiB total, 3.0 GiB used · host 24.3 GiB resident · cap 61.4 GiB",
		"CPU          declared 14 · enforced 14 · host 20",
		"Root disk    150 GiB (instance override) · pool holds 3.1 GiB",
		"Workspace    218.0 GiB used · quota 218.0 GiB · pool chasm-test 900 GiB, 273 GiB used",
		"Pool         chasm-test · 900 GiB · 273 GiB used · loop file 266 GiB allocated of 900 GiB apparent · 19.5 GiB free behind it",
		"Host         MemTotal 62.4 GiB · available 19.5 GiB · reserve 1024 MiB · admitted 48 GiB · host's own use 18.6 GiB",
		"drift: memory record 20480 MiB, enforced 49152 MiB; fix: chasm resize development --memory 49152",
		"warning: the pool's configured size 921600 MiB exceeds its loop file's 272384 MiB allocated plus 20000 MiB free behind it; the pool could not be filled; free at least 629216 MiB more on the filesystem holding /var/lib/incus/disks/chasm-test.img",
		"warning: the host is using 19036 MiB for itself but host_reserve_mib is 1024; admitted budgets may not fit; raise host_reserve_mib in site.conf and rerun provision",
	}
	for _, line := range expected {
		if !strings.Contains(text, line) {
			t.Fatalf("output lacks %q:\n%s", line, text)
		}
	}
	if strings.Contains(text, "drift: CPU") {
		t.Fatalf("matching CPU sides must not print a drift line:\n%s", text)
	}
}

// TestHumanSandboxDetailWithoutResourcesKeepsLegacyLayout pins the old
// executor shape: no resources object means no Resources section and no
// rendering error.
func TestHumanSandboxDetailWithoutResourcesKeepsLegacyLayout(t *testing.T) {
	result := map[string]interface{}{
		"instance": "sandbox-research", "logical_name": "research", "instance_status": "Stopped", "instance_pid": float64(0),
		"description": "LLM evaluation", "workspace": "/workspace", "shared": "/shared",
	}
	var output bytes.Buffer
	handled, err := renderHumanResult("inspect-sandbox", result, &output, humanOutputStyle{Width: 100})
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if strings.Contains(output.String(), "RESOURCES") {
		t.Fatalf("a result without resources must not print the section:\n%s", output.String())
	}
}

// TestHumanSandboxStatusRendersHostOnlyResources pins the default-sandbox
// status path: the host and pool rows render even without the per-sandbox
// resource sets.
func TestHumanSandboxStatusRendersHostOnlyResources(t *testing.T) {
	result := map[string]interface{}{
		"instance": "sandbox-demo", "instance_status": "Running", "instance_pid": int64(55684),
		"services": []interface{}{
			map[string]interface{}{"name": "incus.service", "active_state": "active", "result": "success", "n_restarts": float64(0)},
		},
		"resources": map[string]interface{}{
			"host": map[string]interface{}{
				"mem_total_mib": int64(63920), "mem_available_mib": int64(20000), "host_reserve_mib": int64(1024),
				"admitted_mib": int64(49152), "resident_sum_mib": int64(24884), "observed_own_mib": int64(19036),
				"warnings": []interface{}{},
			},
			"pool": map[string]interface{}{"name": "chasm-pool", "size": map[string]interface{}{"mib": 921600, "source": "storage pool config.size"}, "used": map[string]interface{}{"mib": 279552, "source": "storage pool resources space.used"}},
		},
	}
	var output bytes.Buffer
	handled, err := renderHumanResult("sandbox-status", result, &output, humanOutputStyle{Width: 200})
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	text := output.String()
	for _, expected := range []string{"Host         MemTotal 62.4 GiB", "Pool         chasm-pool · 900 GiB"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("output lacks %q:\n%s", expected, text)
		}
	}
}

// TestHumanResourcesRenderUnknownWithReason pins the unknown rendering: an
// unobservable value is shown with its reason, never as a zero or omitted.
func TestHumanResourcesRenderUnknownWithReason(t *testing.T) {
	fixture := goldenSandboxResult(t)
	resources := fixture["resources"].(map[string]interface{})
	memory := resources["memory"].(map[string]interface{})
	memory["guest_total"] = map[string]interface{}{"unknown": true, "reason": "the guest agent did not report it"}
	memory["host_resident"] = map[string]interface{}{"unknown": true, "reason": "the sandbox is not running"}
	var output bytes.Buffer
	handled, err := renderHumanResult("inspect-sandbox", fixture, &output, humanOutputStyle{Width: 200})
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	text := output.String()
	for _, expected := range []string{"guest unknown (the guest agent did not report it) total", "host unknown (the sandbox is not running) resident"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("output lacks %q:\n%s", expected, text)
		}
	}
}

// TestHumanSandboxDetailRendersUnavailableResources pins the degradation
// rendering: a resources object carrying "unavailable" prints the reason in
// the Resources section instead of dropping the section. Mutation checked:
// removing the unavailable branch from renderSandboxResources fails this
// test because the reason never prints.
func TestHumanSandboxDetailRendersUnavailableResources(t *testing.T) {
	result := map[string]interface{}{
		"instance": "sandbox-research", "logical_name": "research", "instance_status": "Stopped", "instance_pid": float64(0),
		"description": "LLM evaluation", "workspace": "/workspace", "shared": "/shared",
		"resources": map[string]interface{}{"unavailable": "unsafe_state: the storage pool is unreachable"},
	}
	var output bytes.Buffer
	handled, err := renderHumanResult("inspect-sandbox", result, &output, humanOutputStyle{Width: 200})
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	text := output.String()
	if !strings.Contains(text, "RESOURCES") || !strings.Contains(text, "unavailable (unsafe_state: the storage pool is unreachable)") {
		t.Fatalf("a failed collector must still print the section with its reason:\n%s", text)
	}
}
