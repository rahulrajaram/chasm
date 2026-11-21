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

// This file pins chasm resize (idiom 3): the supported path for resizing a
// stopped sandbox, with host-derived caps (idiom 4), record-and-enforced
// written together (idiom 1), a fixed application order, and an explicit
// report when a partial application happens.

// resizeFixture builds a stopped managed summary plus seams that record
// the applied order and the exact (instance, value, fromProfile) tuples
// each apply seam received. Scenario tests mutate the fixture fields.
type resizeFixture struct {
	summary     incusInstanceSummary
	status      string
	memoryCap   [3]int64
	poolMib     int64
	order       *[]string
	memoryOps   *[]appliedResize
	cpuOps      *[]appliedResize
	diskOps     *[]appliedRootDisk
	failCPU     error
	readBackMib *int64 // expanded root size served by the expandedRoot seam
}

// appliedResize records one apply call's instance and exact value.
type appliedResize struct {
	instance string
	value    int64
}

// appliedRootDisk records one root-disk apply call's instance, value, and
// whether the device is inherited from the site profile.
type appliedRootDisk struct {
	instance    string
	value       int64
	fromProfile bool
}

func newResizeFixture(t *testing.T) *resizeFixture {
	t.Helper()
	mustBindDefaultGuestSite(t)
	site := siteconfig.Active()
	summary := managedSummaryForSite("sandbox-research", "Stopped")
	summary.Config[site.MetadataKey("budget-memory-mib")] = "3072"
	summary.Config[site.MetadataKey("budget-cpu")] = "2"
	summary.ExpandedConfig = map[string]string{"limits.memory": "3GiB", "limits.cpu": "2"}
	summary.ExpandedDevices = map[string]map[string]string{
		"root": {"type": "disk", "pool": site.StoragePool, "path": "/", "size": "60GiB"},
	}
	// The fixture's root device is instance-owned (fromProfile=false); the
	// profile-inherited variant deletes it in the ownership test.
	summary.Devices["root"] = map[string]string{"type": "disk", "pool": site.StoragePool, "path": "/", "size": "60GiB"}
	order := []string{}
	memoryOps := []appliedResize{}
	cpuOps := []appliedResize{}
	diskOps := []appliedRootDisk{}
	fixture := &resizeFixture{
		summary:   summary,
		status:    "Stopped",
		memoryCap: [3]int64{7680, 8192, 512},
		poolMib:   80 * 1024,
		order:     &order,
		memoryOps: &memoryOps,
		cpuOps:    &cpuOps,
		diskOps:   &diskOps,
	}
	return fixture
}

func (fixture *resizeFixture) seams() resizeSeams {
	return resizeSeams{
		underLock: func(operation func() (map[string]interface{}, error)) (map[string]interface{}, error) {
			return operation()
		},
		checkManaged: func(string) error { return nil },
		readState: func(string) (map[string]interface{}, error) {
			return map[string]interface{}{"status": fixture.status}, nil
		},
		listSummaries: func() ([]incusInstanceSummary, error) {
			return []incusInstanceSummary{fixture.summary}, nil
		},
		memoryCap: func() (int64, int64, int64, error) {
			return fixture.memoryCap[0], fixture.memoryCap[1], fixture.memoryCap[2], nil
		},
		poolSizeMib: func() (int64, error) { return fixture.poolMib, nil },
		applyMemory: func(instance string, mib int64) error {
			*fixture.order = append(*fixture.order, "memory")
			*fixture.memoryOps = append(*fixture.memoryOps, appliedResize{instance: instance, value: mib})
			return nil
		},
		applyCPU: func(instance string, cpu int64) error {
			*fixture.order = append(*fixture.order, "cpu")
			*fixture.cpuOps = append(*fixture.cpuOps, appliedResize{instance: instance, value: cpu})
			return fixture.failCPU
		},
		applyRootDisk: func(instance string, mib int64, fromProfile bool) error {
			*fixture.order = append(*fixture.order, "root-disk")
			*fixture.diskOps = append(*fixture.diskOps, appliedRootDisk{instance: instance, value: mib, fromProfile: fromProfile})
			return nil
		},
		expandedRoot: func(instance string) (int64, bool, error) {
			if fixture.readBackMib == nil {
				return 0, false, nil
			}
			return *fixture.readBackMib, true, nil
		},
	}
}

