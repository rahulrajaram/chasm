package execserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rahulrajaram/chasm/internal/siteconfig"
)

// This file pins idiom 2 (show the truth from every view, flag drift) and
// the idiom-4 host-reserve and pool-fill warnings, against JSON-shaped
// fixtures served through the showSeams injection surface. Each test names
// the mutation it survives: the behavior disappears and the test fails.

// showFixtureSeams serves one instance summary, one state document, one
// volume state, and pool documents, the way the Incus API would.
type showFixture struct {
	summary     incusInstanceSummary
	state       map[string]interface{}
	volumeState map[string]interface{}
	poolSpace   map[string]interface{}
	poolConfig  map[string]interface{}
	hostCap     func() (int64, int64, int64, error)
	meminfo     map[string]interface{}
	resident    map[int64]int64
	footprint   func(path string) (int64, int64, int64, error)
}

func (fixture showFixture) seams() showSeams {
	return showSeams{
		listSummaries: func() ([]incusInstanceSummary, error) {
			return []incusInstanceSummary{fixture.summary}, nil
		},
		readState: func(string) (map[string]interface{}, error) { return fixture.state, nil },
		volumeState: func(_, _ string) (map[string]interface{}, error) {
			return fixture.volumeState, nil
		},
		poolResources: func(string) (map[string]interface{}, error) {
			return fixture.poolSpace, nil
		},
		poolConfig: func(string) (map[string]interface{}, error) {
			return fixture.poolConfig, nil
		},
		hostMemory:  fixture.hostCap,
		meminfo:     func() (map[string]interface{}, error) { return fixture.meminfo, nil },
		residentMib: func(pid int64) (int64, error) { return fixture.resident[pid], nil },
		pathFootprint: func(path string) (int64, int64, int64, error) {
			if fixture.footprint == nil {
				return 0, 0, 0, errFixtureNoLoop()
			}
			return fixture.footprint(path)
		},
	}
}

func errFixtureNoLoop() error { return errNotLoopFile }

// errNotLoopFile is a stand-in error for fixtures without a loop file.
var errNotLoopFile = &fixtureError{}

type fixtureError struct{}

func (*fixtureError) Error() string { return "no loop file in fixture" }

// driftedShowSummary is the canonical drift fixture: the budget key
// declares 20480 MiB while Incus enforces 48 GiB.
func driftedShowSummary(t *testing.T) incusInstanceSummary {
	t.Helper()
	mustBindDefaultGuestSite(t)
	summary := managedSummaryForSite("sandbox-development", "Running")
	site := siteconfig.Active()
	summary.Config[site.MetadataKey("budget-memory-mib")] = "20480"
	summary.Config[site.MetadataKey("budget-cpu")] = "14"
	summary.ExpandedConfig = map[string]string{"limits.memory": "48GiB", "limits.cpu": "14"}
	summary.Devices["root"] = map[string]string{"type": "disk", "pool": site.StoragePool, "path": "/", "size": "150GiB"}
	summary.ExpandedDevices = map[string]map[string]string{
		"root": {"type": "disk", "pool": site.StoragePool, "path": "/", "size": "150GiB"},
	}
	return summary
}

// runningShowState is a guest-agent-reporting state document.
func runningShowState(pid int64) map[string]interface{} {
	return map[string]interface{}{
		"status": "Running",
		"pid":    pid,
		"memory": map[string]interface{}{"usage": 3254779904, "total": 50465865728},
		"disk":   map[string]interface{}{"root": map[string]interface{}{"usage": 3328599654}},
	}
}

func showViews(t *testing.T, fixture showFixture) map[string]interface{} {
	t.Helper()
	views, err := sandboxShowViewsWith("sandbox-development", "Running", 4242, fixture.state, fixture.seams())
	if err != nil {
		t.Fatal(err)
	}
	return views
}

func viewValue(t *testing.T, resource map[string]interface{}, name string) map[string]interface{} {
	t.Helper()
	value, ok := resource[name].(map[string]interface{})
	if !ok {
		t.Fatalf("%s view is not a value object: %v", name, resource[name])
	}
	return value
}

func viewMib(t *testing.T, resource map[string]interface{}, name string) int64 {
	t.Helper()
	value := viewValue(t, resource, name)
	mib, ok := numericInt64(value["mib"])
	if !ok {
		t.Fatalf("%s view is not a known MiB value: %v", name, value)
	}
	if source, _ := value["source"].(string); source == "" {
		t.Fatalf("%s view does not name its source: %v", name, value)
	}
	return mib
}

