package execserver

// A managed sandbox that will never run again can be torn down through
// the audited control plane instead of unaudited root `incus delete` (which
// would bypass the audit trail). Deletion refuses unless the instance is
// Stopped — the operator stops it first, deletion never stops anything —
// then performs an ordered teardown:
// instance, then its derived workspace volume, then its catalog snapshot.
// Every step is journaled in the response alongside fresh postcondition
// evidence. The global shared volume
// is never touched because the workspace volume name is derived from the
// instance name. Deletion is idempotent: an already-absent instance with no
// residue succeeds as already_deleted.

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/rahulrajaram/chasm/internal/protocol"
	"github.com/rahulrajaram/chasm/internal/siteconfig"
)

const (
	deleteInstanceTimeout = 120 * time.Second
	volumeInventoryLimit  = 256 * 1024
)

type deleteSandboxEnvironment struct {
	inventory      func() ([]incusInstanceSummary, error)
	deleteInstance func() error
	volumePresent  func() (bool, error)
	volumeMetadata func() (bool, error)
	deleteVolume   func() error
	catalogPresent func() (bool, error)
	removeCatalog  func() error
}

type deleteStep struct {
	Step    string `json:"step"`
	Outcome string `json:"outcome"`
}

func deleteSandbox(request protocol.Request) (map[string]interface{}, error) {
	instance := request.Instance()
	return withLifecycleLock(func() (map[string]interface{}, error) {
		result, err := deleteSandboxWith(instance, productionDeleteEnvironment(instance))
		if err != nil {
			return nil, err
		}
		if err := removeLifecycleObservations(lifecycleRoot(), 0, instance); err != nil {
			return nil, protoError("operation_failed", "sandbox was deleted but its lifecycle observations could not be removed")
		}
		return result, nil
	})
}

// deleteSandboxWith is the lock-free delete core, mirroring stopSandboxWith's
// injection seams so refusal paths and postconditions are unit-testable
// without host state.
func deleteSandboxWith(instance string, environment deleteSandboxEnvironment) (map[string]interface{}, error) {
	if !protocol.ValidSandboxInstance(instance) {
		return nil, protoError("invalid_instance", "instance is not permitted")
	}
	return deleteSandboxLocked(instance, environment)
}

func deleteSandboxLocked(instance string, environment deleteSandboxEnvironment) (map[string]interface{}, error) {
	before, err := environment.inventory()
	if err != nil {
		return nil, protoError("unsafe_state", "sandbox inventory is unavailable")
	}
	var steps []deleteStep
	present := false
	managed := false
	status := ""
	for _, summary := range before {
		if summary.Name == instance {
			present, managed, status = true, summary.Config[siteconfig.Active().MetadataKey("managed")] == "true", summary.Status
		}
	}
	instanceDeleted := false
	if present {
		if !managed {
			return nil, protoError("invalid_state", "sandbox is not a managed instance")
		}
		switch status {
		case "Running":
			return nil, protoError("invalid_state", "sandbox cannot be deleted while running; stop it first")
		case "Stopped":
		default:
			return nil, protoError("invalid_state", "sandbox instance is neither running nor stopped")
		}
	}
	volumePresent, err := environment.volumePresent()
	if err != nil {
		return nil, protoError("unsafe_state", "storage volume inventory is unavailable")
	}
	if volumePresent {
		if environment.volumeMetadata == nil {
			return nil, protoError("unsafe_state", "workspace volume ownership metadata is unavailable")
		}
		valid, err := environment.volumeMetadata()
		if err != nil || !valid {
			return nil, protoError("unsafe_state", "workspace volume ownership metadata is not verified")
		}
	}
	if present {
		if err := environment.deleteInstance(); err != nil {
			return nil, protoError("operation_failed", "instance deletion failed")
		}
		instanceDeleted = true
		steps = append(steps, deleteStep{Step: "delete_instance", Outcome: "deleted"})
	} else {
		steps = append(steps, deleteStep{Step: "delete_instance", Outcome: "already_absent"})
	}
	volumeDeleted := false
	if volumePresent {
		if err := environment.deleteVolume(); err != nil {
			return nil, protoError("operation_failed", "workspace volume deletion failed")
		}
		volumeDeleted = true
		steps = append(steps, deleteStep{Step: "delete_workspace_volume", Outcome: "deleted"})
	} else {
		steps = append(steps, deleteStep{Step: "delete_workspace_volume", Outcome: "already_absent"})
	}
	catalogRemoved := false
	catalogPresent, err := environment.catalogPresent()
	if err != nil {
		return nil, protoError("unsafe_state", "catalog state is unavailable")
	}
	if catalogPresent {
		if err := environment.removeCatalog(); err != nil {
			return nil, protoError("operation_failed", "catalog snapshot removal failed")
		}
		catalogRemoved = true
		steps = append(steps, deleteStep{Step: "remove_catalog_snapshot", Outcome: "removed"})
	} else {
		steps = append(steps, deleteStep{Step: "remove_catalog_snapshot", Outcome: "already_absent"})
	}
	after, err := environment.inventory()
	if err != nil {
		return nil, protoError("postcondition_failed", "sandbox inventory is unavailable after deletion")
	}
	for _, summary := range after {
		if summary.Name == instance {
			return nil, protoError("postcondition_failed", "sandbox instance is still present after deletion")
		}
	}
	stillPresent, err := environment.volumePresent()
	if err != nil || stillPresent {
		return nil, protoError("postcondition_failed", "workspace volume is still present after deletion")
	}
	stillCataloged, err := environment.catalogPresent()
	if err != nil || stillCataloged {
		return nil, protoError("postcondition_failed", "catalog snapshot is still present after deletion")
	}
	running := int64(0)
	for _, summary := range after {
		if summary.Status == "Running" || summary.Status == "Frozen" {
			running++
		}
	}
	return map[string]interface{}{
		"instance":                 instance,
		"already_deleted":          !instanceDeleted,
		"instance_deleted":         instanceDeleted,
		"workspace_volume_deleted": volumeDeleted,
		"catalog_snapshot_removed": catalogRemoved,
		"steps":                    steps,
		"running_instances_after":  running,
		"one_running_guard":        "untouched",
	}, nil
}

