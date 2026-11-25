package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/rahulrajaram/chasm/internal/protocol"
)

type humanSandboxDetail struct {
	Name        string
	State       string
	Mode        string
	Description string
	PID         int64
}

type humanService struct {
	Name     string
	State    string
	Result   string
	Restarts int64
}

type humanSandboxStatus struct {
	Detail   humanSandboxDetail
	Services []humanService
}

type humanLayoutEntry struct {
	Path string
	Type string
}

type humanSandboxLayout struct {
	Name       string
	Root       string
	Depth      int64
	Freshness  string
	ObservedAt string
	Complete   bool
	Entries    []humanLayoutEntry
}

func parseHumanSandboxDetail(result map[string]interface{}) (humanSandboxDetail, error) {
	name := stringField(result, "logical_name")
	state := stringField(result, "instance_status")
	description := stringField(result, "description")
	pid, pidOK := humanInteger(result["instance_pid"])
	if name == "" || (state != "Running" && state != "Frozen" && state != "Stopped") || !pidOK || pid < 0 ||
		(state == "Running" && pid == 0) || (state == "Stopped" && pid != 0) || !protocolDescription(description) {
		return humanSandboxDetail{}, fmt.Errorf("sandbox detail response is malformed")
	}
	mode := "managed"
	if stringField(result, "workspace") != "/workspace" || stringField(result, "shared") != "/shared" {
		return humanSandboxDetail{}, fmt.Errorf("sandbox detail response is malformed")
	}
	return humanSandboxDetail{Name: name, State: strings.ToLower(state), Mode: mode, Description: description, PID: pid}, nil
}

func parseHumanSandboxStatus(result map[string]interface{}) (humanSandboxStatus, error) {
	instance := stringField(result, "instance")
	state := stringField(result, "instance_status")
	pid, pidOK := humanInteger(result["instance_pid"])
	rawServices, servicesOK := result["services"].([]interface{})
	if !protocol.ValidSandboxInstance(instance) || (state != "Running" && state != "Frozen" && state != "Stopped") || !pidOK || pid < 0 ||
		(state == "Running" && pid == 0) || (state == "Stopped" && pid != 0) || !servicesOK {
		return humanSandboxStatus{}, fmt.Errorf("sandbox status response is malformed")
	}
	services := make([]humanService, 0, len(rawServices))
	for _, candidate := range rawServices {
		item, ok := candidate.(map[string]interface{})
		if !ok {
			return humanSandboxStatus{}, fmt.Errorf("sandbox status response is malformed")
		}
		service := humanService{Name: stringField(item, "name"), State: stringField(item, "active_state"), Result: stringField(item, "result")}
		var restartsOK bool
		service.Restarts, restartsOK = humanInteger(item["n_restarts"])
		if service.Name == "" || service.State == "" || service.Result == "" || !restartsOK || service.Restarts < 0 {
			return humanSandboxStatus{}, fmt.Errorf("sandbox status response is malformed")
		}
		services = append(services, service)
	}
	detail := humanSandboxDetail{Name: instance, State: strings.ToLower(state), Mode: "managed", PID: pid}
	return humanSandboxStatus{Detail: detail, Services: services}, nil
}

func parseHumanSandboxLayout(result map[string]interface{}) (humanSandboxLayout, error) {
	name := stringField(result, "logical_name")
	root := stringField(result, "root")
	freshness := stringField(result, "freshness")
	observedAt := stringField(result, "observed_at")
	depth, depthOK := humanInteger(result["depth"])
	complete, completeOK := result["complete"].(bool)
	rawEntries, entriesOK := result["entries"].([]interface{})
	if name == "" || root != "/workspace" || !depthOK || depth < 1 || depth > 5 || !completeOK || !entriesOK ||
		!validObservation(freshness, observedAt, true) || complete != (freshness != "unobserved") {
		return humanSandboxLayout{}, fmt.Errorf("sandbox layout response is malformed")
	}
	entries := make([]humanLayoutEntry, 0, len(rawEntries))
	for _, candidate := range rawEntries {
		item, ok := candidate.(map[string]interface{})
		if !ok {
			return humanSandboxLayout{}, fmt.Errorf("sandbox layout response is malformed")
		}
		entry := humanLayoutEntry{Path: stringField(item, "path"), Type: stringField(item, "type")}
		if !isWorkspacePath(entry.Path) || (entry.Type != "directory" && entry.Type != "file" && entry.Type != "symlink") {
			return humanSandboxLayout{}, fmt.Errorf("sandbox layout response is malformed")
		}
		entries = append(entries, entry)
	}
	return humanSandboxLayout{Name: name, Root: root, Depth: depth, Freshness: freshness, ObservedAt: observedAt, Complete: complete, Entries: entries}, nil
}

