package execserver

import (
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rahulrajaram/chasm/internal/protocol"
	"github.com/rahulrajaram/chasm/internal/siteconfig"
)

func managedSummary(name, status string) incusInstanceSummary {
	return incusInstanceSummary{
		Name:   name,
		Status: status,
		Config: map[string]string{
			"user.chasm.managed":          "true",
			"user.chasm.logical-name":     logicalSandboxName(name),
			"user.chasm.workspace-volume": "chasm-workspace-" + name,
			"user.chasm.shared-volume":    "chasm-shared",
		},
	}
}

func TestDeleteSandboxWithHappyPathAndPostconditions(t *testing.T) {
	instance := "sandbox-perf-cycle-1"
	deleted := false
	volumeGone := false
	catalogGone := false
	environment := deleteSandboxEnvironment{
		inventory: func() ([]incusInstanceSummary, error) {
			if deleted {
				return []incusInstanceSummary{managedSummary("sandbox-peer", "Stopped")}, nil
			}
			return []incusInstanceSummary{managedSummary("sandbox-peer", "Stopped"), managedSummary(instance, "Stopped")}, nil
		},
		deleteInstance: func() error { deleted = true; return nil },
		volumePresent: func() (bool, error) {
			return !volumeGone, nil
		},
		volumeMetadata: func() (bool, error) { return true, nil },
		deleteVolume:   func() error { volumeGone = true; return nil },
		catalogPresent: func() (bool, error) { return !catalogGone, nil },
		removeCatalog:  func() error { catalogGone = true; return nil },
	}
	result, err := deleteSandboxWith(instance, environment)
	if err != nil {
		t.Fatalf("delete failed: %v", err)
	}
	if result["instance_deleted"] != true || result["already_deleted"] != false ||
		result["workspace_volume_deleted"] != true || result["catalog_snapshot_removed"] != true {
		t.Fatalf("result fields wrong: %v", result)
	}
	steps, ok := result["steps"].([]deleteStep)
	if !ok || len(steps) != 3 || steps[0].Outcome != "deleted" || steps[1].Outcome != "deleted" || steps[2].Outcome != "removed" {
		t.Fatalf("steps wrong: %v", result["steps"])
	}
	if result["one_running_guard"] != "untouched" {
		t.Fatalf("running-instance admission evidence missing: %v", result)
	}
}

func TestDeleteSandboxWithIdempotentResidueFreeRedelete(t *testing.T) {
	mutations := 0
	environment := deleteSandboxEnvironment{
		inventory: func() ([]incusInstanceSummary, error) {
			return []incusInstanceSummary{managedSummary("sandbox-peer", "Stopped")}, nil
		},
		deleteInstance: func() error { mutations++; return nil },
		volumePresent:  func() (bool, error) { return false, nil },
		deleteVolume:   func() error { mutations++; return nil },
		catalogPresent: func() (bool, error) { return false, nil },
		removeCatalog:  func() error { mutations++; return nil },
	}
	result, err := deleteSandboxWith("sandbox-perf-cycle-1", environment)
	if err != nil {
		t.Fatalf("idempotent delete failed: %v", err)
	}
	if result["already_deleted"] != true || mutations != 0 {
		t.Fatalf("idempotent path mutated state: result=%v mutations=%d", result, mutations)
	}
}

func TestDeleteSandboxWithAbsentInstanceRequiresVolumeOwnershipProof(t *testing.T) {
	tests := []struct {
		name       string
		proof      func() (bool, error)
		wantDelete bool
	}{
		{"verified-workspace-volume", func() (bool, error) { return true, nil }, true},
		{"metadata-mismatch", func() (bool, error) { return false, nil }, false},
		{"metadata-unavailable", func() (bool, error) { return false, errors.New("incus unavailable") }, false},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			deletedVolume := false
			environment := deleteSandboxEnvironment{
				inventory:      func() ([]incusInstanceSummary, error) { return nil, nil },
				volumePresent:  func() (bool, error) { return !deletedVolume, nil },
				volumeMetadata: testCase.proof,
				deleteVolume:   func() error { deletedVolume = true; return nil },
				catalogPresent: func() (bool, error) { return false, nil },
				removeCatalog:  func() error { return nil },
			}
			_, err := deleteSandboxWith("sandbox-perf-cycle-1", environment)
			if testCase.wantDelete {
				if err != nil || !deletedVolume {
					t.Fatalf("verified volume was not deleted: deleted=%v err=%v", deletedVolume, err)
				}
				return
			}
			var protocolError *ProtocolError
			if !errors.As(err, &protocolError) || protocolError.Code != "unsafe_state" || deletedVolume {
				t.Fatalf("unverified volume was not safely refused: deleted=%v err=%v", deletedVolume, err)
			}
		})
	}
}

