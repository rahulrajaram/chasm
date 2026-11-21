package execserver

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/rahulrajaram/chasm/internal/protocol"
	"github.com/rahulrajaram/chasm/internal/siteconfig"
)

// Idiom 2: `chasm show` reports each resource from every angle that can
// disagree — declared (the record), enforced (Incus), guest (the guest
// agent), and host cost — names the source of every value, and prints an
// explicit unknown with a reason for anything it cannot observe. A declared
// value that differs from the enforced one is reported as drift together
// with the command that resolves it.

// showSeams is the injected observation surface of the resource collector.
// Every Incus read goes through one call per view so tests can serve JSON
// fixtures and the production wiring stays bounded (at most one /state call
// per instance and one pool call per show).
type showSeams struct {
	listSummaries func() ([]incusInstanceSummary, error)
	readState     func(instance string) (map[string]interface{}, error)
	volumeState   func(pool, volume string) (map[string]interface{}, error)
	poolResources func(pool string) (map[string]interface{}, error)
	poolConfig    func(pool string) (map[string]interface{}, error)
	hostMemory    func() (int64, int64, int64, error)
	meminfo       func() (map[string]interface{}, error)
	residentMib   func(pid int64) (int64, error)
	pathFootprint func(path string) (apparentMib int64, allocatedMib int64, freeMib int64, err error)
}

func realShowSeams() showSeams {
	return showSeams{
		listSummaries: listInstanceStatuses,
		readState:     instanceStateFor,
		volumeState:   incusVolumeState,
		poolResources: incusPoolResources,
		poolConfig:    incusPoolConfig,
		hostMemory:    hostMemoryCapMiB,
		meminfo:       hostMeminfoKiB,
		residentMib:   processResidentMib,
		pathFootprint: loopFileFootprintMib,
	}
}

// bytesToMibCeil converts a byte count to whole MiB, rounding up, matching
// the arithmetic used everywhere else a byte limit becomes a MiB budget.
func bytesToMibCeil(bytes int64) int64 {
	return (bytes + (1 << 20) - 1) >> 20
}

// mibView is an observable value in MiB together with its source.
func mibView(mib int64, source string) map[string]interface{} {
	return map[string]interface{}{"mib": mib, "source": source}
}

// countView is an observable CPU count together with its source.
func countView(count int64, source string) map[string]interface{} {
	return map[string]interface{}{"count": count, "source": source}
}

// unknownView is an explicitly unobservable value: the reason is required,
// so the operator is never left guessing (idiom 2).
func unknownView(reason string) map[string]interface{} {
	return map[string]interface{}{"unknown": true, "reason": reason}
}

// processResidentMib reads the VmRSS of one instance's QEMU process — the
// host cost of a running sandbox (idiom 2's host view).
func processResidentMib(pid int64) (int64, error) {
	status, err := numericKibRecords(fmt.Sprintf("/proc/%d/status", pid), statusFields)
	if err != nil {
		return 0, err
	}
	rssKiB, ok := numericInt64(status["VmRSS"])
	if !ok || rssKiB < 0 {
		return 0, fmt.Errorf("VmRSS is unavailable for pid %d", pid)
	}
	return rssKiB / 1024, nil
}

// hostMeminfoKiB reads the host's memory counters from /proc/meminfo.
func hostMeminfoKiB() (map[string]interface{}, error) {
	return numericKibRecords("/proc/meminfo", meminfoFields)
}

// incusVolumeState reads one custom volume's usage from Incus.
func incusVolumeState(pool, volume string) (map[string]interface{}, error) {
	return incusQueryGET("/1.0/storage-pools/" + pool + "/volumes/custom/" + volume + "/state")
}

// incusPoolResources reads a storage pool's space usage from Incus.
func incusPoolResources(pool string) (map[string]interface{}, error) {
	return incusQueryGET("/1.0/storage-pools/" + pool + "/resources")
}

// incusPoolConfig reads a storage pool's configuration from Incus.
func incusPoolConfig(pool string) (map[string]interface{}, error) {
	return incusQueryGET("/1.0/storage-pools/" + pool)
}

type syscallStat_t = syscall.Stat_t