func humanInteger(value interface{}) (int64, bool) {
	switch number := value.(type) {
	case int:
		return int64(number), true
	case int64:
		return number, true
	case float64:
		integer := int64(number)
		return integer, float64(integer) == number
	case json.Number:
		integer, err := number.Int64()
		return integer, err == nil
	default:
		return 0, false
	}
}

func protocolDescription(description string) bool {
	return protocol.ValidSandboxDescription(description)
}

func validGitCommit(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, character := range value {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}

func renderSandboxDetail(output io.Writer, detail humanSandboxDetail, style humanOutputStyle) {
	fmt.Fprintln(output, escapeHumanText(detail.Name))
	fmt.Fprintf(output, "  %-12s %s\n", "State", styledState(detail.State, 0, style.Color))
	fmt.Fprintf(output, "  %-12s %s\n", "Mode", detail.Mode)
	if detail.State == "running" {
		fmt.Fprintf(output, "  %-12s %d\n", "PID", detail.PID)
	}
	description := detail.Description
	if description == "" {
		description = "—"
	}
	fmt.Fprintf(output, "  %-12s %s\n", "Description", description)
}

// humanResourceValue is one view of a resource as the operator sees it:
// either an observed value with its unit, or an explicit unknown with the
// reason it could not be observed (idiom 2).
type humanResourceValue struct {
	Known  bool
	Mib    int64
	Count  int64
	Reason string
}

type humanMemoryResources struct {
	Declared     humanResourceValue
	Enforced     humanResourceValue
	GuestTotal   humanResourceValue
	GuestUsed    humanResourceValue
	HostResident humanResourceValue
	HostCap      humanResourceValue
	Drift        bool
	DriftUnknown bool
}

type humanCpuResources struct {
	Declared     humanResourceValue
	Enforced     humanResourceValue
	HostCount    humanResourceValue
	Drift        bool
	DriftUnknown bool
}

type humanRootDiskResources struct {
	Size     humanResourceValue
	PoolUsed humanResourceValue
	Origin   string
}

type humanWorkspaceResources struct {
	Volume          string
	Used            humanResourceValue
	Quota           humanResourceValue
	QuotaNone       bool
	QuotaNoneReason string
	PoolName        string
	PoolSize        humanResourceValue
	PoolUsed        humanResourceValue
}

type humanPoolResources struct {
	Name           string
	Size           humanResourceValue
	Used           humanResourceValue
	LoopKnown      bool
	LoopFile       string
	ApparentMib    int64
	AllocatedMib   int64
	BackingFreeMib int64
	Warnings       []string
}

type humanHostResources struct {
	MemTotal     int64
	MemAvailable int64
	Reserve      int64
	Admitted     int64
	ResidentSum  int64
	ObservedOwn  int64
	Warnings     []string
}

type humanSandboxResources struct {
	Memory      *humanMemoryResources
	Cpu         *humanCpuResources
	RootDisk    *humanRootDiskResources
	Workspace   *humanWorkspaceResources
	Pool        *humanPoolResources
	Host        *humanHostResources
	Unavailable string
}

// parseHumanSandboxResources reads the resources object of an inspect or
// status result. present is false when the executor did not send one; a
// present but malformed object is an error, never a silent partial view.
func parseHumanSandboxResources(result map[string]interface{}) (humanSandboxResources, bool) {
	raw, ok := result["resources"].(map[string]interface{})
	if !ok {
		return humanSandboxResources{}, false
	}
	var resources humanSandboxResources
	if unavailable, ok := raw["unavailable"].(string); ok && unavailable != "" {
		resources.Unavailable = unavailable
		return resources, true
	}
	if memory, ok := raw["memory"].(map[string]interface{}); ok {
		resources.Memory = &humanMemoryResources{
			Declared:     parseHumanResourceValue(memory["declared"]),
			Enforced:     parseHumanResourceValue(memory["enforced"]),
			GuestTotal:   parseHumanResourceValue(memory["guest_total"]),
			GuestUsed:    parseHumanResourceValue(memory["guest_used"]),
			HostResident: parseHumanResourceValue(memory["host_resident"]),
			HostCap:      parseHumanResourceValue(memory["host_cap"]),
			Drift:        memory["drift"] == true,
			DriftUnknown: memory["drift"] == "unverifiable",
		}
	}
	if cpu, ok := raw["cpu"].(map[string]interface{}); ok {
		resources.Cpu = &humanCpuResources{
			Declared:     parseHumanResourceValue(cpu["declared"]),
			Enforced:     parseHumanResourceValue(cpu["enforced"]),
			HostCount:    parseHumanResourceValue(cpu["host_count"]),
			Drift:        cpu["drift"] == true,
			DriftUnknown: cpu["drift"] == "unverifiable",
		}
	}
	if root, ok := raw["root_disk"].(map[string]interface{}); ok {
		resources.RootDisk = &humanRootDiskResources{
			Size:     parseHumanResourceValue(root["size"]),
			PoolUsed: parseHumanResourceValue(root["pool_used"]),
		}
		resources.RootDisk.Origin, _ = root["origin"].(string)
	}
	if workspace, ok := raw["workspace"].(map[string]interface{}); ok {
		entry := &humanWorkspaceResources{
			Used:  parseHumanResourceValue(workspace["used"]),
			Quota: parseHumanResourceValue(workspace["quota"]),
		}
		entry.Volume, _ = workspace["volume"].(string)
		if quota, ok := workspace["quota"].(map[string]interface{}); ok && quota["none"] == true {
			entry.QuotaNone = true
			entry.QuotaNoneReason, _ = quota["reason"].(string)
		}
		if pool, ok := workspace["pool"].(map[string]interface{}); ok {
			entry.PoolName, _ = pool["name"].(string)
			entry.PoolSize = parseHumanResourceValue(pool["size"])
			entry.PoolUsed = parseHumanResourceValue(pool["used"])
		}
		resources.Workspace = entry
	}
	if pool, ok := raw["pool"].(map[string]interface{}); ok {
		entry := &humanPoolResources{
			Size:     parseHumanResourceValue(pool["size"]),
			Used:     parseHumanResourceValue(pool["used"]),
			Warnings: parseHumanWarnings(pool["warnings"]),
		}
		entry.Name, _ = pool["name"].(string)
		if loop, ok := pool["loop"].(map[string]interface{}); ok && loop["unknown"] != true {
			entry.LoopKnown = true
			entry.LoopFile, _ = loop["file"].(string)
			entry.ApparentMib, _ = humanInteger(loop["apparent_mib"])
			entry.AllocatedMib, _ = humanInteger(loop["allocated_mib"])
			entry.BackingFreeMib, _ = humanInteger(loop["backing_free_mib"])
		}
		resources.Pool = entry
	}
	if host, ok := raw["host"].(map[string]interface{}); ok {
		entry := &humanHostResources{Warnings: parseHumanWarnings(host["warnings"])}
		entry.MemTotal, _ = humanInteger(host["mem_total_mib"])
		entry.MemAvailable, _ = humanInteger(host["mem_available_mib"])
		entry.Reserve, _ = humanInteger(host["host_reserve_mib"])
		entry.Admitted, _ = humanInteger(host["admitted_mib"])
		entry.ResidentSum, _ = humanInteger(host["resident_sum_mib"])
		entry.ObservedOwn, _ = humanInteger(host["observed_own_mib"])
		resources.Host = entry
	}
	return resources, true
}

// parseHumanResourceValue reads one view: an observed value with its unit
// or an explicit unknown with its reason. A view in neither shape is
// unknown with a reason that says the response was malformed.
func parseHumanResourceValue(value interface{}) humanResourceValue {
	view, ok := value.(map[string]interface{})
	if !ok {
		return humanResourceValue{Reason: "the view is missing from the response"}
	}
	if view["unknown"] == true {
		reason, _ := view["reason"].(string)
		return humanResourceValue{Reason: reason}
	}
	if mib, ok := humanInteger(view["mib"]); ok {
		return humanResourceValue{Known: true, Mib: mib}
	}
	if count, ok := humanInteger(view["count"]); ok {
		return humanResourceValue{Known: true, Count: count}
	}
	return humanResourceValue{Reason: "the view is malformed in the response"}
}

func parseHumanWarnings(value interface{}) []string {
	items, ok := value.([]interface{})
	if !ok {
		return nil
	}
	warnings := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok && text != "" {
			warnings = append(warnings, text)
		}
	}
	return warnings
}