func viewUnknown(t *testing.T, resource map[string]interface{}, name, reason string) {
	t.Helper()
	value := viewValue(t, resource, name)
	if value["unknown"] != true {
		t.Fatalf("%s view must be unknown, got %v", name, value)
	}
	if text, _ := value["reason"].(string); !strings.Contains(text, reason) {
		t.Fatalf("%s unknown reason %q lacks %q", name, text, reason)
	}
}

// TestShowReportsMemoryDriftWithViewsAndSources pins idiom 2's drift flag:
// declared 20480 MiB beside enforced 48 GiB must show drift=true, every
// view named with its source, and the guest views read from the state
// document. Mutation checked: deleting the drift field or reporting only
// the declared value leaves the assertion on enforced/drift failing.
func TestShowReportsMemoryDriftWithViewsAndSources(t *testing.T) {
	summary := driftedShowSummary(t)
	fixture := showFixture{
		summary:     summary,
		state:       runningShowState(4242),
		volumeState: map[string]interface{}{"usage": map[string]interface{}{"used": int64(234083191193), "total": int64(234083191193 + 1)}},
		poolSpace:   map[string]interface{}{"space": map[string]interface{}{"used": int64(293131517952), "total": int64(966367641600)}},
		poolConfig:  map[string]interface{}{"config": map[string]interface{}{"size": "900GiB", "source": "/var/lib/incus/disks/chasm-test.img"}},
		hostCap:     func() (int64, int64, int64, error) { return 62896, 63920, 1024, nil },
		meminfo:     map[string]interface{}{"MemTotal": int64(63920 * 1024), "MemAvailable": int64(20000 * 1024)},
		resident:    map[int64]int64{4242: 24884},
		footprint:   func(string) (int64, int64, int64, error) { return 921600, 272384, 20000, nil },
	}
	views := showViews(t, fixture)
	memory, ok := views["memory"].(map[string]interface{})
	if !ok {
		t.Fatal("the resources object carries no memory view set")
	}
	if memory["drift"] != true {
		t.Fatalf("declared 20480 MiB beside enforced 48GiB must be drift: %v", memory)
	}
	if got := viewMib(t, memory, "declared"); got != 20480 {
		t.Fatalf("declared = %d, want 20480", got)
	}
	if got := viewMib(t, memory, "enforced"); got != 49152 {
		t.Fatalf("enforced = %d, want 49152", got)
	}
	if got := viewMib(t, memory, "guest_total"); got != 48128 {
		t.Fatalf("guest_total = %d, want 48128", got)
	}
	if got := viewMib(t, memory, "guest_used"); got != 3104 {
		t.Fatalf("guest_used = %d, want 3104", got)
	}
	if got := viewMib(t, memory, "host_resident"); got != 24884 {
		t.Fatalf("host_resident = %d, want 24884", got)
	}
	if got := viewMib(t, memory, "host_cap"); got != 62896 {
		t.Fatalf("host_cap = %d, want 62896", got)
	}
	cpu, ok := views["cpu"].(map[string]interface{})
	if !ok || cpu["drift"] != false {
		t.Fatalf("cpu views missing or wrongly flagged as drift: %v", views["cpu"])
	}
}

// TestShowWithoutDriftLeavesTheDriftFlagOff pins the no-drift fixture:
// budget key 49152 with limits.memory 48GiB is not drift. Mutation checked:
// a drift flag computed from only one side (declaredPresent ignored) turns
// this test red because a missing key would be reported as drift.
func TestShowWithoutDriftLeavesTheDriftFlagOff(t *testing.T) {
	summary := driftedShowSummary(t)
	site := siteconfig.Active()
	summary.Config[site.MetadataKey("budget-memory-mib")] = "49152"
	fixture := showFixture{
		summary:     summary,
		state:       runningShowState(4242),
		volumeState: map[string]interface{}{"usage": map[string]interface{}{"used": int64(1024)}},
		poolSpace:   map[string]interface{}{"space": map[string]interface{}{"used": int64(1024), "total": int64(1024)}},
		poolConfig:  map[string]interface{}{"config": map[string]interface{}{}},
		hostCap:     func() (int64, int64, int64, error) { return 62896, 63920, 1024, nil },
		meminfo:     map[string]interface{}{"MemTotal": int64(63920 * 1024), "MemAvailable": int64(60000 * 1024)},
		resident:    map[int64]int64{4242: 1024},
	}
	views := showViews(t, fixture)
	memory := views["memory"].(map[string]interface{})
	if memory["drift"] != false {
		t.Fatalf("declared 49152 beside enforced 48GiB is not drift: %v", memory)
	}
}