// TestResizeEqualRootDiskTargetWritesNothing pins the proven no-op: a
// root-disk target equal to the current size must issue NO apply write (a
// profile-inherited device is never turned into an instance override) and
// must still prove the value by read-back, so the documented proven no-op
// holds. Mutation checked: routing the equal target through applyRootDisk
// fails the zero-calls assertion, and dropping the read-back proof fails
// the postcondition case.
func TestResizeEqualRootDiskTargetWritesNothing(t *testing.T) {
	fixture := newResizeFixture(t)
	delete(fixture.summary.Devices, "root") // profile-inherited root device
	readBack := int64(61440)
	fixture.readBackMib = &readBack
	result, err := resizeSandboxWith("sandbox-research", resizeTargets{rootDisk: 61440}, fixture.seams())
	if err != nil {
		t.Fatalf("an equal root-disk target must be a proven no-op: %v", err)
	}
	if ops := *fixture.diskOps; len(ops) != 0 {
		t.Fatalf("an equal target must not write: applyRootDisk received %v", ops)
	}
	if len(*fixture.order) != 0 {
		t.Fatalf("an equal target must not enter the apply order: %v", *fixture.order)
	}
	changes, _ := result["resized"].([]map[string]interface{})
	if len(changes) != 1 || changes[0]["resource"] != "root_disk" ||
		changes[0]["before"] != int64(61440) || changes[0]["after"] != int64(61440) {
		t.Fatalf("the equal no-op must report before == after: %v", changes)
	}
	// A read-back that disagrees is a postcondition failure, never success.
	readBack = int64(60*1024 - 1)
	_, err = resizeSandboxWith("sandbox-research", resizeTargets{rootDisk: 61440}, fixture.seams())
	var protocolErr *ProtocolError
	if !errors.As(err, &protocolErr) || protocolErr.Code != "postcondition_failed" {
		t.Fatalf("a disagreeing no-op read-back must fail closed: %v", err)
	}
}

// TestResizeEqualRootDiskReadBackFailureReportsPartiallyApplied pins the
// partial report on the equal-root-disk proof: a memory target that was
// applied, followed by an equal root-disk target whose read-back proof
// fails (an API error, then a disagreeing value), must report the memory
// change as applied and the root disk as not applied — the equal target
// stays pending until proven. Mutation checked: restoring the bare error
// returns on the equal-proof path fails both message assertions.
func TestResizeEqualRootDiskReadBackFailureReportsPartiallyApplied(t *testing.T) {
	fixture := newResizeFixture(t)
	seams := fixture.seams()
	seams.expandedRoot = func(string) (int64, bool, error) {
		return 0, false, protoError("operation_failed", "the expanded size is unreadable")
	}
	_, err := resizeSandboxWith("sandbox-research", resizeTargets{memory: 4096, rootDisk: 61440}, seams)
	code, message := protocolCode(t, err)
	if code != "operation_failed" ||
		!strings.Contains(message, "applied: memory 3GiB->4GiB") ||
		!strings.Contains(message, "not applied: root disk") ||
		!strings.Contains(message, "the expanded size is unreadable") {
		t.Fatalf("a failed equal-proof read-back must report the partial state: %s/%s", code, message)
	}
	seams.expandedRoot = func(string) (int64, bool, error) { return 61439, true, nil }
	_, err = resizeSandboxWith("sandbox-research", resizeTargets{memory: 4096, rootDisk: 61440}, seams)
	code, message = protocolCode(t, err)
	if code != "postcondition_failed" ||
		!strings.Contains(message, "applied: memory 3GiB->4GiB") ||
		!strings.Contains(message, "not applied: root disk") {
		t.Fatalf("a disagreeing equal-proof read-back must report the partial state: %s/%s", code, message)
	}
}