func TestDeleteSandboxAbsentInstanceRefusalPreservesResidue(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		volume      func() (bool, error)
		proof       func() (bool, error)
		wantSuccess bool
	}{
		{"volume-inventory-unavailable", func() (bool, error) { return false, errors.New("unavailable") }, nil, false},
		{"metadata-unavailable", func() (bool, error) { return true, nil }, func() (bool, error) { return false, errors.New("unavailable") }, false},
		{"metadata-mismatch", func() (bool, error) { return true, nil }, func() (bool, error) { return false, nil }, false},
		{"verified-volume", nil, func() (bool, error) { return true, nil }, true},
		{"half-built-managed-only", nil, nil, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			instancePresent, volumePresent, catalogPresent := testCase.name == "half-built-managed-only", testCase.name != "half-built-managed-only", true
			mutations := 0
			volumeChecks := 0
			environment := deleteSandboxEnvironment{
				inventory: func() ([]incusInstanceSummary, error) {
					if instancePresent {
						return []incusInstanceSummary{managedSummary("sandbox-x", "Stopped")}, nil
					}
					return nil, nil
				},
				deleteInstance: func() error { mutations++; instancePresent = false; return nil },
				volumePresent: func() (bool, error) {
					volumeChecks++
					if testCase.volume != nil {
						return testCase.volume()
					}
					return volumePresent, nil
				},
				volumeMetadata: testCase.proof,
				deleteVolume:   func() error { mutations++; volumePresent = false; return nil },
				catalogPresent: func() (bool, error) { return catalogPresent, nil },
				removeCatalog:  func() error { mutations++; catalogPresent = false; return nil },
			}
			_, err := deleteSandboxWith("sandbox-x", environment)
			if testCase.wantSuccess {
				wantMutations := 2
				if err != nil || instancePresent || catalogPresent || volumePresent || mutations != wantMutations {
					t.Fatalf("teardown failed: err=%v instance=%v volume=%v catalog=%v mutations=%d", err, instancePresent, volumePresent, catalogPresent, mutations)
				}
				return
			}
			var protocolError *ProtocolError
			if !errors.As(err, &protocolError) || protocolError.Code != "unsafe_state" || mutations != 0 || instancePresent || !volumePresent || !catalogPresent || volumeChecks != 1 {
				t.Fatalf("refusal altered residue: err=%v instance=%v volume=%v catalog=%v mutations=%d volumeChecks=%d", err, instancePresent, volumePresent, catalogPresent, mutations, volumeChecks)
			}
		})
	}
}

func TestDeleteSandboxPreflightsVolumeBeforeManagedInstanceDeletion(t *testing.T) {
	for _, tc := range []struct {
		name           string
		inventoryError error
		proof          func() (bool, error)
	}{
		{"volume-inventory-error", errors.New("inventory unavailable"), nil},
		{"missing-proof", nil, nil},
		{"metadata-error", nil, func() (bool, error) { return false, errors.New("metadata unavailable") }},
		{"foreign-volume", nil, func() (bool, error) { return false, nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unexpectedMutation := func() error { t.Fatal("unexpected mutation"); return nil }
			env := deleteSandboxEnvironment{
				inventory: func() ([]incusInstanceSummary, error) {
					return []incusInstanceSummary{managedSummary("sandbox-x", "Stopped")}, nil
				},
				deleteInstance: unexpectedMutation, deleteVolume: unexpectedMutation, removeCatalog: unexpectedMutation,
				volumePresent:  func() (bool, error) { return true, tc.inventoryError },
				volumeMetadata: tc.proof,
			}
			_, err := deleteSandboxWith("sandbox-x", env)
			var pe *ProtocolError
			if !errors.As(err, &pe) || pe.Code != "unsafe_state" {
				t.Fatalf("expected unsafe_state refusal, got %v", err)
			}
		})
	}
}