// loopFileFootprintMib reports a loop file's apparent size, its actually
// allocated blocks, and the free space of the filesystem that holds it —
// the three numbers that decide whether a loop-backed pool could ever be
// filled to its configured size (idiom 4).
func loopFileFootprintMib(path string) (int64, int64, int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, 0, err
	}
	stat_t, ok := info.Sys().(*syscallStat_t)
	if !ok {
		return 0, 0, 0, fmt.Errorf("%s allocation is unavailable", path)
	}
	var stat syscallStatfs
	if err := syscallStatfsFn(filepath.Dir(path), &stat); err != nil {
		return 0, 0, 0, err
	}
	apparent := bytesToMibCeil(info.Size())
	allocated := bytesToMibCeil(stat_t.Blocks * 512)
	freeMib := int64(stat.Bavail) * int64(stat.Frsize) >> 20
	return apparent, allocated, freeMib, nil
}

// sandboxShowViews collects the full resource view set of one sandbox. The
// instance's already-fetched state is passed in so the instance costs one
// /state call per show; the inventory listing is shared with drift sums and
// the host view.
func sandboxShowViews(instance string, status string, pid int64, state map[string]interface{}) (map[string]interface{}, error) {
	seams := realShowSeams()
	return sandboxShowViewsWith(instance, status, pid, state, seams)
}

func sandboxShowViewsWith(instance string, status string, pid int64, state map[string]interface{}, seams showSeams) (map[string]interface{}, error) {
	summaries, err := seams.listSummaries()
	if err != nil {
		return nil, err
	}
	var summary *incusInstanceSummary
	for index := range summaries {
		if summaries[index].Name == instance {
			summary = &summaries[index]
			break
		}
	}
	if summary == nil {
		return nil, protoError("unsafe_state", "managed sandbox disappeared from the instance inventory")
	}
	// Declared and enforced reuse the admission budgets so drift cannot
	// disagree with what admission counts (idiom 1). An enforced value
	// chasm cannot parse becomes an explicit unknown inside its view —
	// the section is never dropped, and the drift is reported as
	// unverifiable instead of hidden. A corrupted declared record (a
	// budget key that is not a valid number) fails the collector, which
	// the caller turns into an explicit "unavailable" resources object.
	memory, memoryErr := sandboxMemoryBudget(*summary)
	if memoryErr != nil {
		return nil, memoryErr
	}
	cpu, cpuErr := sandboxCpuBudget(*summary)
	if cpuErr != nil {
		return nil, cpuErr
	}
	root, rootErr := rootDeviceView(summary)
	views := map[string]interface{}{}
	views["memory"] = memoryViews(memory, status, pid, state, seams)
	views["cpu"] = cpuViews(cpu, seams)
	views["root_disk"] = rootDiskViews(root, rootErr, state)
	workspace, pool, err := workspaceAndPoolViews(instance, seams)
	if err != nil {
		return nil, err
	}
	views["workspace"] = workspace
	views["pool"] = pool
	host, err := hostView(summaries, instance, status, pid, seams)
	if err != nil {
		return nil, err
	}
	views["host"] = host
	return views, nil
}

// memoryViews is the memory resource from every angle that can disagree.
// Declared and enforced reuse sandboxMemoryBudget so drift cannot disagree
// with what admission counts (idiom 1). An enforced value chasm cannot
// parse becomes an unknown view with that reason, and the drift becomes
// unverifiable instead of being dropped (idiom 2).
func memoryViews(budget sandboxResourceBudget, status string, pid int64, state map[string]interface{}, seams showSeams) map[string]interface{} {
	views := map[string]interface{}{}
	if budget.declaredPresent {
		views["declared"] = mibView(budget.declared, "the sandbox's budget key")
	} else {
		views["declared"] = unknownView("the budget key is not set on the instance")
	}
	if budget.enforcedPresent {
		views["enforced"] = mibView(budget.enforced, "expanded limits.memory")
	} else if budget.enforcedUnsupported != "" {
		views["enforced"] = unknownView(budget.enforcedUnsupported)
	} else {
		views["enforced"] = unknownView("the instance's expanded limits.memory is not set")
	}
	guestTotal, guestUsed := guestMemoryViews(state)
	views["guest_total"] = guestTotal
	views["guest_used"] = guestUsed
	// A Frozen sandbox still has a process; its resident memory is the
	// host cost of that process, so it is reported like a running one.
	if (status == "Running" || status == "Frozen") && pid > 0 {
		if resident, err := seams.residentMib(pid); err == nil {
			views["host_resident"] = mibView(resident, "VmRSS of the instance process")
		} else {
			views["host_resident"] = unknownView("the instance process's resident memory could not be read")
		}
	} else {
		views["host_resident"] = unknownView("the sandbox is not running")
	}
	views["drift"] = driftView(budget)
	if cap, _, reserve, err := seams.hostMemory(); err == nil {
		views["host_cap"] = mibView(cap, fmt.Sprintf("MemTotal MiB - host_reserve_mib %d MiB", reserve))
	} else {
		views["host_cap"] = unknownView("the host's memory inventory is unavailable")
	}
	return views
}