// TestResizeBeforePrefersUnknownOverDeclaredOnUnsupportedEnforced pins
// before-precedence: when the enforced value is unsupported, the resize
// reports `before` as an explicit unknown even though a declared value
// exists — the declared record must never stand in for a value Incus
// enforces but chasm cannot read. Mutation checked: restoring the old
// declared-first fallback fails the unknown assertion.
func TestResizeBeforePrefersUnknownOverDeclaredOnUnsupportedEnforced(t *testing.T) {
	fixture := newResizeFixture(t)
	fixture.summary.ExpandedConfig = map[string]string{"limits.memory": "50%", "limits.cpu": "2"}
	result, err := resizeSandboxWith("sandbox-research", resizeTargets{memory: 4096}, fixture.seams())
	if err != nil {
		t.Fatalf("an unsupported enforced value must not abort the resize: %v", err)
	}
	changes, _ := result["resized"].([]map[string]interface{})
	if len(changes) != 1 {
		t.Fatalf("one change expected: %v", changes)
	}
	if _, known := changes[0]["before"]; known {
		t.Fatalf("before must be an explicit unknown beside an unsupported enforced value: %v", changes[0])
	}
	if reason, _ := changes[0]["before_unknown"].(string); !strings.Contains(reason, "50%") {
		t.Fatalf("before_unknown must carry the reason: %v", changes[0])
	}
}

func protocolCode(t *testing.T, err error) (string, string) {
	t.Helper()
	var protocolErr *ProtocolError
	if !errors.As(err, &protocolErr) {
		t.Fatalf("expected a ProtocolError, got %v", err)
	}
	return protocolErr.Code, protocolErr.Message
}

func TestResizeAppliesTargetsInFixedOrderAndReportsBeforeAfter(t *testing.T) {
	fixture := newResizeFixture(t)
	result, err := resizeSandboxWith("sandbox-research", resizeTargets{memory: 4096, cpu: 4, rootDisk: 65536}, fixture.seams())
	if err != nil {
		t.Fatalf("resize must apply: %v", err)
	}
	if strings.Join(*fixture.order, ",") != "memory,cpu,root-disk" {
		t.Fatalf("resize must apply memory, then CPU, then root disk: %v", *fixture.order)
	}
	// The apply seams receive exactly the instance and value requested.
	if len(*fixture.memoryOps) != 1 || (*fixture.memoryOps)[0] != (appliedResize{"sandbox-research", 4096}) {
		t.Fatalf("memory apply = %v", *fixture.memoryOps)
	}
	if len(*fixture.cpuOps) != 1 || (*fixture.cpuOps)[0] != (appliedResize{"sandbox-research", 4}) {
		t.Fatalf("cpu apply = %v", *fixture.cpuOps)
	}
	if len(*fixture.diskOps) != 1 || (*fixture.diskOps)[0] != (appliedRootDisk{"sandbox-research", 65536, false}) {
		t.Fatalf("root disk apply = %v", *fixture.diskOps)
	}
	changes, _ := result["resized"].([]map[string]interface{})
	if len(changes) != 3 {
		t.Fatalf("three resources changed, got %v", result)
	}
	if changes[0]["resource"] != "memory" || changes[0]["before"] != int64(3072) || changes[0]["after"] != int64(4096) || changes[0]["unit"] != "MiB" {
		t.Fatalf("memory change = %v", changes[0])
	}
	if changes[1]["resource"] != "cpu" || changes[1]["before"] != int64(2) || changes[1]["after"] != int64(4) || changes[1]["unit"] != "count" {
		t.Fatalf("cpu change = %v", changes[1])
	}
	if changes[2]["resource"] != "root_disk" || changes[2]["before"] != int64(61440) || changes[2]["after"] != int64(65536) || changes[2]["unit"] != "MiB" {
		t.Fatalf("root disk change = %v", changes[2])
	}
}