// humanObserved renders an observed MiB value the way guests and hosts
// report memory: whole GiB when exact, one decimal otherwise, plain MiB
// below a GiB.
func humanObserved(mib int64) string {
	if mib >= 1024 {
		if mib%1024 == 0 {
			return fmt.Sprintf("%d GiB", mib/1024)
		}
		return fmt.Sprintf("%.1f GiB", float64(mib)/1024)
	}
	return fmt.Sprintf("%d MiB", mib)
}

// humanMemoryObserved renders a memory observation with one decimal so
// guest and resident figures keep their granularity (47.0 GiB, not 47 GiB).
func humanMemoryObserved(mib int64) string {
	if mib >= 1024 {
		return fmt.Sprintf("%.1f GiB", float64(mib)/1024)
	}
	return fmt.Sprintf("%d MiB", mib)
}

// renderResourceValue renders one view: the value with its unit, or the
// unknown marker with its reason, so an unobservable value is never left
// out (idiom 2).
func renderResourceValue(value humanResourceValue, unit string) string {
	if !value.Known {
		reason := value.Reason
		if reason == "" {
			reason = "unknown"
		}
		return "unknown (" + reason + ")"
	}
	if unit == "count" {
		return strconv.FormatInt(value.Count, 10)
	}
	return humanObserved(value.Mib)
}