// TestShowMarksUnobservableGuestValuesUnknown pins the absent-guest-agent
// fixture: no memory object in the state document yields explicit unknowns
// with reasons, never zeros or omissions. Mutation checked: defaulting the
// guest views to zero leaves the unknown-reason assertion failing.
func TestShowMarksUnobservableGuestValuesUnknown(t *testing.T) {
	summary := driftedShowSummary(t)
	state := map[string]interface{}{"status": "Running", "pid": int64(4242)}
	fixture := showFixture{
		summary:     summary,
		state:       state,
		volumeState: map[string]interface{}{"usage": map[string]interface{}{"used": int64(1024)}},
		poolSpace:   map[string]interface{}{"space": map[string]interface{}{"used": int64(1024)}},
		poolConfig:  map[string]interface{}{"config": map[string]interface{}{}},
		hostCap:     func() (int64, int64, int64, error) { return 62896, 63920, 1024, nil },
		meminfo:     map[string]interface{}{"MemTotal": int64(63920 * 1024), "MemAvailable": int64(60000 * 1024)},
		resident:    map[int64]int64{4242: 1024},
	}
	views, err := sandboxShowViewsWith("sandbox-development", "Running", 4242, state, fixture.seams())
	if err != nil {
		t.Fatal(err)
	}
	memory := views["memory"].(map[string]interface{})
	viewUnknown(t, memory, "guest_total", "the guest agent did not report it")
	viewUnknown(t, memory, "guest_used", "the guest agent did not report it")
	rootDisk := views["root_disk"].(map[string]interface{})
	viewUnknown(t, rootDisk, "pool_used", "the storage driver did not report it")
}

// TestShowMarksStoppedHostResidentUnknown pins the stopped-instance
// fixture: no host_resident for a stopped sandbox, with the reason named.
// Mutation checked: reading /proc for pid 0 (or omitting the view) fails
// the unknown-reason assertion.
func TestShowMarksStoppedHostResidentUnknown(t *testing.T) {
	summary := driftedShowSummary(t)
	summary.Status = "Stopped"
	summary.Config[siteconfig.Active().MetadataKey("budget-memory-mib")] = "49152"
	state := map[string]interface{}{"status": "Stopped", "pid": int64(0)}
	fixture := showFixture{
		summary:     summary,
		state:       state,
		volumeState: map[string]interface{}{"usage": map[string]interface{}{"used": int64(1024)}},
		poolSpace:   map[string]interface{}{"space": map[string]interface{}{"used": int64(1024)}},
		poolConfig:  map[string]interface{}{"config": map[string]interface{}{}},
		hostCap:     func() (int64, int64, int64, error) { return 62896, 63920, 1024, nil },
		meminfo:     map[string]interface{}{"MemTotal": int64(63920 * 1024), "MemAvailable": int64(60000 * 1024)},
		resident:    map[int64]int64{},
	}
	views, err := sandboxShowViewsWith("sandbox-development", "Stopped", 0, state, fixture.seams())
	if err != nil {
		t.Fatal(err)
	}
	memory := views["memory"].(map[string]interface{})
	viewUnknown(t, memory, "host_resident", "the sandbox is not running")
}

// TestShowNamesProfileInheritedAndOverriddenRootSizes pins the root-disk
// source distinction. Mutation checked: dropping the fromProfile branch
// makes the profile fixture's source assertion fail.
func TestShowNamesProfileInheritedAndOverriddenRootSizes(t *testing.T) {
	site := siteconfig.Active()
	state := map[string]interface{}{"status": "Stopped", "pid": int64(0)}

	profileInherited := driftedShowSummary(t)
	delete(profileInherited.Devices, "root")
	profileInherited.Devices["shared"] = map[string]string{"type": "disk", "pool": site.StoragePool, "source": site.SharedVolumeName(), "path": sharedPath, "readonly": "false"}
	profileInherited.Devices["workspace"] = map[string]string{"type": "disk", "pool": site.StoragePool, "source": site.WorkspaceVolumeName("sandbox-development"), "path": workspacePath, "readonly": "false"}
	profileFixture := showFixture{
		summary:     profileInherited,
		state:       state,
		volumeState: map[string]interface{}{"usage": map[string]interface{}{"used": int64(1024)}},
		poolSpace:   map[string]interface{}{"space": map[string]interface{}{"used": int64(1024)}},
		poolConfig:  map[string]interface{}{"config": map[string]interface{}{}},
		hostCap:     func() (int64, int64, int64, error) { return 62896, 63920, 1024, nil },
		meminfo:     map[string]interface{}{"MemTotal": int64(63920 * 1024), "MemAvailable": int64(60000 * 1024)},
	}
	views, err := sandboxShowViewsWith("sandbox-development", "Stopped", 0, state, profileFixture.seams())
	if err != nil {
		t.Fatal(err)
	}
	source, _ := viewValue(t, views["root_disk"].(map[string]interface{}), "size")["source"].(string)
	if !strings.Contains(source, "profile") {
		t.Fatalf("a root size without an instance device must name the profile: %q", source)
	}

	overridden := driftedShowSummary(t)
	overridden.Config[siteconfig.Active().MetadataKey("budget-memory-mib")] = "49152"
	overrideFixture := profileFixture
	overrideFixture.summary = overridden
	views, err = sandboxShowViewsWith("sandbox-development", "Stopped", 0, state, overrideFixture.seams())
	if err != nil {
		t.Fatal(err)
	}
	source, _ = viewValue(t, views["root_disk"].(map[string]interface{}), "size")["source"].(string)
	if !strings.Contains(source, "override") {
		t.Fatalf("a root size set on the instance must name the override: %q", source)
	}
}