// TestResizeRecordsFromProfileTrueAndFalse pins the device-ownership flag
// the root-disk apply receives: an instance whose root device comes only
// from the expanded profile must receive fromProfile=true (needing
// `config device override`), an instance that owns the device must receive
// false (`config device set`). Mutation checked: the call site passing a
// hard-coded false (or true) fails exactly one of the two cases.
func TestResizeRecordsFromProfileTrueAndFalse(t *testing.T) {
	profileInherited := newResizeFixture(t)
	delete(profileInherited.summary.Devices, "root")
	if _, err := resizeSandboxWith("sandbox-research", resizeTargets{rootDisk: 65536}, profileInherited.seams()); err != nil {
		t.Fatal(err)
	}
	if ops := *profileInherited.diskOps; len(ops) != 1 || !ops[0].fromProfile {
		t.Fatalf("a profile-inherited root device must apply with fromProfile=true: %v", ops)
	}
	instanceOwned := newResizeFixture(t)
	if _, err := resizeSandboxWith("sandbox-research", resizeTargets{rootDisk: 65536}, instanceOwned.seams()); err != nil {
		t.Fatal(err)
	}
	if ops := *instanceOwned.diskOps; len(ops) != 1 || ops[0].fromProfile {
		t.Fatalf("an instance-owned root device must apply with fromProfile=false: %v", ops)
	}
}

func TestResizeMemoryRepairsDeclaredAndEnforcedDrift(t *testing.T) {
	fixture := newResizeFixture(t)
	site := siteconfig.Active()
	// Drift created outside chasm: the record says 20480 MiB while Incus
	// enforces 48GiB. Both must land on the resize target.
	fixture.summary.Config[site.MetadataKey("budget-memory-mib")] = "20480"
	fixture.summary.ExpandedConfig = map[string]string{"limits.memory": "48GiB", "limits.cpu": "2"}
	fixture.memoryCap = [3]int64{65536, 73728, 8192}
	var applied int64
	seams := fixture.seams()
	seams.applyMemory = func(_ string, mib int64) error {
		applied = mib
		*fixture.order = append(*fixture.order, "memory")
		return nil
	}
	result, err := resizeSandboxWith("sandbox-research", resizeTargets{memory: 20480}, seams)
	if err != nil {
		t.Fatalf("drift repair must apply: %v", err)
	}
	if applied != 20480 {
		t.Fatalf("the enforced value must be rewritten to the target, got %d", applied)
	}
	changes, _ := result["resized"].([]map[string]interface{})
	if len(changes) != 1 || changes[0]["before"] != int64(49152) || changes[0]["after"] != int64(20480) || changes[0]["unit"] != "MiB" {
		t.Fatalf("before must report the enforced 48GiB: %v", changes)
	}
}

func TestResizeRefusesARunningSandboxWithoutTouchingIt(t *testing.T) {
	fixture := newResizeFixture(t)
	fixture.status = "Running"
	_, err := resizeSandboxWith("sandbox-research", resizeTargets{memory: 4096}, fixture.seams())
	code, message := protocolCode(t, err)
	if code != "invalid_state" || !strings.Contains(message, "chasm stop") {
		t.Fatalf("a running sandbox must be refused with the stop command: %s/%s", code, message)
	}
	if len(*fixture.order) != 0 {
		t.Fatalf("a refused resize must not apply anything: %v", *fixture.order)
	}
}

func TestResizeMemoryRefusesAboveTheHostCapNamingItsSource(t *testing.T) {
	fixture := newResizeFixture(t)
	_, err := resizeSandboxWith("sandbox-research", resizeTargets{memory: 8192}, fixture.seams())
	code, message := protocolCode(t, err)
	if code != "resource_exhausted" || !strings.Contains(message, "MemTotal 8192 MiB - host_reserve_mib 512") {
		t.Fatalf("the cap refusal must name its source: %s/%s", code, message)
	}
	if len(*fixture.order) != 0 {
		t.Fatalf("a refused target must not apply anything: %v", *fixture.order)
	}
}

func TestResizeCPURefusesAboveTheHostCPUCount(t *testing.T) {
	fixture := newResizeFixture(t)
	_, err := resizeSandboxWith("sandbox-research", resizeTargets{cpu: 1 << 20}, fixture.seams())
	code, message := protocolCode(t, err)
	if code != "invalid_cpu_budget" || !strings.Contains(message, "budget_cpu must be an integer CPU count") {
		t.Fatalf("an out-of-range CPU target must be refused by the protocol range: %s/%s", code, message)
	}
}

