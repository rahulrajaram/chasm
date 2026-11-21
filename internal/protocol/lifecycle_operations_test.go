package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestStopGuestProtocolIsExactAndInstanceBound(t *testing.T) {
	public := []byte(`{"version":1,"request_id":"stop-1","operation":"stop_guest"}`)
	request, err := ParsePublic(public)
	if err != nil || request.Operation != "stop_guest" {
		t.Fatalf("public stop request=%+v err=%v", request, err)
	}
	executor := []byte(`{"version":1,"request_id":"stop-2","operation":"stop_guest","instance":"sandbox-demo"}`)
	request, err = ParseExecutor(executor)
	if err != nil || request.Operation != "stop_guest" || request.Instance() != "sandbox-demo" {
		t.Fatalf("executor stop request=%+v err=%v", request, err)
	}
	for _, candidate := range [][]byte{
		[]byte(`{"version":1,"request_id":"stop-4","operation":"stop_guest"}`),
		[]byte(`{"version":1,"request_id":"stop-5","operation":"stop_guest","extra":true}`),
	} {
		if _, err := ParseExecutor(candidate); err == nil {
			t.Fatalf("executor candidate admitted: %s", candidate)
		}
	}
}

func TestNamedLifecycleOperationsAcceptOnlyCanonicalInstanceNames(t *testing.T) {
	for _, operation := range []string{"create_sandbox", "describe_sandbox", "inspect_sandbox", "ensure_guest_running", "stop_guest"} {
		for _, instance := range []string{"sandbox-research", "sandbox-project-42", "sandbox-a"} {
			raw := []byte(`{"version":1,"request_id":"named-1","operation":"` + operation + `","instance":"` + instance + `"}`)
			request, err := ParseExecutor(raw)
			if err != nil || request.Instance() != instance {
				t.Fatalf("operation=%s instance=%s request=%+v err=%v", operation, instance, request, err)
			}
		}
	}

	invalid := []string{
		"", "research", "project-42", "a", "sandbox-UPPER", "sandbox--leading", "sandbox-trailing-", "sandbox-under_score", "sandbox-with.dot",
		"two words", "../escape", "--project", "a/b", "a%2fb", "a?query",
		"sandbox-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	for _, operation := range []string{"create_sandbox", "describe_sandbox", "inspect_sandbox", "ensure_guest_running", "stop_guest"} {
		for _, instance := range invalid {
			raw, err := json.Marshal(map[string]interface{}{
				"version": 1, "request_id": "named-2", "operation": operation, "instance": instance,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ParseExecutor(raw); err == nil {
				t.Fatalf("operation=%s admitted invalid instance %q", operation, instance)
			} else if code, _ := ErrorCode(err); code != "invalid_instance" {
				t.Fatalf("operation=%s instance=%q code=%q err=%v", operation, instance, code, err)
			}
		}
	}
}

func TestSandboxDescriptionValidationIsBoundedAndSingleLine(t *testing.T) {
	valid := []string{"", "LLM evaluation and tooling", "Résumé experiments 🧪", strings.Repeat("a", MaxSandboxDescription)}
	invalid := []string{
		" leading", "trailing ", "line one\nline two", "tab\tseparated", "line\u2028separator",
		strings.Repeat("a", MaxSandboxDescription+1), string([]byte{0xff}),
	}
	for _, description := range valid {
		if !ValidSandboxDescription(description) {
			t.Errorf("valid description refused: %q", description)
		}
	}
	for _, description := range invalid {
		if ValidSandboxDescription(description) {
			t.Errorf("invalid description admitted: %q", description)
		}
	}
}

func TestSandboxDescriptionProtocolShapesAreExact(t *testing.T) {
	accepted := [][]byte{
		[]byte(`{"version":1,"request_id":"description-1","operation":"describe_sandbox","instance":"sandbox-research"}`),
		[]byte(`{"version":1,"request_id":"description-2","operation":"set_sandbox_description","instance":"sandbox-research","description":"LLM evaluation"}`),
		[]byte(`{"version":1,"request_id":"description-3","operation":"set_sandbox_description","instance":"sandbox-demo","description":""}`),
	}
	for _, raw := range accepted {
		if _, err := ParsePublic(raw); err != nil {
			t.Fatalf("public description request rejected: %s: %v", raw, err)
		}
		if _, err := ParseExecutor(raw); err != nil {
			t.Fatalf("executor description request rejected: %s: %v", raw, err)
		}
	}

	rejected := [][]byte{
		[]byte(`{"version":1,"request_id":"description-4","operation":"set_sandbox_description","instance":"sandbox-research"}`),
		[]byte(`{"version":1,"request_id":"description-5","operation":"set_sandbox_description","instance":"sandbox-research","description":" leading"}`),
		[]byte(`{"version":1,"request_id":"description-6","operation":"describe_sandbox","instance":"sandbox-research","description":"extra"}`),
	}
	for _, raw := range rejected {
		if _, err := ParsePublic(raw); err == nil {
			t.Fatalf("invalid public description request admitted: %s", raw)
		}
		if _, err := ParseExecutor(raw); err == nil {
			t.Fatalf("invalid executor description request admitted: %s", raw)
		}
	}
}

func TestPiWorkflowOperationsAcceptOnlyManagedInstances(t *testing.T) {
	accepted := []byte(`{"version":1,"request_id":"pause-1","operation":"pause_guest","instance":"sandbox-demo"}`)
	if _, err := ParseExecutor(accepted); err != nil {
		t.Fatalf("managed instance refused for a lifecycle operation: %v", err)
	}
	raw := []byte(`{"version":1,"request_id":"pause-1","operation":"pause_guest","instance":"research"}`)
	if _, err := ParseExecutor(raw); err == nil {
		t.Fatal("unmanaged instance admitted for a lifecycle operation")
	} else if code, _ := ErrorCode(err); code != "invalid_instance" {
		t.Fatalf("code=%q err=%v", code, err)
	}
}

func TestPublicNamedSandboxDiscoveryShapesAreExact(t *testing.T) {
	accepted := [][]byte{
		[]byte(`{"version":1,"request_id":"public-1","operation":"create_sandbox","instance":"sandbox-research"}`),
		[]byte(`{"version":1,"request_id":"public-2","operation":"list_sandboxes"}`),
		[]byte(`{"version":1,"request_id":"public-3","operation":"inspect_sandbox","instance":"sandbox-research"}`),
		[]byte(`{"version":1,"request_id":"public-4","operation":"start_sandbox","instance":"sandbox-research"}`),
		[]byte(`{"version":1,"request_id":"public-5","operation":"stop_sandbox","instance":"sandbox-research"}`),
		[]byte(`{"version":1,"request_id":"public-5a","operation":"pause_sandbox","instance":"sandbox-research"}`),
		[]byte(`{"version":1,"request_id":"public-5b","operation":"resume_sandbox","instance":"sandbox-research"}`),
		[]byte(`{"version":1,"request_id":"public-6","operation":"sandbox_layout","instance":"sandbox-research","depth":3}`),
		[]byte(`{"version":1,"request_id":"public-7","operation":"list_repositories"}`),
		[]byte(`{"version":1,"request_id":"public-8","operation":"locate_repository","repository":"demo.git"}`),
		[]byte(`{"version":1,"request_id":"public-15","operation":"describe_sandbox","instance":"sandbox-research"}`),
		[]byte(`{"version":1,"request_id":"public-16","operation":"set_sandbox_description","instance":"sandbox-research","description":"Research work"}`),
	}
	for _, raw := range accepted {
		if _, err := ParsePublic(raw); err != nil {
			t.Fatalf("public request rejected: %s: %v", raw, err)
		}
	}

	rejected := [][]byte{
		[]byte(`{"version":1,"request_id":"public-9","operation":"start_sandbox","instance":"research"}`),
		[]byte(`{"version":1,"request_id":"public-10","operation":"sandbox_layout","instance":"sandbox-research","depth":0}`),
		[]byte(`{"version":1,"request_id":"public-11","operation":"sandbox_layout","instance":"sandbox-research","depth":6}`),
		[]byte(`{"version":1,"request_id":"public-12","operation":"sandbox_layout","instance":"sandbox-research","depth":1.5}`),
		[]byte(`{"version":1,"request_id":"public-13","operation":"list_repositories","instance":"sandbox-research"}`),
		[]byte(`{"version":1,"request_id":"public-14","operation":"locate_repository","repository":"../demo"}`),
	}
	for _, raw := range rejected {
		if _, err := ParsePublic(raw); err == nil {
			t.Fatalf("invalid public request admitted: %s", raw)
		}
	}
}

func TestExecutorDiscoveryShapesAreExact(t *testing.T) {
	accepted := [][]byte{
		[]byte(`{"version":1,"request_id":"exec-1","operation":"list_sandboxes"}`),
		[]byte(`{"version":1,"request_id":"exec-2","operation":"sandbox_layout","instance":"sandbox-research","depth":5}`),
		[]byte(`{"version":1,"request_id":"exec-3","operation":"list_repositories"}`),
		[]byte(`{"version":1,"request_id":"exec-4","operation":"locate_repository","repository":"demo"}`),
		[]byte(`{"version":1,"request_id":"exec-5","operation":"describe_sandbox","instance":"sandbox-research"}`),
		[]byte(`{"version":1,"request_id":"exec-6","operation":"set_sandbox_description","instance":"sandbox-research","description":"Research work"}`),
	}
	for _, raw := range accepted {
		if _, err := ParseExecutor(raw); err != nil {
			t.Fatalf("executor request rejected: %s: %v", raw, err)
		}
	}
}

func TestBudgetRangeMessageRendersTheProtocolConstants(t *testing.T) {
	message := BudgetRangeMessage()
	for _, expected := range []string{
		"budget_memory_mib",
		fmt.Sprintf("%d", MinSandboxBudgetMemoryMiB),
		fmt.Sprintf("%d", SafetyBoundSandboxBudgetMemoryMiB),
	} {
		if !strings.Contains(message, expected) {
			t.Fatalf("budget range message lacks %q: %s", expected, message)
		}
	}
}

func TestCreateSandboxBudgetFieldIsValidated(t *testing.T) {
	accepted := [][]byte{
		[]byte(`{"version":1,"request_id":"budget-1","operation":"create_sandbox","instance":"sandbox-research"}`),
		[]byte(`{"version":1,"request_id":"budget-2","operation":"create_sandbox","instance":"sandbox-research","budget_memory_mib":512}`),
		[]byte(`{"version":1,"request_id":"budget-3","operation":"create_sandbox","instance":"sandbox-research","budget_memory_mib":1048576}`),
		[]byte(`{"version":1,"request_id":"budget-3b","operation":"create_sandbox","instance":"sandbox-research","budget_memory_mib":70000}`),
		// 70000 MiB is above the historical fixed cap and must still parse:
		// the host-derived cap is enforced by the executor, not here.
		[]byte(`{"version":1,"request_id":"budget-4","operation":"create_sandbox","instance":"sandbox-research","budget_memory_mib":2048}`),
		[]byte(`{"version":1,"request_id":"budget-11","operation":"create_sandbox","instance":"sandbox-research","budget_cpu":4}`),
	}
	for _, raw := range accepted {
		executorRequest, err := ParseExecutor(raw)
		if err != nil {
			t.Fatalf("executor request rejected: %s: %v", raw, err)
		}
		if _, present := executorRequest.Fields["budget_memory_mib"]; present {
			if budget, ok := executorRequest.NumberField("budget_memory_mib"); !ok || budget <= 0 {
				t.Fatalf("executor budget not carried: %s", raw)
			}
		}
		if _, err := ParsePublic(raw); err != nil {
			t.Fatalf("public request rejected: %s: %v", raw, err)
		}
	}

	rejected := [][]byte{
		[]byte(`{"version":1,"request_id":"budget-5","operation":"create_sandbox","instance":"sandbox-research","budget_memory_mib":511}`),
		[]byte(`{"version":1,"request_id":"budget-6","operation":"create_sandbox","instance":"sandbox-research","budget_memory_mib":1048577}`),
		[]byte(`{"version":1,"request_id":"budget-7","operation":"create_sandbox","instance":"sandbox-research","budget_memory_mib":2048.5}`),
		[]byte(`{"version":1,"request_id":"budget-8","operation":"create_sandbox","instance":"sandbox-research","budget_memory_mib":"2048"}`),
		[]byte(`{"version":1,"request_id":"budget-9","operation":"create_sandbox","instance":"sandbox-research","budget_memory_mib":-2048}`),
	}
	for _, raw := range rejected {
		if _, err := ParseExecutor(raw); err == nil {
			t.Fatalf("executor admitted invalid budget: %s", raw)
		} else if code, _ := ErrorCode(err); code != "invalid_budget" {
			t.Fatalf("executor code=%q for %s", code, raw)
		} else if !strings.Contains(err.Error(), BudgetRangeMessage()) {
			t.Fatalf("executor budget error does not state the allowed range: %v", err)
		}
		if _, err := ParsePublic(raw); err == nil {
			t.Fatalf("public admitted invalid budget: %s", raw)
		} else if code, _ := ErrorCode(err); code != "invalid_budget" {
			t.Fatalf("public code=%q for %s", code, raw)
		} else if !strings.Contains(err.Error(), BudgetRangeMessage()) {
			t.Fatalf("public budget error does not state the allowed range: %v", err)
		}
	}

	extra := []byte(`{"version":1,"request_id":"budget-10","operation":"create_sandbox","instance":"sandbox-research","budget_memory_mib":2048,"unexpected":1}`)
	if _, err := ParseExecutor(extra); err == nil {
		t.Fatal("executor admitted an unexpected extra field")
	}
	if _, err := ParsePublic(extra); err == nil {
		t.Fatal("public admitted an unexpected extra field")
	}
}

func TestCreateSandboxCpuBudgetFieldIsValidated(t *testing.T) {
	accepted := [][]byte{
		[]byte(`{"version":1,"request_id":"cpu-1","operation":"create_sandbox","instance":"sandbox-research"}`),
		[]byte(`{"version":1,"request_id":"cpu-2","operation":"create_sandbox","instance":"sandbox-research","budget_cpu":1}`),
		[]byte(`{"version":1,"request_id":"cpu-3","operation":"create_sandbox","instance":"sandbox-research","budget_memory_mib":2048,"budget_cpu":2}`),
	}
	for _, raw := range accepted {
		if _, err := ParseExecutor(raw); err != nil {
			t.Fatalf("executor request rejected: %s: %v", raw, err)
		}
		if _, err := ParsePublic(raw); err != nil {
			t.Fatalf("public request rejected: %s: %v", raw, err)
		}
	}
	rejected := [][]byte{
		[]byte(`{"version":1,"request_id":"cpu-4","operation":"create_sandbox","instance":"sandbox-research","budget_cpu":0}`),
		[]byte(`{"version":1,"request_id":"cpu-5","operation":"create_sandbox","instance":"sandbox-research","budget_cpu":1025}`),
		[]byte(`{"version":1,"request_id":"cpu-6","operation":"create_sandbox","instance":"sandbox-research","budget_cpu":"2"}`),
	}
	for _, raw := range rejected {
		if _, err := ParseExecutor(raw); err == nil {
			t.Fatalf("executor admitted invalid CPU budget: %s", raw)
		} else if code, _ := ErrorCode(err); code != "invalid_cpu_budget" {
			t.Fatalf("executor code=%q for %s", code, raw)
		}
		if _, err := ParsePublic(raw); err == nil {
			t.Fatalf("public admitted invalid CPU budget: %s", raw)
		}
	}
}

func TestAdmitLaunchOperationShape(t *testing.T) {
	accepted := []byte(`{"version":1,"request_id":"admit-1","operation":"admit_launch","instance":"sandbox-research"}`)
	executorRequest, err := ParseExecutor(accepted)
	if err != nil || executorRequest.Instance() != "sandbox-research" {
		t.Fatalf("executor err=%v request=%+v", err, executorRequest)
	}
	if _, err := ParsePublic(accepted); err != nil {
		t.Fatalf("public err=%v", err)
	}
	rejected := [][]byte{
		[]byte(`{"version":1,"request_id":"admit-2","operation":"admit_launch","instance":"research"}`),
		[]byte(`{"version":1,"request_id":"admit-3","operation":"admit_launch"}`),
		[]byte(`{"version":1,"request_id":"admit-4","operation":"admit_launch","instance":"sandbox-research","extra":1}`),
	}
	for _, raw := range rejected {
		if _, err := ParseExecutor(raw); err == nil {
			t.Fatalf("executor admitted %s", raw)
		}
		if _, err := ParsePublic(raw); err == nil {
			t.Fatalf("public admitted %s", raw)
		}
	}
}