// TestShowWorkspaceWithoutQuotaNamesThePool pins the quota-less workspace
// fixture: the quota is explicitly "none" with the reason that the volume
// shares the pool. Mutation checked: reporting the pool's size as the
// quota leaves the none-marker assertion failing.
func TestShowWorkspaceWithoutQuotaNamesThePool(t *testing.T) {
	summary := driftedShowSummary(t)
	summary.Config[siteconfig.Active().MetadataKey("budget-memory-mib")] = "49152"
	fixture := showFixture{
		summary:     summary,
		state:       runningShowState(4242),
		volumeState: map[string]interface{}{"usage": map[string]interface{}{"used": int64(234083191193)}},
		poolSpace:   map[string]interface{}{"space": map[string]interface{}{"used": int64(293131517952), "total": int64(966367641600)}},
		poolConfig:  map[string]interface{}{"config": map[string]interface{}{"size": "900GiB"}},
		hostCap:     func() (int64, int64, int64, error) { return 62896, 63920, 1024, nil },
		meminfo:     map[string]interface{}{"MemTotal": int64(63920 * 1024), "MemAvailable": int64(60000 * 1024)},
		resident:    map[int64]int64{4242: 1024},
	}
	views := showViews(t, fixture)
	workspace, ok := views["workspace"].(map[string]interface{})
	if !ok {
		t.Fatal("the resources object carries no workspace view set")
	}
	if got := viewMib(t, workspace, "used"); got != 223240 {
		t.Fatalf("workspace used = %d, want 223240", got)
	}
	quota := viewValue(t, workspace, "quota")
	if quota["none"] != true {
		t.Fatalf("a quota-less volume must say so explicitly: %v", quota)
	}
	reason, _ := quota["reason"].(string)
	if !strings.Contains(reason, "shares the pool") {
		t.Fatalf("quota reason %q must explain the pool sharing", reason)
	}
	pool := workspace["pool"].(map[string]interface{})
	if name, _ := pool["name"].(string); name != siteconfig.Active().StoragePool {
		t.Fatalf("workspace pool name = %q", name)
	}
}