// TestResizeCPURefusesAboveTheInjectedHostCPUCount pins the wiring of the
// host CPU-count cap into resize: with the host's count injected at 16, a
// target between the cap and the protocol bound (64) must be refused as
// resource_exhausted naming "host CPU count", with nothing applied.
// Mutation checked: deleting the refuseCpuBudgetAboveHostCap call in
// validateResizeCPU, or inverting the comparison to >=, fails this test.
func TestResizeCPURefusesAboveTheInjectedHostCPUCount(t *testing.T) {
	original := hostCpuCount
	t.Cleanup(func() { hostCpuCount = original })
	hostCpuCount = func() int64 { return 16 }
	fixture := newResizeFixture(t)
	_, err := resizeSandboxWith("sandbox-research", resizeTargets{cpu: 64}, fixture.seams())
	code, message := protocolCode(t, err)
	if code != "resource_exhausted" || !strings.Contains(message, "host CPU count") {
		t.Fatalf("the host CPU cap must refuse the target naming its source: %s/%s", code, message)
	}
	if len(*fixture.order) != 0 {
		t.Fatalf("a refused CPU target must not apply anything: %v", *fixture.order)
	}
	// The boundary: a target exactly at the cap is not refused by the cap
	// (it proceeds past validation and applies, so the fixture reports the
	// apply instead of a resource_exhausted refusal).
	boundary := newResizeFixture(t)
	if _, err := resizeSandboxWith("sandbox-research", resizeTargets{cpu: 16}, boundary.seams()); err != nil {
		var protocolErr *ProtocolError
		if errors.As(err, &protocolErr) && protocolErr.Code == "resource_exhausted" {
			t.Fatalf("a CPU target at the host cap must not be refused by the cap: %v", err)
		}
		t.Fatalf("a CPU target at the cap must pass validation: %v", err)
	}
	if len(*boundary.cpuOps) != 1 {
		t.Fatalf("a CPU target at the cap must apply: %v", *boundary.cpuOps)
	}
}

// TestResizeRootDiskRefusesSmallerAndProvesEqualWithoutWriting pins the
// disk rules: a smaller target is refused naming the current size; an
// equal target is a proven no-op with NO write (a profile-inherited device
// is never turned into an instance override) whose value is still proven
// by read-back (idiom 3), so the change is reported like any other.
func TestResizeRootDiskRefusesSmallerAndProvesEqualWithoutWriting(t *testing.T) {
	fixture := newResizeFixture(t)
	_, err := resizeSandboxWith("sandbox-research", resizeTargets{rootDisk: 30720}, fixture.seams())
	code, message := protocolCode(t, err)
	if code != "invalid_state" || !strings.Contains(message, "current root disk is 60GiB") {
		t.Fatalf("shrink refusal must name the current size: %s/%s", code, message)
	}
	if len(*fixture.order) != 0 {
		t.Fatalf("a refused resize must not apply anything: %v", *fixture.order)
	}
	readBack := int64(61440)
	fixture.readBackMib = &readBack
	result, err := resizeSandboxWith("sandbox-research", resizeTargets{rootDisk: 61440}, fixture.seams())
	if err != nil {
		t.Fatalf("re-applying the current size must be a proven no-op: %v", err)
	}
	changes, _ := result["resized"].([]map[string]interface{})
	if len(changes) != 1 || changes[0]["before"] != int64(61440) || changes[0]["after"] != int64(61440) {
		t.Fatalf("the equal resize must be reported: %v", changes)
	}
	if ops := *fixture.diskOps; len(ops) != 0 {
		t.Fatalf("the equal no-op must not write at all: applyRootDisk received %v", ops)
	}
}

func TestResizeRootDiskRefusesAboveThePoolCeiling(t *testing.T) {
	fixture := newResizeFixture(t)
	fixture.poolMib = 64 * 1024
	_, err := resizeSandboxWith("sandbox-research", resizeTargets{rootDisk: 66560}, fixture.seams())
	code, message := protocolCode(t, err)
	if code != "resource_exhausted" || !strings.Contains(message, "exceeds the storage pool's configured size 64GiB") {
		t.Fatalf("the pool ceiling refusal must name both numbers: %s/%s", code, message)
	}
}