func TestDeleteSandboxRefusesIncusStateChangeAtDeletion(t *testing.T) {
	original := runIncus
	t.Cleanup(func() { runIncus = original })
	calls := 0
	runIncus = func(args []string, _ io.Reader, _ io.Writer, _ []*os.File, _ time.Duration) (int, error) {
		calls++
		if !slices.Equal(args, []string{"delete", "sandbox-x"}) {
			t.Fatalf("expected stopped-only delete, got %q", args)
		}
		return 1, errors.New("instance is running")
	}
	env := productionDeleteEnvironment("sandbox-x")
	env.inventory = func() ([]incusInstanceSummary, error) {
		return []incusInstanceSummary{managedSummary("sandbox-x", "Stopped")}, nil
	}
	env.volumePresent = func() (bool, error) { return true, nil }
	env.volumeMetadata = func() (bool, error) { return true, nil }
	env.deleteVolume = func() error { t.Fatal("removed volume after Incus refusal"); return nil }
	env.removeCatalog = func() error { t.Fatal("removed catalog after Incus refusal"); return nil }
	_, err := deleteSandboxWith("sandbox-x", env)
	var pe *ProtocolError
	if calls != 1 || !errors.As(err, &pe) || pe.Code != "operation_failed" {
		t.Fatalf("expected single failed delete, got calls=%d err=%v", calls, err)
	}
}

func TestWorkspaceVolumeMetadataValid(t *testing.T) {
	tests := []struct {
		name       string
		metadata   map[string]interface{}
		volumeType string
		content    string
		queryErr   error
		wantValid  bool
		wantErr    bool
	}{
		{"all-markers-match", map[string]interface{}{"protected": "true", "scope": "workspace", "owner": "sandbox-x"}, "custom", "filesystem", nil, true, false},
		{"wrong-owner", map[string]interface{}{"protected": "true", "scope": "workspace", "owner": "sandbox-other"}, "custom", "filesystem", nil, false, false},
		{"wrong-scope", map[string]interface{}{"protected": "true", "scope": "global", "owner": "sandbox-x"}, "custom", "filesystem", nil, false, false},
		{"non-string-protection", map[string]interface{}{"protected": true, "scope": "workspace", "owner": "sandbox-x"}, "custom", "filesystem", nil, false, false},
		{"wrong-filesystem-type", nil, "custom", "block", nil, false, false},
		{"proof-unavailable", nil, "custom", "filesystem", errors.New("incus unavailable"), false, true},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			calls := 0
			query := func(path string) (map[string]interface{}, error) {
				calls++
				if path != "/1.0/storage-pools/pool/volumes/custom/chasm-workspace-sandbox-x" {
					t.Fatalf("unexpected Incus API path: %q", path)
				}
				if testCase.queryErr != nil {
					return nil, testCase.queryErr
				}
				config := make(map[string]interface{})
				for field, value := range testCase.metadata {
					config[siteconfig.Active().MetadataKey(field)] = value
				}
				return map[string]interface{}{"type": testCase.volumeType, "content_type": testCase.content, "config": config}, nil
			}
			valid, err := workspaceVolumeMetadataValidWith(query, "pool", "chasm-workspace-sandbox-x", "sandbox-x")
			if valid != testCase.wantValid || (err != nil) != testCase.wantErr {
				t.Fatalf("valid=%v err=%v, want valid=%v error=%v", valid, err, testCase.wantValid, testCase.wantErr)
			}
			if calls != 1 {
				t.Fatalf("Incus calls=%d, want one API read", calls)
			}
		})
	}
}

func TestDeleteSandboxWithRefusalPaths(t *testing.T) {
	tests := []struct {
		name     string
		instance string
		status   string
		managed  bool
		wantCode string
	}{
		{"unmanaged-name", "Template-Build", "Stopped", false, "invalid_instance"},
		{"running-refused", "sandbox-research", "Running", true, "invalid_state"},
		{"unmanaged", "sandbox-research", "Stopped", false, "invalid_state"},
		{"ambiguous-state", "sandbox-research", "Starting", true, "invalid_state"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			summary := managedSummary("sandbox-research", testCase.status)
			if !testCase.managed {
				summary.Config = map[string]string{}
			}
			inventory := []incusInstanceSummary{managedSummary("sandbox-peer", "Stopped")}
			if protocol.ValidSandboxInstance(testCase.instance) {
				inventory = append(inventory, summary)
			}
			environment := deleteSandboxEnvironment{
				inventory:      func() ([]incusInstanceSummary, error) { return inventory, nil },
				deleteInstance: func() error { t.Fatal("deleteInstance must not run on a refusal path"); return nil },
				volumePresent:  func() (bool, error) { t.Fatal("volume inventory must not run on a refusal path"); return false, nil },
				deleteVolume:   func() error { t.Fatal("deleteVolume must not run on a refusal path"); return nil },
				catalogPresent: func() (bool, error) { return false, nil },
				removeCatalog:  func() error { t.Fatal("removeCatalog must not run on a refusal path"); return nil },
			}
			_, err := deleteSandboxWith(testCase.instance, environment)
			if err == nil {
				t.Fatalf("expected refusal for %s", testCase.name)
			}
			var protocolError *ProtocolError
			if !errors.As(err, &protocolError) || protocolError.Code != testCase.wantCode {
				t.Fatalf("error = %v, want code %s", err, testCase.wantCode)
			}
		})
	}
}