// TestShowLoopBackedPoolNumbersAndFillWarning pins the loop pool fixture:
// the pool view carries size, used, and the loop file's allocation, and
// the fill warning fires exactly when configured size exceeds allocated
// plus backing free. Mutation checked: removing the comparison (or its
// warning) leaves the on-case failing; an inverted comparison leaves the
// off-case failing.
func TestShowLoopBackedPoolNumbersAndFillWarning(t *testing.T) {
	base := func(freeMib int64) showFixture {
		summary := driftedShowSummary(t)
		summary.Config[siteconfig.Active().MetadataKey("budget-memory-mib")] = "49152"
		summary.Status = "Stopped"
		return showFixture{
			summary:     summary,
			state:       map[string]interface{}{"status": "Stopped", "pid": int64(0)},
			volumeState: map[string]interface{}{"usage": map[string]interface{}{"used": int64(1024)}},
			poolSpace:   map[string]interface{}{"space": map[string]interface{}{"used": int64(293131517952), "total": int64(966367641600)}},
			poolConfig:  map[string]interface{}{"config": map[string]interface{}{"size": "900GiB", "source": "/var/lib/incus/disks/chasm-test.img"}},
			hostCap:     func() (int64, int64, int64, error) { return 62896, 63920, 1024, nil },
			meminfo:     map[string]interface{}{"MemTotal": int64(63920 * 1024), "MemAvailable": int64(60000 * 1024)},
			resident:    map[int64]int64{},
			footprint:   func(string) (int64, int64, int64, error) { return 921600, 272384, freeMib, nil },
		}
	}
	// 900 GiB configured against 266 GiB allocated: fillable only with
	// enough free space behind the loop file.
	fillable := base(700000)
	views := showViews(t, fillable)
	pool := views["pool"].(map[string]interface{})
	if got := viewMib(t, pool, "size"); got != 921600 {
		t.Fatalf("pool size = %d, want 921600", got)
	}
	if got := viewMib(t, pool, "used"); got != 279552 {
		t.Fatalf("pool used = %d, want 279552", got)
	}
	loop, ok := pool["loop"].(map[string]interface{})
	if !ok || loop["unknown"] == true {
		t.Fatalf("a loop-backed pool must report its loop file: %v", pool["loop"])
	}
	if allocated, _ := numericInt64(loop["allocated_mib"]); allocated != 272384 {
		t.Fatalf("loop allocated = %d, want 272384", allocated)
	}
	if warnings, _ := pool["warnings"].([]string); len(warnings) != 0 {
		t.Fatalf("a fillable pool must not warn: %v", warnings)
	}

	unfillable := base(20000)
	views = showViews(t, unfillable)
	pool = views["pool"].(map[string]interface{})
	warnings, _ := pool["warnings"].([]string)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "the pool could not be filled") {
		t.Fatalf("an unfillable pool must warn exactly once: %v", warnings)
	}
	// The warning names the real remedy: free space on the filesystem
	// holding the loop file (path and MiB still needed), never growing
	// the pool — growing widens the shortfall and provision preflight
	// refuses it in this state. Mutation checked: restoring "grow the
	// pool through the site descriptor" fails the growth-advice
	// assertion; dropping the needed-MiB figure or the loop file path
	// fails the corresponding Contains.
	remedy := warnings[0]
	if strings.Contains(remedy, "grow the pool") || strings.Contains(remedy, "rerun provision") {
		t.Fatalf("the fill warning must not recommend growing the pool: %q", remedy)
	}
	if !strings.Contains(remedy, "272384 MiB allocated plus 20000 MiB free") || !strings.Contains(remedy, "629216") {
		t.Fatalf("the fill warning must state the real allocated and free figures and the MiB still needed (921600 - 272384 - 20000 = 629216): %q", remedy)
	}
	if !strings.Contains(remedy, "/var/lib/incus/disks/chasm-test.img") {
		t.Fatalf("the fill warning must name the loop file path: %q", remedy)
	}
}

// TestHostReserveWarningOnAndOff pins idiom 4's host warning: the host's
// observed own usage (MemTotal - MemAvailable - sandbox resident sum)
// above host_reserve_mib warns exactly once, and below it stays silent.
// Mutation checked: removing the warning branch fails the on-case; warning
// unconditionally fails the off-case.
func TestHostReserveWarningOnAndOff(t *testing.T) {
	fixture := func(memAvailableMib int64) showFixture {
		summary := driftedShowSummary(t)
		return showFixture{
			summary:     summary,
			state:       runningShowState(4242),
			volumeState: map[string]interface{}{"usage": map[string]interface{}{"used": int64(1024)}},
			poolSpace:   map[string]interface{}{"space": map[string]interface{}{"used": int64(1024)}},
			poolConfig:  map[string]interface{}{"config": map[string]interface{}{}},
			hostCap:     func() (int64, int64, int64, error) { return 62896, 63920, 1024, nil },
			meminfo:     map[string]interface{}{"MemTotal": int64(63920 * 1024), "MemAvailable": int64(memAvailableMib * 1024)},
			resident:    map[int64]int64{4242: 24884},
		}
	}
	// 63920 - 20000 - 24884 = 19036 MiB of observed own use, far above the
	// fixture site's 1024 MiB reserve. The sandbox stays genuinely drifted
	// (declared 20480 MiB, enforced 48GiB, with no override key rewriting
	// the record), so admitted_mib can only reach 49152 through the
	// larger-of rule: counting only the declared key would report 20480
	// and fail.
	on := showViews(t, fixture(20000))
	host := on["host"].(map[string]interface{})
	if host["observed_own_mib"] != int64(19036) {
		t.Fatalf("observed own use = %v, want 19036", host["observed_own_mib"])
	}
	if host["admitted_mib"] != int64(49152) {
		t.Fatalf("admitted = %v, want the enforced 49152, not the declared 20480", host["admitted_mib"])
	}
	warnings, _ := host["warnings"].([]string)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "raise host_reserve_mib in site.conf and rerun provision") {
		t.Fatalf("an over-reserved host must warn exactly once: %v", warnings)
	}

	// Off-case: 63920 - 38536 - 24884 = 500 MiB of observed own use —
	// strictly between 0 and the 1024 MiB reserve, so the warning must
	// stay silent here.
	off := showViews(t, fixture(38536))
	host = off["host"].(map[string]interface{})
	if host["observed_own_mib"] != int64(500) {
		t.Fatalf("observed own use = %v, want 500", host["observed_own_mib"])
	}
	warnings, _ = host["warnings"].([]string)
	if len(warnings) != 0 {
		t.Fatalf("a host within its reserve must not warn: %v", warnings)
	}
}