func TestResizeWithoutATargetFailsClosed(t *testing.T) {
	fixture := newResizeFixture(t)
	_, err := resizeSandboxWith("sandbox-research", resizeTargets{}, fixture.seams())
	code, _ := protocolCode(t, err)
	if code != "invalid_fields" {
		t.Fatalf("a resize with no target must fail closed, got %s", code)
	}
}

func TestResizeReportsExactlyWhatWasAppliedWhenALaterStepFails(t *testing.T) {
	fixture := newResizeFixture(t)
	fixture.failCPU = protoError("operation_failed", "the CPU limit could not be applied")
	_, err := resizeSandboxWith("sandbox-research", resizeTargets{memory: 4096, cpu: 4}, fixture.seams())
	code, message := protocolCode(t, err)
	if code != "operation_failed" ||
		!strings.Contains(message, "applied: memory 3GiB->4GiB") ||
		!strings.Contains(message, "not applied: CPU") ||
		!strings.Contains(message, "the CPU limit could not be applied") {
		t.Fatalf("a partial application must name applied and pending changes: %s/%s", code, message)
	}
}

func TestResizeFailureOnTheFirstStepCarriesNoPartialReport(t *testing.T) {
	fixture := newResizeFixture(t)
	seams := fixture.seams()
	seams.applyMemory = func(string, int64) error { return protoError("operation_failed", "budget write failed") }
	_, err := resizeSandboxWith("sandbox-research", resizeTargets{memory: 4096}, seams)
	code, message := protocolCode(t, err)
	if code != "operation_failed" || strings.Contains(message, "partially applied") {
		t.Fatalf("nothing applied means no partial report: %s/%s", code, message)
	}
}

// TestResizeRootDiskOverridesProfileDevice pins the device argv: a root
// device inherited from the site profile needs `config device override`, an
// instance-owned device needs `config device set`.
func TestResizeRootDiskOverridesProfileDevice(t *testing.T) {
	profile := rootDeviceArguments("sandbox-research", 65536, true)
	want := []string{"config", "device", "override", "sandbox-research", "root", "size=65536MiB"}
	if len(profile) != len(want) {
		t.Fatalf("override argv = %v, want %v", profile, want)
	}
	for index := range want {
		if profile[index] != want[index] {
			t.Fatalf("override argv = %v, want %v", profile, want)
		}
	}
	instance := rootDeviceArguments("sandbox-research", 65536, false)
	if instance[2] != "set" {
		t.Fatalf("an instance-owned device must be set, got %v", instance)
	}
}