// memoryValueText renders one memory view: the observation with its
// decimal GiB form, or the unknown marker with its reason.
func memoryValueText(value humanResourceValue) string {
	if !value.Known {
		reason := value.Reason
		if reason == "" {
			reason = "unknown"
		}
		return "unknown (" + reason + ")"
	}
	return humanMemoryObserved(value.Mib)
}

// renderSandboxResources prints the Resources section after the sandbox's
// own fields: one aligned row per resource with the views side by side,
// then drift lines with the command that resolves them, then the host and
// pool warnings (idioms 2 and 4).
func renderSandboxResources(output io.Writer, resources humanSandboxResources, name string, style humanOutputStyle) {
	fmt.Fprintln(output)
	fmt.Fprintln(output, "RESOURCES")
	if resources.Unavailable != "" {
		fmt.Fprintf(output, "  %-12s unavailable (%s)\n", "Resources", escapeHumanText(resources.Unavailable))
		return
	}
	if resources.Memory != nil {
		declared := fmt.Sprintf("%d MiB", resources.Memory.Declared.Mib)
		if !resources.Memory.Declared.Known {
			declared = renderResourceValue(resources.Memory.Declared, "mib")
		}
		enforced := fmt.Sprintf("%d MiB", resources.Memory.Enforced.Mib)
		if !resources.Memory.Enforced.Known {
			enforced = renderResourceValue(resources.Memory.Enforced, "mib")
		}
		line := "declared " + declared +
			" \u00b7 enforced " + enforced +
			" \u00b7 guest " + memoryValueText(resources.Memory.GuestTotal) + " total, " +
			memoryValueText(resources.Memory.GuestUsed) + " used" +
			" \u00b7 host " + memoryValueText(resources.Memory.HostResident) + " resident" +
			" \u00b7 cap " + memoryValueText(resources.Memory.HostCap)
		fmt.Fprintf(output, "  %-12s %s\n", "Memory", escapeHumanText(line))
	}
	if resources.Cpu != nil {
		line := "declared " + renderResourceValue(resources.Cpu.Declared, "count") +
			" \u00b7 enforced " + renderResourceValue(resources.Cpu.Enforced, "count") +
			" \u00b7 host " + renderResourceValue(resources.Cpu.HostCount, "count")
		fmt.Fprintf(output, "  %-12s %s\n", "CPU", escapeHumanText(line))
	}
	if resources.RootDisk != nil {
		line := renderResourceValue(resources.RootDisk.Size, "mib")
		switch resources.RootDisk.Origin {
		case "override":
			line += " (instance override)"
		case "profile":
			line += " (site profile)"
		}
		line += " \u00b7 pool holds " + renderResourceValue(resources.RootDisk.PoolUsed, "mib")
		fmt.Fprintf(output, "  %-12s %s\n", "Root disk", escapeHumanText(line))
	}
	if resources.Workspace != nil {
		line := renderResourceValue(resources.Workspace.Used, "mib") + " used \u00b7 quota "
		if resources.Workspace.QuotaNone {
			line += "none (" + resources.Workspace.QuotaNoneReason + ")"
		} else {
			line += renderResourceValue(resources.Workspace.Quota, "mib")
		}
		if resources.Workspace.PoolName != "" {
			line += " \u00b7 pool " + resources.Workspace.PoolName + " " +
				renderResourceValue(resources.Workspace.PoolSize, "mib") + ", " +
				renderResourceValue(resources.Workspace.PoolUsed, "mib") + " used"
		}
		fmt.Fprintf(output, "  %-12s %s\n", "Workspace", escapeHumanText(line))
	}
	if resources.Pool != nil {
		line := resources.Pool.Name + " \u00b7 " + renderResourceValue(resources.Pool.Size, "mib") +
			" \u00b7 " + renderResourceValue(resources.Pool.Used, "mib") + " used"
		if resources.Pool.LoopKnown {
			line += fmt.Sprintf(" \u00b7 loop file %s allocated of %s apparent \u00b7 %s free behind it",
				humanObserved(resources.Pool.AllocatedMib), humanObserved(resources.Pool.ApparentMib), humanObserved(resources.Pool.BackingFreeMib))
		}
		fmt.Fprintf(output, "  %-12s %s\n", "Pool", escapeHumanText(line))
	}
	if resources.Host != nil {
		fmt.Fprintf(output, "  %-12s MemTotal %s \u00b7 available %s \u00b7 reserve %d MiB \u00b7 admitted %s \u00b7 host's own use %s\n",
			"Host", humanObserved(resources.Host.MemTotal), humanObserved(resources.Host.MemAvailable),
			resources.Host.Reserve, humanObserved(resources.Host.Admitted), humanObserved(resources.Host.ObservedOwn))
	}
	if resources.Memory != nil && resources.Memory.Drift {
		fmt.Fprintf(output, "  drift: memory record %d MiB, enforced %s; fix: chasm resize %s --memory %d\n",
			resources.Memory.Declared.Mib, memoryEnforcedMibText(resources.Memory), escapeHumanText(name), resources.Memory.Enforced.Mib)
	}
	if resources.Memory != nil && resources.Memory.DriftUnknown {
		fmt.Fprintf(output, "  drift: memory is unverifiable (%s)\n", escapeHumanText(resources.Memory.Enforced.Reason))
	}
	if resources.Cpu != nil && resources.Cpu.Drift {
		fmt.Fprintf(output, "  drift: CPU record %d, enforced %d; fix: chasm resize %s --cpu %d\n",
			resources.Cpu.Declared.Count, resources.Cpu.Enforced.Count, escapeHumanText(name), resources.Cpu.Enforced.Count)
	}
	if resources.Cpu != nil && resources.Cpu.DriftUnknown {
		fmt.Fprintf(output, "  drift: CPU is unverifiable (%s)\n", escapeHumanText(resources.Cpu.Enforced.Reason))
	}
	for _, warning := range hostAndPoolWarnings(resources) {
		fmt.Fprintf(output, "  warning: %s\n", escapeHumanText(warning))
	}
}