func productionDeleteEnvironment(instance string) deleteSandboxEnvironment {
	site := siteconfig.Active()
	volume := site.WorkspaceVolumeName(instance)
	pool := site.StoragePool
	return deleteSandboxEnvironment{
		inventory: listInstanceStatuses,
		deleteInstance: func() error {
			code, err := runIncus([]string{"delete", instance}, nil, nil, nil, deleteInstanceTimeout)
			if err != nil || code != 0 {
				return fmt.Errorf("incus delete refused")
			}
			return nil
		},
		volumePresent: func() (bool, error) {
			return workspaceVolumePresent(pool, volume)
		},
		volumeMetadata: func() (bool, error) {
			return workspaceVolumeMetadataValid(pool, volume, instance)
		},
		deleteVolume: func() error {
			code, err := incusExec([]string{"storage", "volume", "delete", pool, volume}, nil, nil, nil, deleteInstanceTimeout)
			if err != nil || code != 0 {
				return fmt.Errorf("incus storage volume delete refused")
			}
			return nil
		},
		catalogPresent: func() (bool, error) {
			_, err := os.Stat(catalogFile(instance))
			if os.IsNotExist(err) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			return true, nil
		},
		removeCatalog: func() error {
			return os.Remove(catalogFile(instance))
		},
	}
}

// workspaceVolumeMetadataValid proves that a derived volume has the
// same ownership markers required by launch-sandbox-vm.sh before deleting it.
func workspaceVolumeMetadataValid(pool, volume, instance string) (bool, error) {
	return workspaceVolumeMetadataValidWith(incusQueryGET, pool, volume, instance)
}

func workspaceVolumeMetadataValidWith(query func(string) (map[string]interface{}, error), pool, volume, instance string) (bool, error) {
	details, err := query("/1.0/storage-pools/" + pool + "/volumes/custom/" + volume)
	if err != nil || details == nil {
		return false, fmt.Errorf("storage volume details unavailable")
	}
	if details["type"] != "custom" || details["content_type"] != "filesystem" {
		return false, nil
	}
	config, ok := details["config"].(map[string]interface{})
	if !ok {
		return false, nil
	}
	for _, expected := range []struct{ field, value string }{
		{"protected", "true"}, {"scope", "workspace"}, {"owner", instance},
	} {
		if config[siteconfig.Active().MetadataKey(expected.field)] != expected.value {
			return false, nil
		}
	}
	return true, nil
}

// workspaceVolumePresent inventories custom volumes in the site's storage
// pool and reports whether the named workspace volume exists. The global
// shared volume is never a candidate: callers only ever pass an
// instance-derived workspace volume name (<prefix>-workspace-<instance>).
func workspaceVolumePresent(pool, volume string) (bool, error) {
	buffer := &boundedBuffer{limit: volumeInventoryLimit}
	code, err := incusExec([]string{"storage", "volume", "list", pool, "--format", "csv"}, nil, buffer, nil, 30*time.Second)
	if err != nil || code != 0 || buffer.exceeded {
		return false, fmt.Errorf("storage volume inventory failed")
	}
	for _, line := range strings.Split(strings.TrimSpace(string(buffer.Bytes())), "\n") {
		parts := strings.Split(line, ",")
		if len(parts) >= 2 && parts[0] == "custom" && parts[1] == volume {
			return true, nil
		}
	}
	return false, nil
}
