package execserver

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/rahulrajaram/chasm/internal/protocol"
	"github.com/rahulrajaram/chasm/internal/siteconfig"
)

// Sandbox resize (idiom 3): the supported path for changing a stopped
// sandbox's memory, CPU, or root disk. Every target is validated against
// host-derived caps before anything is written (idiom 4), the record and the
// enforced value are written together and proven by read-back (idiom 1),
// and the change order is fixed: memory, then CPU, then root disk. A failure
// after an earlier step succeeded is reported as a partial application that
// names exactly which changes were applied and which were not.

// resizeTargets carries the requested values in MiB / CPU counts; 0 means
// "not requested" and the resource is left untouched.
type resizeTargets struct {
	memory   int64
	cpu      int64
	rootDisk int64
}

// any reports whether at least one target was requested. The protocol
// parser already refuses a resize with no target; the executor re-checks
// and fails closed rather than no-op on a malformed path.
func (targets resizeTargets) any() bool {
	return targets.memory != 0 || targets.cpu != 0 || targets.rootDisk != 0
}

// resizeSeams is the injected operation surface of the resize handler. The
// production wiring is realReseizeSeams; tests inject fakes the same way
// setSandboxDescriptionWith does for the description writer.
type resizeSeams struct {
	readState     func(instance string) (map[string]interface{}, error)
	listSummaries func() ([]incusInstanceSummary, error)
	memoryCap     func() (int64, int64, int64, error)
	poolSizeMib   func() (int64, error)
	applyMemory   func(instance string, mib int64) error
	applyCPU      func(instance string, cpu int64) error
	applyRootDisk func(instance string, mib int64, fromProfile bool) error
	expandedRoot  func(instance string) (int64, bool, error)
	underLock     func(operation func() (map[string]interface{}, error)) (map[string]interface{}, error)
	checkManaged  func(instance string) error
}

func realResizeSeams() resizeSeams {
	return resizeSeams{
		readState:     instanceStateFor,
		listSummaries: listInstanceStatuses,
		memoryCap:     hostMemoryCapMiB,
		poolSizeMib:   storagePoolSizeMib,
		applyMemory:   applySandboxMemoryResize,
		applyCPU:      applySandboxCpuResize,
		applyRootDisk: applyRootDiskResize,
		expandedRoot:  expandedRootDeviceSizeMib,
		underLock:     withLifecycleLock,
		checkManaged:  requireManagedSandbox,
	}
}

func resizeSandbox(instance string, targets resizeTargets) (map[string]interface{}, error) {
	return resizeSandboxWith(instance, targets, realResizeSeams())
}