// memoryEnforcedMibText renders the enforced memory of a drifted memory
// record as raw MiB: the resize fix names the exact value to apply.
func memoryEnforcedMibText(memory *humanMemoryResources) string {
	if memory.Enforced.Known {
		return fmt.Sprintf("%d MiB", memory.Enforced.Mib)
	}
	return "unknown"
}

// hostAndPoolWarnings collects the host-reserve and pool-fill warnings so
// they print after the resource rows.
func hostAndPoolWarnings(resources humanSandboxResources) []string {
	warnings := make([]string, 0, 2)
	if resources.Pool != nil {
		warnings = append(warnings, resources.Pool.Warnings...)
	}
	if resources.Host != nil {
		warnings = append(warnings, resources.Host.Warnings...)
	}
	return warnings
}

func renderSandboxStatus(output io.Writer, status humanSandboxStatus, style humanOutputStyle) {
	renderSandboxDetail(output, status.Detail, style)
	if len(status.Services) == 0 {
		return
	}
	fmt.Fprintln(output)
	fmt.Fprintln(output, "SERVICES")
	nameWidth := len("SERVICE")
	for _, service := range status.Services {
		if width := cellWidth(service.Name); width > nameWidth {
			nameWidth = width
		}
	}
	fmt.Fprintf(output, "%-*s  %-8s  %-8s  %s\n", nameWidth, "SERVICE", "STATE", "RESULT", "RESTARTS")
	healthy := 0
	for _, service := range status.Services {
		fmt.Fprintf(output, "%-*s  %-8s  %-8s  %d\n", nameWidth, escapeHumanText(service.Name), escapeHumanText(service.State), escapeHumanText(service.Result), service.Restarts)
		if service.State == "active" && service.Result == "success" && service.Restarts == 0 {
			healthy++
		}
	}
	fmt.Fprintln(output)
	fmt.Fprintf(output, "%d %s · %d healthy\n", len(status.Services), pluralize("service", len(status.Services)), healthy)
}