// TestShowReportsUnsupportedEnforcedValueAsUnknownView pins the degradation
// contract: an enforced limits.memory chasm cannot parse becomes an
// unknown view with the reason, the drift becomes "unverifiable", and the
// Resources section still carries every other view. Mutation checked:
// failing the collector on the unsupported value (the old behavior) makes
// this test fail because the memory views disappear entirely.
func TestShowReportsUnsupportedEnforcedValueAsUnknownView(t *testing.T) {
	summary := driftedShowSummary(t)
	summary.ExpandedConfig = map[string]string{"limits.memory": "50%", "limits.cpu": "14"}
	fixture := showFixture{
		summary:     summary,
		state:       runningShowState(4242),
		volumeState: map[string]interface{}{"usage": map[string]interface{}{"used": int64(1024)}},
		poolSpace:   map[string]interface{}{"space": map[string]interface{}{"used": int64(1024)}},
		poolConfig:  map[string]interface{}{"config": map[string]interface{}{}},
		hostCap:     func() (int64, int64, int64, error) { return 62896, 63920, 1024, nil },
		meminfo:     map[string]interface{}{"MemTotal": int64(63920 * 1024), "MemAvailable": int64(60000 * 1024)},
		resident:    map[int64]int64{4242: 1024},
	}
	views, err := sandboxShowViewsWith("sandbox-development", "Running", 4242, fixture.state, fixture.seams())
	if err != nil {
		t.Fatalf("an unsupported enforced value must not abort the collector: %v", err)
	}
	memory := views["memory"].(map[string]interface{})
	viewUnknown(t, memory, "enforced", "limits.memory '50%' is not a supported size")
	if memory["drift"] != "unverifiable" {
		t.Fatalf("drift must be unverifiable beside an unsupported enforced value: %v", memory["drift"])
	}
	// The other views still report: the section is never dropped.
	if got := viewMib(t, memory, "declared"); got != 20480 {
		t.Fatalf("declared = %d, want 20480", got)
	}
	if got := viewMib(t, memory, "host_resident"); got != 1024 {
		t.Fatalf("host_resident = %d, want 1024", got)
	}
}

// TestShowReportsAFrozenSandboxHostResident pins the Frozen fix: a frozen
// sandbox has a process, so host_resident reports its VmRSS instead of
// "not running", keeping the memory row and the host view in agreement.
// Mutation checked: reverting the condition to status == "Running" makes
// the known-value assertion fail.
func TestShowReportsAFrozenSandboxHostResident(t *testing.T) {
	summary := driftedShowSummary(t)
	summary.Status = "Frozen"
	fixture := showFixture{
		summary:     summary,
		state:       map[string]interface{}{"status": "Frozen", "pid": int64(4242)},
		volumeState: map[string]interface{}{"usage": map[string]interface{}{"used": int64(1024)}},
		poolSpace:   map[string]interface{}{"space": map[string]interface{}{"used": int64(1024)}},
		poolConfig:  map[string]interface{}{"config": map[string]interface{}{}},
		hostCap:     func() (int64, int64, int64, error) { return 62896, 63920, 1024, nil },
		meminfo:     map[string]interface{}{"MemTotal": int64(63920 * 1024), "MemAvailable": int64(60000 * 1024)},
		resident:    map[int64]int64{4242: 24884},
	}
	views, err := sandboxShowViewsWith("sandbox-development", "Frozen", 4242, fixture.state, fixture.seams())
	if err != nil {
		t.Fatal(err)
	}
	memory := views["memory"].(map[string]interface{})
	if got := viewMib(t, memory, "host_resident"); got != 24884 {
		t.Fatalf("a frozen sandbox's resident = %d, want 24884", got)
	}
}