// TestApplyRootDiskResizeProvesReadBack pins the root-disk proof through
// the query seam: the expanded device size is read back and compared, and
// the runIncus stubs assert the exact argv — anything else (a changed
// command, a hard-coded pool, `storage get`) fails the test. Mutation
// checked: `if !present || expanded != mib` reduced to `if false` fails
// the mismatched case; querying volume.size instead of the expanded device
// fails the argv assertion.
func TestApplyRootDiskResizeProvesReadBack(t *testing.T) {
	originalRunIncus, originalQuery := runIncus, incusQueryGETFn
	t.Cleanup(func() { runIncus, incusQueryGETFn = originalRunIncus, originalQuery })
	var deviceArgv []string
	runIncus = func(arguments []string, _ io.Reader, _ io.Writer, _ []*os.File, _ time.Duration) (int, error) {
		if len(arguments) != 6 || arguments[0] != "config" || arguments[1] != "device" || (arguments[2] != "override" && arguments[2] != "set") {
			t.Fatalf("unexpected incus argv: %v", arguments)
		}
		deviceArgv = arguments
		return 0, nil
	}
	expanded := "65536MiB"
	incusQueryGETFn = func(path string) (map[string]interface{}, error) {
		if path != "/1.0/instances/sandbox-research" {
			t.Fatalf("read back queried %q", path)
		}
		if expanded == "" {
			return map[string]interface{}{"expanded_devices": map[string]interface{}{}}, nil
		}
		return map[string]interface{}{"expanded_devices": map[string]interface{}{
			"root": map[string]interface{}{"size": expanded},
		}}, nil
	}
	// Matching read-back: override argv for a profile-inherited device.
	if err := applyRootDiskResize("sandbox-research", 65536, true); err != nil {
		t.Fatalf("a matching read-back must prove the resize: %v", err)
	}
	want := []string{"config", "device", "override", "sandbox-research", "root", "size=65536MiB"}
	if fmt.Sprint(deviceArgv) != fmt.Sprint(want) {
		t.Fatalf("device argv = %v, want %v", deviceArgv, want)
	}
	// Matching read-back with an instance-owned device: `device set`.
	if err := applyRootDiskResize("sandbox-research", 65536, false); err != nil {
		t.Fatalf("a matching read-back must prove the resize: %v", err)
	}
	if deviceArgv[2] != "set" {
		t.Fatalf("an instance-owned device must be set, got %v", deviceArgv)
	}
	// A disagreeing read-back is a postcondition failure, never success.
	expanded = "60GiB"
	err := applyRootDiskResize("sandbox-research", 65536, true)
	var protocolErr *ProtocolError
	if !errors.As(err, &protocolErr) || protocolErr.Code != "postcondition_failed" {
		t.Fatalf("a mismatched read-back must fail closed: %v", err)
	}
	// An absent expanded root device cannot be proven either.
	expanded = ""
	if err := applyRootDiskResize("sandbox-research", 65536, true); !errors.As(err, &protocolErr) || protocolErr.Code != "postcondition_failed" {
		t.Fatalf("an absent expanded size must fail closed: %v", err)
	}
}

// TestStoragePoolSizeMibFailsClosedWithoutAValue pins the pool ceiling's
// fail-closed read: an unreadable or unset pool size is an error, never an
// unbounded admission.
func TestStoragePoolSizeMibFailsClosedWithoutAValue(t *testing.T) {
	mustBindDefaultGuestSite(t)
	original := runIncus
	t.Cleanup(func() { runIncus = original })
	runIncus = func(arguments []string, _ io.Reader, stdout io.Writer, _ []*os.File, _ time.Duration) (int, error) {
		// The ceiling may only come from the site pool's configured size:
		// the stub asserts the exact `storage get <site pool> size` argv, so
		// a mutated path (another pool, another key, a show parse) cannot
		// feed this test a value. Mutation checked: changing the read to
		// `storage show` or a hard-coded pool fails the argv assertion.
		want := []string{"storage", "get", siteconfig.Active().StoragePool, "size"}
		if fmt.Sprint(arguments) != fmt.Sprint(want) {
			t.Fatalf("storage size read argv = %v, want %v", arguments, want)
		}
		return 0, nil
	}
	if _, err := storagePoolSizeMib(); err == nil {
		t.Fatal("a pool without a configured size must fail closed")
	}
	runIncus = func(arguments []string, _ io.Reader, stdout io.Writer, _ []*os.File, _ time.Duration) (int, error) {
		_, _ = stdout.Write([]byte("64GiB\n"))
		return 0, nil
	}
	mib, err := storagePoolSizeMib()
	if err != nil || mib != 65536 {
		t.Fatalf("pool size read = %d, %v; want 65536 MiB", mib, err)
	}
}