func resizeSandboxWith(instance string, targets resizeTargets, seams resizeSeams) (map[string]interface{}, error) {
	if !targets.any() {
		return nil, protoError("invalid_fields", "resize_sandbox requires at least one of budget_memory_mib, budget_cpu, root_disk_mib")
	}
	return seams.underLock(func() (map[string]interface{}, error) {
		if err := seams.checkManaged(instance); err != nil {
			return nil, err
		}
		// A resize is a stopped-instance operation: Incus cannot apply a
		// memory ceiling or grow a VM root disk live, and chasm does not
		// attempt a live resize (idiom 3 refuses what it cannot do safely).
		before, err := seams.readState(instance)
		if err != nil {
			return nil, protoError("invalid_state", "sandbox instance state is unavailable")
		}
		if status, _ := before["status"].(string); status != "Stopped" {
			return nil, protoError("invalid_state", "resize requires a stopped sandbox: stop the sandbox first: chasm stop "+logicalSandboxName(instance))
		}
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
		// The before value is derived only for requested resources: an
		// enforced value chasm cannot parse becomes an explicit unknown in
		// the result instead of aborting, because the write plus read-back
		// below proves the new state without trusting the old one.
		memoryBefore, _ := resizeResourceBefore(summary, sandboxMemoryBudget, targets.memory != 0)
		if memoryBefore.hasError {
			return nil, memoryBefore.err
		}
		cpuBefore, _ := resizeResourceBefore(summary, sandboxCpuBudget, targets.cpu != 0)
		if cpuBefore.hasError {
			return nil, cpuBefore.err
		}
		var root rootDevice
		if targets.rootDisk != 0 {
			root, err = rootDeviceView(summary)
			if err != nil {
				return nil, err
			}
		}
		// Validation happens in full before any mutation: a resize that
		// must be refused is refused without touching the instance.
		if err := validateResizeMemory(targets.memory, seams.memoryCap); err != nil {
			return nil, err
		}
		if err := validateResizeCPU(targets.cpu); err != nil {
			return nil, err
		}
		if err := validateResizeRootDisk(targets.rootDisk, root, seams.poolSizeMib); err != nil {
			return nil, err
		}
		applied := make([]string, 0, 3)
		pending := make([]string, 0, 3)
		if targets.memory != 0 {
			pending = append(pending, "memory")
			if err := seams.applyMemory(instance, targets.memory); err != nil {
				return nil, partialResizeError(applied, pending, err)
			}
			applied = append(applied, fmt.Sprintf("memory %s->%s", renderBeforeMib(memoryBefore), renderMib(targets.memory)))
			pending = pending[:len(pending)-1]
		}
		if targets.cpu != 0 {
			pending = append(pending, "CPU")
			if err := seams.applyCPU(instance, targets.cpu); err != nil {
				return nil, partialResizeError(applied, pending, err)
			}
			applied = append(applied, fmt.Sprintf("CPU %s->%d", renderBeforeCount(cpuBefore), targets.cpu))
			pending = pending[:len(pending)-1]
		}
		if targets.rootDisk != 0 {
			pending = append(pending, "root disk")
			if targets.rootDisk == root.sizeMib {
				// A target equal to the current size is a proven no-op:
				// nothing is written (a profile-inherited device is never
				// turned into an instance override) and the value is still
				// proven by reading the enforced expanded size back.
				expanded, present, err := seams.expandedRoot(instance)
				if err != nil {
					return nil, partialResizeError(applied, pending, err)
				}
				if !present || expanded != targets.rootDisk {
					return nil, partialResizeError(applied, pending,
						protoError("postcondition_failed", "the root disk size was not proven"))
				}
				applied = append(applied, fmt.Sprintf("root disk %s (already %s, proven by read-back)", renderMib(root.sizeMib), renderMib(targets.rootDisk)))
			} else if err := seams.applyRootDisk(instance, targets.rootDisk, root.fromProfile); err != nil {
				return nil, partialResizeError(applied, pending, err)
			} else {
				applied = append(applied, fmt.Sprintf("root disk %s->%s", renderMib(root.sizeMib), renderMib(targets.rootDisk)))
			}
			pending = pending[:len(pending)-1]
		}
		changes := make([]map[string]interface{}, 0, 3)
		if targets.memory != 0 {
			changes = append(changes, changeEntry("memory", memoryBefore, targets.memory, "MiB"))
		}
		if targets.cpu != 0 {
			changes = append(changes, changeEntry("cpu", cpuBefore, targets.cpu, "count"))
		}
		if targets.rootDisk != 0 {
			changes = append(changes, changeEntry("root_disk", beforeValue{mib: root.sizeMib, known: true}, targets.rootDisk, "MiB"))
		}
		return map[string]interface{}{"instance": instance, "resized": changes}, nil
	})
}

// beforeValue is one resource's observed value before the resize: known
// with its MiB or count, or an explicit unknown with the reason (idiom 2).
type beforeValue struct {
	mib      int64
	known    bool
	unknown  string
	hasError bool
	err      error
}

// changeEntry renders one resize result entry with before, after, and the
// unit the values are counted in, so a CPU count is never labeled MiB.
func changeEntry(resource string, before beforeValue, after int64, unit string) map[string]interface{} {
	entry := map[string]interface{}{"resource": resource, "after": after, "unit": unit}
	if before.known {
		entry["before"] = before.mib
	} else {
		entry["before_unknown"] = before.unknown
	}
	return entry
}

// renderBeforeMib renders a memory before-value: the MiB figure, or the
// explicit unknown when it could not be observed.
func renderBeforeMib(before beforeValue) string {
	if !before.known {
		return "unknown"
	}
	return renderMib(before.mib)
}

// renderBeforeCount renders a CPU before-value: the count, or the explicit
// unknown when it could not be observed.
func renderBeforeCount(before beforeValue) string {
	if !before.known {
		return "unknown"
	}
	return strconv.FormatInt(before.mib, 10)
}

// resizeResourceBefore derives one resource's observed value before the
// resize: the enforced Incus value when readable, an explicit unknown when
// the enforced value cannot be parsed (the write plus read-back below
// proves the new state without trusting the old one, so an unsupported
// enforced value is never replaced by the declared record), else the
// declared record (idiom 1). Only a corrupted declared record fails
// closed. When requested is false the resource is not being resized
// and nothing is read at all.
func resizeResourceBefore(summary *incusInstanceSummary, derive func(incusInstanceSummary) (sandboxResourceBudget, error), requested bool) (beforeValue, error) {
	if !requested {
		return beforeValue{}, nil
	}
	budget, err := derive(*summary)
	if err != nil {
		return beforeValue{hasError: true, err: err}, err
	}
	if budget.enforcedPresent {
		return beforeValue{mib: budget.enforced, known: true}, nil
	}
	if budget.enforcedUnsupported != "" {
		return beforeValue{unknown: budget.enforcedUnsupported}, nil
	}
	if budget.declaredPresent {
		return beforeValue{mib: budget.declared, known: true}, nil
	}
	return beforeValue{unknown: "the sandbox resource is neither recorded nor enforced"}, nil
}