func renderSandboxLayout(output io.Writer, layout humanSandboxLayout) {
	fmt.Fprintln(output, escapeHumanText(layout.Name))
	fmt.Fprintf(output, "  %-12s %s\n", "Freshness", freshnessLabel(layout.Freshness))
	if layout.ObservedAt != "" {
		fmt.Fprintf(output, "  %-12s %s\n", "Observed", escapeHumanText(layout.ObservedAt))
	}
	fmt.Fprintf(output, "  %-12s %s\n", "Root", layout.Root)
	fmt.Fprintf(output, "  %-12s %d\n", "Depth", layout.Depth)
	if len(layout.Entries) == 0 {
		fmt.Fprintln(output)
		if layout.Complete {
			fmt.Fprintln(output, "No entries observed.")
		} else {
			fmt.Fprintln(output, "No filesystem observation is available.")
		}
	} else {
		fmt.Fprintln(output)
		fmt.Fprintln(output, "TYPE       PATH")
		for _, entry := range layout.Entries {
			fmt.Fprintf(output, "%-10s %s\n", entry.Type, escapeHumanText(entry.Path))
		}
	}
	fmt.Fprintln(output)
	fmt.Fprintf(output, "%d %s · %s\n", len(layout.Entries), pluralize("entry", len(layout.Entries)), freshnessLabel(layout.Freshness))
}

func renderLifecycleObservations(stdout io.Writer, result map[string]interface{}) {
	if result["lifecycle_observations_unavailable"] == true {
		fmt.Fprintln(stdout, "Lifecycle timings unavailable.")
		return
	}
	observations, ok := result["lifecycle_observations"].([]interface{})
	if !ok || len(observations) == 0 {
		return
	}
	fmt.Fprintln(stdout, "Lifecycle timings")
	for _, entry := range observations {
		observation, _ := entry.(map[string]interface{})
		operation := stringField(observation, "operation")
		switch operation {
		case "create", "start", "stop":
		default:
			continue
		}
		timings, _ := observation["phase_timings_ms"].(map[string]interface{})
		outcome := "failed"
		if observation["ok"] == true {
			outcome = "ok"
		}
		fmt.Fprintf(stdout, "  %s (%s)", operation, outcome)
		for _, phase := range []struct{ key, label string }{
			{"total", "total"}, {"provisioning", "provisioning"}, {"clone", "clone"}, {"boot", "boot"},
			{"guest_agent_ready", "guest agent"}, {"shutdown_request", "shutdown"}, {"stopped_confirmation", "stopped confirmation"},
			{"provisioning_clone", "provisioning clone"}, {"provisioning_boot", "provisioning boot"},
			{"provisioning_guest_agent_ready", "provisioning guest agent"},
			{"provisioning_shutdown_request", "provisioning shutdown"}, {"provisioning_stopped_confirmation", "provisioning stopped confirmation"},
		} {
			if millis, ok := humanInteger(timings[phase.key]); ok && millis >= 0 {
				fmt.Fprintf(stdout, "  %s %d ms", phase.label, millis)
			}
		}
		fmt.Fprintln(stdout)
	}
}
