package execserver

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rahulrajaram/chasm/internal/protocol"
)

// stopGuestWith drives the default sandbox's stop path with injected state
// and stop seams, mirroring the production stopGuest wiring.
func stopGuestWith(readState instanceStateReader, stop instanceStopper) (map[string]interface{}, error) {
	instance, err := guestInstance()
	if err != nil {
		return nil, err
	}
	return stopSandboxWith(instance, readState, stop)
}

func TestStopGuestTransitionsRunningInstanceAndProvesPostcondition(t *testing.T) {
	mustBindGuestSite(t)
	states := []map[string]interface{}{
		{"status": "Running", "pid": float64(4242)},
		{"status": "Stopped", "pid": float64(0)},
	}
	reads := 0
	stopCalls := 0
	result, err := stopGuestWith(func() (map[string]interface{}, error) {
		state := states[reads]
		reads++
		return state, nil
	}, func() (int, error) {
		stopCalls++
		return 0, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if reads != 2 || stopCalls != 1 || result["instance"] != "sandbox-demo" || result["instance_status"] != "Stopped" || result["instance_pid"] != int64(0) || result["stopped"] != true {
		t.Fatalf("reads=%d stops=%d result=%v", reads, stopCalls, result)
	}
}

func TestStopGuestIsIdempotentForStoppedInstance(t *testing.T) {
	mustBindGuestSite(t)
	stopCalls := 0
	result, err := stopGuestWith(func() (map[string]interface{}, error) {
		return map[string]interface{}{"status": "Stopped", "pid": float64(0)}, nil
	}, func() (int, error) {
		stopCalls++
		return 0, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if stopCalls != 0 || result["stopped"] != false {
		t.Fatalf("stops=%d result=%v", stopCalls, result)
	}
}

func TestStopGuestFailsClosedOnIntermediateOrUnprovenState(t *testing.T) {
	mustBindGuestSite(t)
	if _, err := stopGuestWith(func() (map[string]interface{}, error) {
		return map[string]interface{}{"status": "Stopping", "pid": float64(4242)}, nil
	}, func() (int, error) { return 0, nil }); err == nil {
		t.Fatal("intermediate state was admitted")
	}
	reads := 0
	if _, err := stopGuestWith(func() (map[string]interface{}, error) {
		reads++
		if reads == 1 {
			return map[string]interface{}{"status": "Running", "pid": float64(4242)}, nil
		}
		return map[string]interface{}{"status": "Running", "pid": float64(4242)}, nil
	}, func() (int, error) { return 0, nil }); err == nil {
		t.Fatal("missing stopped postcondition was admitted")
	}
}

func TestStopNamedSandboxReturnsTheRequestedManagedInstance(t *testing.T) {
	result, err := stopSandboxWith("sandbox-research", func() (map[string]interface{}, error) {
		return map[string]interface{}{"status": "Stopped", "pid": float64(0)}, nil
	}, func() (int, error) {
		t.Fatal("already-stopped sandbox must not be mutated")
		return -1, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result["instance"] != "sandbox-research" || result["stopped"] != false {
		t.Fatalf("result=%v", result)
	}
}

func TestNamedSandboxStartAdmissionEnforcesManagedRunningTopology(t *testing.T) {
	tests := []struct {
		name      string
		instance  string
		inventory []incusInstanceSummary
		want      string
		wantError bool
	}{
		{name: "stopped target", instance: "sandbox-a", inventory: []incusInstanceSummary{managedLifecycleSummary("sandbox-a", "Stopped"), managedLifecycleSummary("sandbox-b", "Stopped")}, want: "Stopped"},
		{name: "running target", instance: "sandbox-a", inventory: []incusInstanceSummary{managedLifecycleSummary("sandbox-a", "Running"), managedLifecycleSummary("sandbox-b", "Stopped")}, want: "Running"},
		{name: "running target remains idempotent with a managed peer", instance: "sandbox-a", inventory: []incusInstanceSummary{managedLifecycleSummary("sandbox-a", "Running"), managedLifecycleSummary("sandbox-b", "Running")}, want: "Running"},
		{name: "other managed running", instance: "sandbox-a", inventory: []incusInstanceSummary{managedLifecycleSummary("sandbox-a", "Stopped"), managedLifecycleSummary("sandbox-b", "Running")}, want: "Stopped"},
		{name: "unrelated stopped instance is ignored", instance: "sandbox-a", inventory: []incusInstanceSummary{managedLifecycleSummary("sandbox-a", "Stopped"), {Name: "template-builder", Status: "Stopped"}}, want: "Stopped"},
		{name: "unmanaged running", instance: "sandbox-a", inventory: []incusInstanceSummary{managedLifecycleSummary("sandbox-a", "Stopped"), {Name: "template-builder", Status: "Running"}}, wantError: true},
		{name: "malformed managed running", instance: "sandbox-a", inventory: []incusInstanceSummary{managedLifecycleSummary("sandbox-a", "Stopped"), {Name: "sandbox-b", Status: "Running"}}, wantError: true},
		{name: "malformed target", instance: "sandbox-a", inventory: []incusInstanceSummary{{Name: "sandbox-a", Status: "Stopped"}}, wantError: true},
		{name: "missing", instance: "sandbox-a", inventory: []incusInstanceSummary{managedLifecycleSummary("sandbox-b", "Stopped")}, wantError: true},
		{name: "duplicate target is ambiguous", instance: "sandbox-a", inventory: []incusInstanceSummary{managedLifecycleSummary("sandbox-a", "Stopped"), managedLifecycleSummary("sandbox-a", "Stopped")}, wantError: true},
		{name: "intermediate", instance: "sandbox-a", inventory: []incusInstanceSummary{{Name: "sandbox-a", Status: "Starting"}}, wantError: true},
		{name: "unmanaged request", instance: "template-builder", inventory: []incusInstanceSummary{{Name: "template-builder", Status: "Stopped"}}, wantError: true},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := admitSandboxStart(testCase.instance, testCase.inventory)
			if testCase.wantError {
				if err == nil {
					t.Fatalf("got=%q without error", got)
				}
				return
			}
			if err != nil || got != testCase.want {
				t.Fatalf("got=%q err=%v want=%q", got, err, testCase.want)
			}
		})
	}
}

func managedLifecycleSummary(name, status string) incusInstanceSummary {
	return incusInstanceSummary{
		Name: name, Status: status,
		Config: map[string]string{
			"user.chasm.managed":          "true",
			"user.chasm.logical-name":     logicalSandboxName(name),
			"user.chasm.workspace-volume": "chasm-workspace-" + name,
			"user.chasm.shared-volume":    "chasm-shared",
		},
		Devices: map[string]map[string]string{
			"shared":    {"type": "disk", "pool": "chasm-test", "source": "chasm-shared", "path": sharedPath, "readonly": "false"},
			"workspace": {"type": "disk", "pool": "chasm-test", "source": "chasm-workspace-" + name, "path": workspacePath, "readonly": "false"},
		},
	}
}

func managedLifecycleSummaryWithBudget(name, status, budget string) incusInstanceSummary {
	summary := managedLifecycleSummary(name, status)
	summary.Config[budgetMetadataKey()] = budget
	return summary
}

func TestLifecycleLockSerializesIndependentStartPaths(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "lifecycle.lock")
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondEntered := make(chan struct{}, 1)
	errors := make(chan error, 2)
	var group sync.WaitGroup
	group.Add(2)
	go func() {
		defer group.Done()
		_, err := withLifecycleLockAt(lockPath, uint32(os.Getuid()), func() (map[string]interface{}, error) {
			close(firstEntered)
			<-releaseFirst
			return map[string]interface{}{}, nil
		})
		errors <- err
	}()
	select {
	case <-firstEntered:
	case err := <-errors:
		t.Fatalf("first lock failed: %v", err)
	case <-time.After(time.Second):
		t.Fatal("first lifecycle path did not acquire the lock")
	}
	go func() {
		defer group.Done()
		_, err := withLifecycleLockAt(lockPath, uint32(os.Getuid()), func() (map[string]interface{}, error) {
			secondEntered <- struct{}{}
			return map[string]interface{}{}, nil
		})
		errors <- err
	}()
	select {
	case <-secondEntered:
		t.Fatal("second lifecycle path entered while the first held the lock")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFirst)
	group.Wait()
	for range 2 {
		if err := <-errors; err != nil {
			t.Fatalf("lifecycle lock failed: %v", err)
		}
	}
	select {
	case <-secondEntered:
	default:
		t.Fatal("second lifecycle path never entered after lock release")
	}
}

func TestLockedAdmissionRefusesSecondSimultaneousStart(t *testing.T) {
	const memTotalMiB = 8192
	lockPath := filepath.Join(t.TempDir(), "lifecycle.lock")
	inventory := []incusInstanceSummary{
		managedLifecycleSummaryWithBudget("sandbox-a", "Stopped", "4096"),
		managedLifecycleSummaryWithBudget("sandbox-b", "Stopped", "4096"),
	}
	firstAdmitted := make(chan struct{})
	finishFirst := make(chan struct{})
	firstStatePublished := make(chan struct{})
	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	go func() {
		_, err := withLifecycleLockAt(lockPath, uint32(os.Getuid()), func() (map[string]interface{}, error) {
			if _, err := admitSandboxStartWith("sandbox-a", inventory, memTotalMiB); err != nil {
				return nil, err
			}
			close(firstAdmitted)
			<-finishFirst
			inventory[0].Status = "Running"
			close(firstStatePublished)
			return map[string]interface{}{}, nil
		})
		firstDone <- err
	}()
	select {
	case <-firstAdmitted:
	case err := <-firstDone:
		t.Fatalf("first start failed before admission: %v", err)
	case <-time.After(time.Second):
		t.Fatal("first start did not reach admission")
	}
	go func() {
		_, err := withLifecycleLockAt(lockPath, uint32(os.Getuid()), func() (map[string]interface{}, error) {
			// The channel makes the fixture's inventory write visible to the
			// race detector; flock is outside Go's synchronization model.
			<-firstStatePublished
			_, err := admitSandboxStartWith("sandbox-b", inventory, memTotalMiB)
			return nil, err
		})
		secondDone <- err
	}()
	select {
	case err := <-secondDone:
		t.Fatalf("second start completed before the first released its lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(finishFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("first start failed: %v", err)
	}
	var protocolErr *ProtocolError
	if err := <-secondDone; !errors.As(err, &protocolErr) || protocolErr.Code != "resource_exhausted" {
		t.Fatalf("second start was not refused for headroom: %v", err)
	}
}

func TestSandboxStartAdmissionEnforcesBudgetHeadroom(t *testing.T) {
	const memTotalMiB = 8192
	tests := []struct {
		name          string
		instance      string
		inventory     []incusInstanceSummary
		want          string
		wantErrorCode string
		wantErrorText string
	}{
		{
			name:      "single stopped sandbox within headroom",
			instance:  "sandbox-a",
			inventory: []incusInstanceSummary{managedLifecycleSummary("sandbox-a", "Stopped")},
			want:      "Stopped",
		},
		{
			name:     "single stopped sandbox exceeding headroom names the resource",
			instance: "sandbox-a",
			inventory: []incusInstanceSummary{
				managedLifecycleSummaryWithBudget("sandbox-a", "Stopped", "7800"),
			},
			wantErrorCode: "resource_exhausted",
			wantErrorText: "running sandbox budgets 0 MiB + requested 7800 MiB exceed this host's cap 7680 MiB (MemTotal 8192 MiB - host_reserve_mib 512)",
		},
		{
			name:     "aggregate exact headroom boundary is admitted",
			instance: "sandbox-a",
			inventory: []incusInstanceSummary{
				managedLifecycleSummaryWithBudget("sandbox-a", "Stopped", "3584"),
				managedLifecycleSummaryWithBudget("sandbox-b", "Running", "4096"),
			},
			want: "Stopped",
		},
		{
			name:     "running budgets count against headroom once co-running is enabled",
			instance: "sandbox-a",
			inventory: []incusInstanceSummary{
				managedLifecycleSummary("sandbox-a", "Stopped"),
				managedLifecycleSummaryWithBudget("sandbox-b", "Running", "2000"),
			},
			want: "Stopped",
		},
		{
			name:     "multiple running peer budgets are aggregated",
			instance: "sandbox-a",
			inventory: []incusInstanceSummary{
				managedLifecycleSummaryWithBudget("sandbox-a", "Stopped", "1024"),
				managedLifecycleSummaryWithBudget("sandbox-b", "Running", "1024"),
				managedLifecycleSummaryWithBudget("sandbox-c", "Running", "1024"),
			},
			want: "Stopped",
		},
		{
			name:     "aggregate budget one MiB above headroom is refused",
			instance: "sandbox-a",
			inventory: []incusInstanceSummary{
				managedLifecycleSummaryWithBudget("sandbox-a", "Stopped", "3585"),
				managedLifecycleSummaryWithBudget("sandbox-b", "Running", "4096"),
			},
			wantErrorCode: "resource_exhausted",
			wantErrorText: "running sandbox budgets 4096 MiB + requested 3585 MiB exceed this host's cap 7680 MiB (MemTotal 8192 MiB - host_reserve_mib 512)",
		},
		{
			name:     "invalid stored budget fails closed",
			instance: "sandbox-a",
			inventory: []incusInstanceSummary{
				managedLifecycleSummaryWithBudget("sandbox-a", "Stopped", "1048577"),
			},
			wantErrorCode: "unsafe_state",
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := admitSandboxStartWith(testCase.instance, testCase.inventory, memTotalMiB)
			if testCase.wantErrorCode != "" {
				var protocolErr *ProtocolError
				if !errors.As(err, &protocolErr) {
					t.Fatalf("err=%v is not a ProtocolError", err)
				}
				if protocolErr.Code != testCase.wantErrorCode {
					t.Fatalf("code=%q err=%v", protocolErr.Code, err)
				}
				if testCase.wantErrorText != "" && !strings.Contains(protocolErr.Message, testCase.wantErrorText) {
					t.Fatalf("message=%q missing %q", protocolErr.Message, testCase.wantErrorText)
				}
				return
			}
			if err != nil || got != testCase.want {
				t.Fatalf("got=%q err=%v want=%q", got, err, testCase.want)
			}
		})
	}
}

func TestSandboxBudgetFromConfig(t *testing.T) {
	tests := []struct {
		name   string
		config map[string]string
		want   int64
		fails  bool
	}{
		{name: "absent defaults", config: nil, want: protocol.DefaultSandboxBudgetMemoryMiB},
		{name: "empty defaults", config: map[string]string{"user.chasm.budget-memory-mib": ""}, want: protocol.DefaultSandboxBudgetMemoryMiB},
		{name: "stored value", config: map[string]string{"user.chasm.budget-memory-mib": "2048"}, want: 2048},
		{name: "minimum bound", config: map[string]string{"user.chasm.budget-memory-mib": "512"}, want: 512},
		{name: "maximum safety bound", config: map[string]string{"user.chasm.budget-memory-mib": "1048576"}, want: 1048576},
		{name: "above the old fixed cap is a valid declared value", config: map[string]string{"user.chasm.budget-memory-mib": "70000"}, want: 70000},
		{name: "below minimum fails closed", config: map[string]string{"user.chasm.budget-memory-mib": "511"}, fails: true},
		{name: "above the safety bound fails closed", config: map[string]string{"user.chasm.budget-memory-mib": "1048577"}, fails: true},
		{name: "non-numeric fails closed", config: map[string]string{"user.chasm.budget-memory-mib": "abc"}, fails: true},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := sandboxBudgetFromConfig(testCase.config)
			if testCase.fails {
				if err == nil {
					t.Fatalf("admitted %v", testCase.config)
				}
				return
			}
			if err != nil || got != testCase.want {
				t.Fatalf("got=%d err=%v want=%d", got, err, testCase.want)
			}
		})
	}
}