// validateResizeMemory re-checks the protocol range (fail closed even on a
// path that skipped the parser) and refuses a target above the host cap,
// naming the cap's source (idiom 4).
func validateResizeMemory(mib int64, memoryCap func() (int64, int64, int64, error)) error {
	if mib == 0 {
		return nil
	}
	if !protocol.ValidMemoryBudget(mib) {
		return protoError("invalid_budget", protocol.BudgetRangeMessage())
	}
	cap, memTotal, reserve, err := memoryCap()
	if err != nil {
		return err
	}
	return hostCapRefusalError(mib, cap, memTotal, reserve)
}

// validateResizeCPU re-checks the protocol range and refuses a target above
// the host CPU count, naming the cap's source.
func validateResizeCPU(cpu int64) error {
	if cpu == 0 {
		return nil
	}
	if !protocol.ValidCpuBudget(cpu) {
		return protoError("invalid_cpu_budget", protocol.CpuBudgetRangeMessage())
	}
	return refuseCpuBudgetAboveHostCap(cpu)
}

// validateResizeRootDisk enforces the relational disk rules: the protocol
// range, grow-only against the live root device, and the storage pool's
// configured size as the ceiling. Every refusal names its numbers.
func validateResizeRootDisk(mib int64, root rootDevice, poolSizeMib func() (int64, error)) error {
	if mib == 0 {
		return nil
	}
	if !protocol.ValidRootDiskMib(mib) {
		return protoError("invalid_root_disk", protocol.RootDiskRangeMessage())
	}
	if !root.present {
		return protoError("unsafe_state", "the sandbox's root device could not be read")
	}
	if mib < root.sizeMib {
		return protoError("invalid_state", fmt.Sprintf(
			"shrinking the root disk is not supported: current root disk is %s, requested %s",
			renderMib(root.sizeMib), renderMib(mib)))
	}
	poolMib, err := poolSizeMib()
	if err != nil {
		return err
	}
	if mib > poolMib {
		return protoError("resource_exhausted", fmt.Sprintf(
			"requested root disk %s exceeds the storage pool's configured size %s; grow the pool first (see docs/install.md)",
			renderMib(mib), renderMib(poolMib)))
	}
	return nil
}

// rootDevice describes the sandbox's root device as Incus enforces it.
type rootDevice struct {
	present     bool
	sizeMib     int64
	fromProfile bool // inherited from the site profile; the resize must create an instance-level override
}

// rootDeviceView reads the root device from the expanded devices of one
// inventory record. A root device without a parseable size fails closed.
func rootDeviceView(summary *incusInstanceSummary) (rootDevice, error) {
	device, present := summary.ExpandedDevices["root"]
	if !present {
		return rootDevice{}, protoError("unsafe_state", "the sandbox has no root device")
	}
	raw := device["size"]
	if raw == "" {
		return rootDevice{}, protoError("unsafe_state", "the sandbox's root device has no size")
	}
	bytes, err := parseMemoryLimitBytes(raw)
	if err != nil {
		return rootDevice{}, protoError("unsafe_state", "the sandbox's root device size is invalid")
	}
	_, instanceLevel := summary.Devices["root"]
	return rootDevice{present: true, sizeMib: (bytes + (1 << 20) - 1) >> 20, fromProfile: !instanceLevel}, nil
}

// partialResizeError renders a partial application without silent success:
// the message names every applied change with its before/after and every
// pending change, then the cause.
func partialResizeError(applied []string, pending []string, cause error) error {
	if len(applied) == 0 {
		return cause
	}
	var code, message string
	var protocolErr *ProtocolError
	if errors.As(cause, &protocolErr) {
		code, message = protocolErr.Code, protocolErr.Message
	} else {
		code, message = "operation_failed", cause.Error()
	}
	return protoError(code, fmt.Sprintf(
		"resize partially applied: applied: %s; not applied: %s: %s",
		strings.Join(applied, ", "), strings.Join(pending, ", "), message))
}