// TestHostViewNotesAPeerItCannotVerify pins the host-view degradation: a
// running peer whose enforced limits.memory chasm cannot parse is counted
// and noted in a warning, and the host view still renders. Mutation
// checked: returning an error for that peer (the old behavior) makes the
// host view disappear and this test fail.
func TestHostViewNotesAPeerItCannotVerify(t *testing.T) {
	summary := driftedShowSummary(t)
	summary.Config[siteconfig.Active().MetadataKey("budget-memory-mib")] = "49152"
	peer := managedSummaryForSite("sandbox-drifted", "Running")
	peer.ExpandedConfig = map[string]string{"limits.memory": "50%"}
	fixture := showFixture{
		summary:     summary,
		state:       runningShowState(4242),
		volumeState: map[string]interface{}{"usage": map[string]interface{}{"used": int64(1024)}},
		poolSpace:   map[string]interface{}{"space": map[string]interface{}{"used": int64(1024)}},
		poolConfig:  map[string]interface{}{"config": map[string]interface{}{}},
		hostCap:     func() (int64, int64, int64, error) { return 62896, 63920, 1024, nil },
		meminfo:     map[string]interface{}{"MemTotal": int64(63920 * 1024), "MemAvailable": int64(60000 * 1024)},
		resident:    map[int64]int64{4242: 1024},
	}
	seams := fixture.seams()
	seams.listSummaries = func() ([]incusInstanceSummary, error) {
		return []incusInstanceSummary{summary, peer}, nil
	}
	seams.readState = func(name string) (map[string]interface{}, error) {
		if name == "sandbox-drifted" {
			return map[string]interface{}{"status": "Running", "pid": int64(500)}, nil
		}
		return fixture.state, nil
	}
	views, err := sandboxShowViewsWith("sandbox-development", "Running", 4242, fixture.state, seams)
	if err != nil {
		t.Fatalf("a peer the host view cannot verify must not abort the view: %v", err)
	}
	host := views["host"].(map[string]interface{})
	if host["unresolved_peers"] != 1 {
		t.Fatalf("the unverified peer must be counted: %v", host["unresolved_peers"])
	}
	warnings, _ := host["warnings"].([]string)
	found := false
	for _, warning := range warnings {
		if strings.Contains(warning, "sandbox-drifted") && strings.Contains(warning, "could not verify") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the unverified peer must be noted: %v", warnings)
	}
}

// TestAttachResourcesKeepsTheSectionOnCollectorFailure pins the show-side
// guarantee that a whole-collector failure still produces a Resources
// object carrying the reason, never a silent drop. Mutation checked:
// dropping the resources key on error (the old best-effort attachment)
// fails the present-and-reason assertion.
func TestAttachResourcesKeepsTheSectionOnCollectorFailure(t *testing.T) {
	result := map[string]interface{}{}
	attachResources(result, func() (map[string]interface{}, error) {
		return nil, protoError("unsafe_state", "the storage pool is unreachable")
	})
	resources, ok := result["resources"].(map[string]interface{})
	if !ok {
		t.Fatal("a failed collector must still attach the resources object")
	}
	if reason, _ := resources["unavailable"].(string); !strings.Contains(reason, "the storage pool is unreachable") {
		t.Fatalf("the resources object must carry the reason: %v", resources)
	}
	result = map[string]interface{}{}
	attachResources(result, func() (map[string]interface{}, error) {
		return map[string]interface{}{"memory": map[string]interface{}{}}, nil
	})
	if _, ok := result["resources"].(map[string]interface{})["memory"]; !ok {
		t.Fatalf("a successful collector must attach its views: %v", result)
	}
}

// TestGoldenShowResourcesFixtureMatchesProducer pins the shared golden
// fixture: it is a JSON round-trip of sandboxShowViewsWith output for the
// canonical drift scenario, so producer-side drift fails here and
// renderer-side drift fails the chasmctl test that loads the same file.
// Regenerate deliberately (update the committed file in the same commit)
// when the view shape changes.
func TestGoldenShowResourcesFixtureMatchesProducer(t *testing.T) {
	views := goldenShowViews(t)
	data, err := json.MarshalIndent(views, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(moduleRoot(), "testdata", "golden", "sandbox-show-resources.json")
	committed, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the committed golden fixture is missing (%v); regenerate testdata/golden/sandbox-show-resources.json", err)
	}
	if string(committed) != string(data)+"\n" {
		t.Fatal("the committed golden fixture drifted from sandboxShowViewsWith output; regenerate testdata/golden/sandbox-show-resources.json")
	}
}

// goldenShowViews builds the canonical drift scenario through the real
// collector: budget key 20480 beside enforced 48GiB, a 150 GiB root
// override, guest-agent figures, a quota-less workspace, a 900 GiB
// loop-backed pool with a fill warning, and a host above its reserve.
// Every host-derived value is injected (MemTotal, MemAvailable, reserve,
// the statfs footprint, the process VmRSS, and the host CPU count), so
// the committed fixture never depends on the machine that runs the test.
func goldenShowViews(t *testing.T) map[string]interface{} {
	t.Helper()
	return goldenShowViewsWithHostCpus(t, 20)
}