// TestResizeTargetsFromRequestMapsAllThreeFields pins the executor's
// field-to-target wiring: each request field lands in exactly its target.
// Mutation checked: mapping budget_cpu into memory, dropping root_disk_mib,
// or routing resize_sandbox away from the resize handler all fail here.
func TestResizeTargetsFromRequestMapsAllThreeFields(t *testing.T) {
	request, err := protocol.ParseExecutor([]byte(`{"version":1,"request_id":"req-1","operation":"resize_sandbox","instance":"sandbox-research","budget_memory_mib":4096,"budget_cpu":4,"root_disk_mib":65536}`))
	if err != nil {
		t.Fatal(err)
	}
	targets := resizeTargetsFromRequest(request)
	if targets != (resizeTargets{memory: 4096, cpu: 4, rootDisk: 65536}) {
		t.Fatalf("targets = %v, want {4096 4 65536}", targets)
	}
	single := func(body string) resizeTargets {
		t.Helper()
		parsed, err := protocol.ParseExecutor([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		return resizeTargetsFromRequest(parsed)
	}
	if got := single(`{"version":1,"request_id":"r","operation":"resize_sandbox","instance":"sandbox-research","budget_memory_mib":2048}`); got != (resizeTargets{memory: 2048}) {
		t.Fatalf("memory-only targets = %v", got)
	}
	if got := single(`{"version":1,"request_id":"r","operation":"resize_sandbox","instance":"sandbox-research","budget_cpu":4}`); got != (resizeTargets{cpu: 4}) {
		t.Fatalf("cpu-only targets = %v", got)
	}
	if got := single(`{"version":1,"request_id":"r","operation":"resize_sandbox","instance":"sandbox-research","root_disk_mib":65536}`); got != (resizeTargets{rootDisk: 65536}) {
		t.Fatalf("root-disk-only targets = %v", got)
	}
}

// TestDispatchRoutesResizeSandbox pins the dispatch wiring: a resize
// request reaches the resize handler — whose distinctive no-target refusal
// is the resize path's own — instead of falling through to stop, pause, or
// unknown_operation. Mutation checked: routing resize_sandbox to
// stopSandbox or pauseSandbox fails, because neither produces the
// invalid_fields refusal naming resize_sandbox (they enter the lifecycle
// lock and managed-sandbox check instead and report a different code).
func TestDispatchRoutesResizeSandbox(t *testing.T) {
	mustBindDefaultGuestSite(t)
	request := protocol.Request{Operation: "resize_sandbox", Fields: map[string]interface{}{"instance": "sandbox-research"}}
	_, err := Dispatch(request)
	var protocolErr *ProtocolError
	if !errors.As(err, &protocolErr) || protocolErr.Code != "invalid_fields" ||
		!strings.Contains(protocolErr.Message, "resize_sandbox requires at least one of") {
		t.Fatalf("Dispatch must route resize_sandbox to the resize handler (distinctive invalid_fields refusal), got %v", err)
	}
}

// TestResizeOverAnUnsupportedEnforcedValueSucceeds pins the drift-repair
// path over a value chasm cannot parse: a stopped sandbox with
// limits.memory "50%" plus resize --memory succeeds, applies both writes
// (the budget key and the enforced limit), and reports the before value as
// an explicit unknown instead of aborting. Mutation checked: aborting on
// the unsupported enforced value (the old fail-closed before-derivation)
// makes this test fail with unsafe_state.
func TestResizeOverAnUnsupportedEnforcedValueSucceeds(t *testing.T) {
	fixture := newResizeFixture(t)
	site := siteconfig.Active()
	delete(fixture.summary.Config, site.MetadataKey("budget-memory-mib"))
	fixture.summary.ExpandedConfig = map[string]string{"limits.memory": "50%", "limits.cpu": "2"}
	result, err := resizeSandboxWith("sandbox-research", resizeTargets{memory: 4096}, fixture.seams())
	if err != nil {
		t.Fatalf("an unsupported enforced value must not abort the resize: %v", err)
	}
	if ops := *fixture.memoryOps; len(ops) != 1 || ops[0].value != 4096 {
		t.Fatalf("the memory write must be applied: %v", ops)
	}
	changes, _ := result["resized"].([]map[string]interface{})
	if len(changes) != 1 {
		t.Fatalf("one change expected: %v", changes)
	}
	if _, known := changes[0]["before"]; known {
		t.Fatalf("the before value must be an explicit unknown: %v", changes[0])
	}
	if reason, _ := changes[0]["before_unknown"].(string); !strings.Contains(reason, "limits.memory '50%' is not a supported size") {
		t.Fatalf("the unknown must name the value: %v", changes[0])
	}
	if changes[0]["after"] != int64(4096) || changes[0]["unit"] != "MiB" {
		t.Fatalf("the applied value must be reported: %v", changes[0])
	}
}