// renderMib prints a MiB value for operator-facing messages: whole GiB when
// the value divides evenly, plain MiB otherwise.
func renderMib(mib int64) string {
	if mib > 0 && mib%1024 == 0 {
		return fmt.Sprintf("%dGiB", mib/1024)
	}
	return fmt.Sprintf("%dMiB", mib)
}

// applySandboxMemoryResize writes the declared budget and the enforced
// limits.memory together, each proven by its own read-back (idiom 1). Both
// are idempotent writes, so re-applying the same target still proves the
// read-back without changing the value.
func applySandboxMemoryResize(instance string, mib int64) error {
	if err := setSandboxBudget(instance, mib); err != nil {
		return err
	}
	return setInstanceMemoryLimit(instance, mib)
}

// applySandboxCpuResize writes the CPU budget key and the enforced
// limits.cpu together, each proven by read-back.
func applySandboxCpuResize(instance string, cpu int64) error {
	if err := setSandboxCpuBudget(instance, cpu); err != nil {
		return err
	}
	return setInstanceCpuLimit(instance, cpu)
}

// storagePoolSizeMib reads the site pool's configured size. The value is
// the ceiling a root-disk resize may never exceed; an unreadable or unset
// size fails closed rather than admitting an unbounded disk.
func storagePoolSizeMib() (int64, error) {
	pool := siteconfig.Active().StoragePool
	var out strings.Builder
	code, err := runIncus([]string{"storage", "get", pool, "size"}, nil, &out, nil, 30*time.Second)
	if err != nil || code != 0 {
		return 0, protoError("unsafe_state", "the storage pool's configured size could not be read")
	}
	value := strings.TrimSpace(out.String())
	if value == "" {
		return 0, protoError("unsafe_state", "the storage pool has no configured size")
	}
	bytes, parseErr := parseMemoryLimitBytes(value)
	if parseErr != nil {
		return 0, protoError("unsafe_state", "the storage pool's configured size is invalid")
	}
	return (bytes + (1 << 20) - 1) >> 20, nil
}

// rootDeviceArguments renders the incus device argv for a root disk
// resize: `device set` when the instance owns the root device, `device
// override` when it is inherited from the site profile (the override keeps
// the profile's pool and path and adds the instance-level size).
func rootDeviceArguments(instance string, mib int64, fromProfile bool) []string {
	if fromProfile {
		return []string{"config", "device", "override", instance, "root", "size=" + memoryLimitValue(mib)}
	}
	return []string{"config", "device", "set", instance, "root", "size=" + memoryLimitValue(mib)}
}

// applyRootDiskResize applies the new root disk size: an instance-level
// device override when the root device comes from the site profile, a plain
// device set when the instance already owns the device — then proves the
// change by reading the expanded device size back from Incus.
func applyRootDiskResize(instance string, mib int64, fromProfile bool) error {
	code, err := runIncus(rootDeviceArguments(instance, mib, fromProfile), nil, nil, nil, 30*time.Second)
	if err != nil || code != 0 {
		return protoError("operation_failed", "the root disk size could not be applied")
	}
	expanded, present, err := expandedRootDeviceSizeMib(instance)
	if err != nil {
		return err
	}
	if !present || expanded != mib {
		return protoError("postcondition_failed", "the root disk size was not proven")
	}
	return nil
}

// incusQueryGETFn is the read seam for the root-disk proof: tests inject
// the expanded device document so the read-back comparison runs without an
// Incus API socket.
var incusQueryGETFn = incusQueryGET

// expandedRootDeviceSizeMib reads the root device's size from the
// instance's expanded devices — the value Incus actually enforces, whether
// it is set on the instance or inherited through the profile.
func expandedRootDeviceSizeMib(instance string) (int64, bool, error) {
	payload, err := incusQueryGETFn("/1.0/instances/" + instance)
	if err != nil {
		return 0, false, protoError("operation_failed", "the root disk size could not be read back")
	}
	expanded, ok := payload["expanded_devices"].(map[string]interface{})
	if !ok {
		return 0, false, protoError("unsafe_state", "the instance's expanded devices are unavailable")
	}
	device, ok := expanded["root"].(map[string]interface{})
	if !ok {
		return 0, false, nil
	}
	raw, _ := device["size"].(string)
	if raw == "" {
		return 0, false, protoError("unsafe_state", "the sandbox's root device has no size")
	}
	bytes, parseErr := parseMemoryLimitBytes(raw)
	if parseErr != nil {
		return 0, false, protoError("unsafe_state", "the sandbox's root device size is invalid")
	}
	return (bytes + (1 << 20) - 1) >> 20, true, nil
}