// driftView reports declared-versus-enforced drift: false, true, or the
// string "unverifiable" when an unsupported enforced value makes the
// comparison impossible (idiom 2 never guesses).
func driftView(budget sandboxResourceBudget) interface{} {
	if budget.enforcedUnsupported != "" && budget.declaredPresent {
		return "unverifiable"
	}
	return budget.drifted()
}

// guestMemoryViews reads what the guest agent reports: memory.total and
// memory.usage from the instance state. A VM without a reporting agent
// yields explicit unknowns, never zeros.
func guestMemoryViews(state map[string]interface{}) (map[string]interface{}, map[string]interface{}) {
	guestTotal := unknownView("the guest agent did not report it")
	guestUsed := unknownView("the guest agent did not report it")
	memory, ok := state["memory"].(map[string]interface{})
	if !ok {
		return guestTotal, guestUsed
	}
	if total, ok := numericInt64(memory["total"]); ok && total > 0 {
		guestTotal = mibView(bytesToMibCeil(total), "instance state memory.total as reported by the guest agent")
	}
	if used, ok := numericInt64(memory["usage"]); ok && used > 0 {
		guestUsed = mibView(bytesToMibCeil(used), "instance state memory.usage as reported by the guest agent")
	}
	return guestTotal, guestUsed
}

// cpuViews is the CPU resource: declared, enforced, and the host count.
// An unsupported enforced value is an unknown view with drift
// unverifiable, never a dropped section (idiom 2).
func cpuViews(budget sandboxResourceBudget, seams showSeams) map[string]interface{} {
	views := map[string]interface{}{}
	if budget.declaredPresent {
		views["declared"] = countView(budget.declared, "the sandbox's CPU budget key")
	} else {
		views["declared"] = unknownView("the CPU budget key is not set on the instance")
	}
	if budget.enforcedPresent {
		views["enforced"] = countView(budget.enforced, "expanded limits.cpu")
	} else if budget.enforcedUnsupported != "" {
		views["enforced"] = unknownView(budget.enforcedUnsupported)
	} else {
		views["enforced"] = unknownView("the instance's expanded limits.cpu is not set")
	}
	views["drift"] = driftView(budget)
	views["host_count"] = countView(hostCpuCount(), "the host's CPU count")
	return views
}

// rootDiskViews is the root disk: the enforced size (with the origin —
// site profile or instance override — as its own field) and the pool's
// usage of the root volume: the host cost of the disk, reported by the
// storage driver, not by the guest (idiom 2 names the true source).
func rootDiskViews(root rootDevice, rootErr error, state map[string]interface{}) map[string]interface{} {
	views := map[string]interface{}{}
	switch {
	case rootErr != nil:
		views["size"] = unknownView(rootErr.Error())
		views["origin"] = "unknown"
	case !root.present:
		views["size"] = unknownView("the sandbox has no root device")
		views["origin"] = "unknown"
	case root.fromProfile:
		views["size"] = mibView(root.sizeMib, "expanded root device size, inherited from the site profile")
		views["origin"] = "profile"
	default:
		views["size"] = mibView(root.sizeMib, "expanded root device size, an instance override")
		views["origin"] = "override"
	}
	poolUsed := unknownView("the storage driver did not report it")
	disk, ok := state["disk"].(map[string]interface{})
	if ok {
		if rootDisk, ok := disk["root"].(map[string]interface{}); ok {
			if used, ok := numericInt64(rootDisk["usage"]); ok && used > 0 {
				poolUsed = mibView(bytesToMibCeil(used), "instance state disk.root.usage (the storage pool's usage of the root volume)")
			}
		}
	}
	views["pool_used"] = poolUsed
	return views
}