func TestDeleteSandboxWithPostconditionFailuresFailClosed(t *testing.T) {
	tests := []struct {
		name       string
		after      func() ([]incusInstanceSummary, error)
		volume     func() (bool, error)
		catalog    func() (bool, error)
		wantSecret string
	}{
		{
			name: "instance-still-present",
			after: func() ([]incusInstanceSummary, error) {
				return []incusInstanceSummary{managedSummary("sandbox-research", "Stopped")}, nil
			},
			wantSecret: "instance is still present",
		},
		{
			name: "volume-still-present",
			after: func() ([]incusInstanceSummary, error) {
				return []incusInstanceSummary{managedSummary("sandbox-peer", "Stopped")}, nil
			},
			volume:     func() (bool, error) { return true, nil },
			wantSecret: "volume is still present",
		},
		{
			name: "catalog-still-present",
			after: func() ([]incusInstanceSummary, error) {
				return []incusInstanceSummary{managedSummary("sandbox-peer", "Stopped")}, nil
			},
			volume:     func() (bool, error) { return false, nil },
			catalog:    func() (bool, error) { return true, nil },
			wantSecret: "catalog snapshot is still present",
		},
		{
			name:       "inventory-unavailable-after",
			after:      func() ([]incusInstanceSummary, error) { return nil, errors.New("unreachable") },
			wantSecret: "inventory is unavailable after deletion",
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			calls := 0
			environment := deleteSandboxEnvironment{
				inventory: func() ([]incusInstanceSummary, error) {
					calls++
					if calls == 1 {
						return []incusInstanceSummary{managedSummary("sandbox-research", "Stopped")}, nil
					}
					return testCase.after()
				},
				deleteInstance: func() error { return nil },
				volumePresent: func() (bool, error) {
					if testCase.volume != nil {
						return testCase.volume()
					}
					return false, nil
				},
				volumeMetadata: func() (bool, error) { return true, nil },
				deleteVolume:   func() error { return nil },
				catalogPresent: func() (bool, error) {
					if testCase.catalog != nil {
						return testCase.catalog()
					}
					return false, nil
				},
				removeCatalog: func() error { return nil },
			}
			_, err := deleteSandboxWith("sandbox-research", environment)
			var protocolError *ProtocolError
			if !errors.As(err, &protocolError) || protocolError.Code != "postcondition_failed" ||
				!strings.Contains(protocolError.Message, testCase.wantSecret) {
				t.Fatalf("error = %v, want postcondition_failed with %q", err, testCase.wantSecret)
			}
		})
	}
}

func TestMemoryLimitValueAndRangeGuard(t *testing.T) {
	original := runIncus
	t.Cleanup(func() { runIncus = original })
	calls := 0
	runIncus = func(arguments []string, _ io.Reader, stdout io.Writer, _ []*os.File, _ time.Duration) (int, error) {
		calls++
		want := []string{"config", "set", "sandbox-x", "limits.memory", "3072MiB"}
		if !slices.Equal(arguments, want) {
			t.Fatalf("runIncus arguments = %q, want %q", arguments, want)
		}
		return 1, errors.New("stubbed incus refusal")
	}
	if got := memoryLimitValue(3072); got != "3072MiB" {
		t.Fatalf("memoryLimitValue(3072) = %q", got)
	}
	if got := memoryLimitValue(512); got != "512MiB" {
		t.Fatalf("memoryLimitValue(512) = %q", got)
	}
	// Out-of-range budgets must be refused before runIncus is called.
	if err := setInstanceMemoryLimit("sandbox-x", 100); err == nil {
		t.Fatal("below-floor budget was admitted")
	}
	if err := setInstanceMemoryLimit("sandbox-x", protocol.SafetyBoundSandboxBudgetMemoryMiB+1); err == nil {
		t.Fatal("above-ceiling budget was admitted")
	}
	if calls != 0 {
		t.Fatalf("out-of-range budgets called runIncus %d times", calls)
	}
	if err := setInstanceMemoryLimit("sandbox-x", 3072); err == nil {
		t.Fatal("in-range budget did not reach the refusing stub")
	}
	if calls != 1 {
		t.Fatalf("in-range budget called runIncus %d times, want 1", calls)
	}
}