// goldenShowViewsWithHostCpus builds the golden scenario with the host CPU
// count pinned to the given value, restoring the real seam on cleanup.
func goldenShowViewsWithHostCpus(t *testing.T, cpus int64) map[string]interface{} {
	t.Helper()
	original := hostCpuCount
	t.Cleanup(func() { hostCpuCount = original })
	hostCpuCount = func() int64 { return cpus }
	summary := driftedShowSummary(t)
	fixture := showFixture{
		summary:     summary,
		state:       runningShowState(4242),
		volumeState: map[string]interface{}{"usage": map[string]interface{}{"used": int64(234083191193), "total": int64(234083191193 + 1)}},
		poolSpace:   map[string]interface{}{"space": map[string]interface{}{"used": int64(293131517952), "total": int64(966367641600)}},
		poolConfig:  map[string]interface{}{"config": map[string]interface{}{"size": "900GiB", "source": "/var/lib/incus/disks/chasm-test.img"}},
		hostCap:     func() (int64, int64, int64, error) { return 62896, 63920, 1024, nil },
		meminfo:     map[string]interface{}{"MemTotal": int64(63920 * 1024), "MemAvailable": int64(20000 * 1024)},
		resident:    map[int64]int64{4242: 24884},
		footprint:   func(string) (int64, int64, int64, error) { return 921600, 272384, 20000, nil },
	}
	views, err := sandboxShowViewsWith("sandbox-development", "Running", 4242, fixture.state, fixture.seams())
	if err != nil {
		t.Fatal(err)
	}
	return views
}

// TestGoldenShowViewsHostIndependent guards the fixture's host
// independence: generation with two different real-looking host CPU
// counts injected must be identical everywhere except the injected
// cpu.host_count itself — nothing else may leak from the machine that
// runs the test. Mutation checked: reading the host CPU count from the
// machine instead of the injection fails the per-injection count check;
// the comment scope is exactly what the test proves: no field besides
// cpu.host_count depends on the injected CPU count, and any other leaked
// host value fails the byte comparison.
func TestGoldenShowViewsHostIndependent(t *testing.T) {
	marshal := func(cpus int64) (string, int64) {
		views := goldenShowViewsWithHostCpus(t, cpus)
		cpu, _ := views["cpu"].(map[string]interface{})
		count, _ := cpu["host_count"].(map[string]interface{})
		got, _ := count["count"].(int64)
		if got != cpus {
			t.Fatalf("cpu.host_count = %v, want the injected %d", count["count"], cpus)
		}
		delete(cpu, "host_count")
		data, err := json.MarshalIndent(views, "", " ")
		if err != nil {
			t.Fatal(err)
		}
		return string(data), got
	}
	withoutHostCountA, countA := marshal(20)
	withoutHostCountB, countB := marshal(64)
	if countA == countB {
		t.Fatal("the two injections must differ, or this guard proves nothing")
	}
	if withoutHostCountA != withoutHostCountB {
		t.Fatal("the golden generation leaked a host-derived value: the two runs differ beyond the injected host CPU count")
	}
}

// TestShowNamesThePoolUsageSource pins the root disk's host-cost
// attribution: the pool_used view names the storage pool's usage of the
// root volume (the storage driver), never the guest agent. Mutation
// checked: reverting the source string to "as reported by the guest agent"
// fails this test.
func TestShowNamesThePoolUsageSource(t *testing.T) {
	summary := driftedShowSummary(t)
	summary.Config[siteconfig.Active().MetadataKey("budget-memory-mib")] = "49152"
	fixture := showFixture{
		summary:     summary,
		state:       runningShowState(4242),
		volumeState: map[string]interface{}{"usage": map[string]interface{}{"used": int64(1024)}},
		poolSpace:   map[string]interface{}{"space": map[string]interface{}{"used": int64(1024)}},
		poolConfig:  map[string]interface{}{"config": map[string]interface{}{}},
		hostCap:     func() (int64, int64, int64, error) { return 62896, 63920, 1024, nil },
		meminfo:     map[string]interface{}{"MemTotal": int64(63920 * 1024), "MemAvailable": int64(60000 * 1024)},
		resident:    map[int64]int64{4242: 1024},
	}
	views := showViews(t, fixture)
	source, _ := viewValue(t, views["root_disk"].(map[string]interface{}), "pool_used")["source"].(string)
	if !strings.Contains(source, "the storage pool's usage of the root volume") {
		t.Fatalf("pool_used must name the pool-side source: %q", source)
	}
	if strings.Contains(source, "guest agent") {
		t.Fatalf("pool_used must not be attributed to the guest agent: %q", source)
	}
	origin, _ := views["root_disk"].(map[string]interface{})["origin"].(string)
	if origin != "override" {
		t.Fatalf("an instance-level root device must carry origin override, got %q", origin)
	}
}