// workspaceAndPoolViews reads the workspace volume and its pool. The pool
// view is returned separately because it is site-level and the host view
// reuses its warnings.
func workspaceAndPoolViews(instance string, seams showSeams) (map[string]interface{}, map[string]interface{}, error) {
	site := siteconfig.Active()
	poolName := site.StoragePool
	volume := site.WorkspaceVolumeName(instance)
	views := map[string]interface{}{"volume": volume}
	volumeState, err := seams.volumeState(poolName, volume)
	if err != nil {
		views["used"] = unknownView("the workspace volume's usage is unavailable")
		views["quota"] = unknownView("the workspace volume's state is unavailable")
	} else {
		usage, _ := volumeState["usage"].(map[string]interface{})
		used, usedOK := numericInt64(usage["used"])
		if usedOK && used >= 0 {
			views["used"] = mibView(bytesToMibCeil(used), "workspace volume state usage.used")
		} else {
			views["used"] = unknownView("the workspace volume did not report usage.used")
		}
		if total, ok := numericInt64(usage["total"]); ok && total > 0 {
			views["quota"] = mibView(bytesToMibCeil(total), "workspace volume state usage.total")
		} else {
			views["quota"] = map[string]interface{}{"none": true, "reason": "the volume has no quota and shares the pool"}
		}
	}
	pool, err := poolViews(poolName, seams)
	if err != nil {
		return nil, nil, err
	}
	views["pool"] = map[string]interface{}{"name": poolName, "size": pool["size"], "used": pool["used"]}
	return views, pool, nil
}

// poolViews is the site-level storage pool: its configured size, its used
// space, and — for a loop-backed pool — the loop file's real allocation
// against its apparent size and the free space backing it (idiom 4).
func poolViews(poolName string, seams showSeams) (map[string]interface{}, error) {
	views := map[string]interface{}{"name": poolName}
	resources, err := seams.poolResources(poolName)
	if err != nil {
		return nil, err
	}
	space, _ := resources["space"].(map[string]interface{})
	if used, ok := numericInt64(space["used"]); ok && used >= 0 {
		views["used"] = mibView(bytesToMibCeil(used), "storage pool resources space.used")
	} else {
		views["used"] = unknownView("the storage pool did not report space.used")
	}
	config, err := seams.poolConfig(poolName)
	if err != nil {
		return nil, err
	}
	poolConfigMap, _ := config["config"].(map[string]interface{})
	size := unknownView("the pool has no configured size")
	if raw, ok := poolConfigMap["size"].(string); ok && raw != "" {
		if bytes, err := parseMemoryLimitBytes(raw); err == nil {
			size = mibView(bytesToMibCeil(bytes), "storage pool config.size")
		} else {
			size = unknownView("the pool's configured size is not a supported size value")
		}
	}
	views["size"] = size
	loop := loopViews(poolConfigMap, seams)
	views["loop"] = loop
	if sizeMib, ok := poolSizeMib(size); ok {
		if loopMib, ok := loop["allocated_mib"].(int64); ok {
			freeMib, _ := loop["backing_free_mib"].(int64)
			if sizeMib > loopMib+freeMib {
				needed := sizeMib - loopMib - freeMib
				views["warnings"] = []string{fmt.Sprintf(
					"the pool's configured size %d MiB exceeds its loop file's %d MiB allocated plus %d MiB free behind it; the pool could not be filled; free at least %d MiB more on the filesystem holding %s",
					sizeMib, loopMib, freeMib, needed, loopFilePath(loop))}
			}
		}
	}
	return views, nil
}

// poolSizeMib reads the MiB value out of a pool size view.
func poolSizeMib(size map[string]interface{}) (int64, bool) {
	if size == nil {
		return 0, false
	}
	return numericInt64(size["mib"])
}

// loopFilePath reads the loop file's path out of a loop view, or a
// placeholder when the path is missing.
func loopFilePath(loop map[string]interface{}) string {
	if path, ok := loop["file"].(string); ok && path != "" {
		return path
	}
	return "(unknown loop file)"
}

// loopViews describes a loop-backed pool's real footprint on the host, or
// an explicit unknown when the pool is not loop-backed.
func loopViews(poolConfigMap map[string]interface{}, seams showSeams) map[string]interface{} {
	source, _ := poolConfigMap["source"].(string)
	if source == "" {
		return unknownView("the pool has no loop file; it is not loop-backed")
	}
	apparent, allocated, free, err := seams.pathFootprint(source)
	if err != nil {
		return unknownView("the pool's loop file could not be measured")
	}
	return map[string]interface{}{
		"file":             source,
		"apparent_mib":     apparent,
		"allocated_mib":    allocated,
		"backing_free_mib": free,
		"source":           "the loop file's size, its allocated blocks, and the free space of the filesystem that holds it",
	}
}

// hostView is idiom 4's host-side truth: the host's own memory figures,
// the sum of what running sandboxes are enforced to use, the host's
// observed own usage once sandbox resident memory is subtracted, and the
// warnings that fire when admitted budgets may no longer fit.
func hostView(summaries []incusInstanceSummary, instance string, status string, pid int64, seams showSeams) (map[string]interface{}, error) {
	meminfo, err := seams.meminfo()
	if err != nil {
		return nil, protoError("unsafe_state", "host memory inventory is unavailable")
	}
	totalKiB, totalOK := numericInt64(meminfo["MemTotal"])
	availableKiB, availableOK := numericInt64(meminfo["MemAvailable"])
	if !totalOK || totalKiB <= 0 || !availableOK || availableKiB < 0 {
		return nil, protoError("unsafe_state", "host memory inventory is invalid")
	}
	_, _, reserve, err := seams.hostMemory()
	if err != nil {
		return nil, err
	}
	memTotal := totalKiB / 1024
	memAvailable := availableKiB / 1024
	admitted := int64(0)
	residentSum := int64(0)
	unresolved := 0
	warnings := []string{}
	for index := range summaries {
		summary := &summaries[index]
		running := summary.Status == "Running" || summary.Status == "Frozen"
		if running {
			budget, err := sandboxMemoryBudget(*summary)
			if err != nil || budget.enforcedUnsupported != "" {
				// A peer whose enforced limit chasm cannot verify is
				// counted and noted, never fatal: the host view stays
				// available and names what it could not verify (idiom 2).
				unresolved++
				reason := budget.enforcedUnsupported
				if err != nil {
					reason = err.Error()
				}
				warnings = append(warnings, fmt.Sprintf("could not verify the memory of running sandbox %s: %s", summary.Name, reason))
			} else {
				// The sum follows admission's rule (idiom 1): the larger of
				// declared and enforced, falling back to the legacy default.
				admitted += budget.effective(protocol.DefaultSandboxBudgetMemoryMiB)
			}
			resPid := pid
			if summary.Name != instance {
				state, err := seams.readState(summary.Name)
				if err != nil {
					// Same policy: noted, not fatal (idiom 2).
					unresolved++
					warnings = append(warnings, fmt.Sprintf("could not verify the state of running sandbox %s", summary.Name))
					running = false
				} else {
					statusText, _ := state["status"].(string)
					if statusText != "Running" && statusText != "Frozen" {
						running = false
					} else if value, ok := numericInt64(state["pid"]); ok && value > 0 {
						resPid = value
					} else {
						running = false
					}
				}
			}
			if running && resPid > 0 {
				if resident, err := seams.residentMib(resPid); err == nil {
					residentSum += resident
				}
			}
		}
	}
	observedOwn := memTotal - memAvailable - residentSum
	if observedOwn > reserve {
		warnings = append(warnings, fmt.Sprintf(
			"the host is using %d MiB for itself but host_reserve_mib is %d; admitted budgets may not fit; raise host_reserve_mib in site.conf and rerun provision",
			observedOwn, reserve))
	}
	views := map[string]interface{}{
		"mem_total_mib":     memTotal,
		"mem_available_mib": memAvailable,
		"host_reserve_mib":  reserve,
		"admitted_mib":      admitted,
		"resident_sum_mib":  residentSum,
		"observed_own_mib":  observedOwn,
		"unresolved_peers":  unresolved,
		"warning":           len(warnings) > 0,
		"warnings":          warnings,
	}
	return views, nil
}
