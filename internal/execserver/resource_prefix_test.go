package execserver

import (
	"errors"
	"testing"

	"github.com/rahulrajaram/chasm/internal/protocol"
	"github.com/rahulrajaram/chasm/internal/siteconfig"
)

// This file proves the resource_prefix contract: with
// resource_prefix=agent-sandbox the executor's inventory, start admission,
// delete, and description paths operate on instances carrying
// user.agent-sandbox.* metadata and agent-sandbox-* volume names, and treat
// instances carrying user.chasm.* metadata as foreign (ignored or refused) —
// and vice versa with the default prefix.

// mustBindPrefixedGuestSite binds the agent-sandbox-prefixed fixture with a
// default sandbox, mirroring mustBindGuestSite.
func mustBindPrefixedGuestSite(t *testing.T) {
	t.Helper()
	site, err := siteconfig.Load(testSitePath(t, "testsite-prefixed"))
	if err != nil {
		t.Fatal(err)
	}
	site.DefaultSandbox = "demo"
	if err := siteconfig.Set(site); err != nil {
		t.Fatal(err)
	}
}

// managedSummaryForSite builds a fully managed instance summary whose
// metadata keys and volume names derive from the bound site.
func managedSummaryForSite(name, status string) incusInstanceSummary {
	site := siteconfig.Active()
	return incusInstanceSummary{
		Name: name, Status: status,
		Config: map[string]string{
			site.MetadataKey("managed"):          "true",
			site.MetadataKey("logical-name"):     logicalSandboxName(name),
			site.MetadataKey("workspace-volume"): site.WorkspaceVolumeName(name),
			site.MetadataKey("shared-volume"):    site.SharedVolumeName(),
		},
		Devices: map[string]map[string]string{
			"shared":    {"type": "disk", "pool": site.StoragePool, "source": site.SharedVolumeName(), "path": sharedPath, "readonly": "false"},
			"workspace": {"type": "disk", "pool": site.StoragePool, "source": site.WorkspaceVolumeName(name), "path": workspacePath, "readonly": "false"},
		},
	}
}

// agentSandboxLifecycleSummary is the canonical foreign instance for the
// default-prefix site: hardcoded agent-sandbox derivation, mirroring
// managedLifecycleSummary's hardcoded chasm one.
func agentSandboxLifecycleSummary(name, status string) incusInstanceSummary {
	return incusInstanceSummary{
		Name: name, Status: status,
		Config: map[string]string{
			"user.agent-sandbox.managed":          "true",
			"user.agent-sandbox.logical-name":     logicalSandboxName(name),
			"user.agent-sandbox.workspace-volume": "agent-sandbox-workspace-" + name,
			"user.agent-sandbox.shared-volume":    "agent-sandbox-shared",
		},
		Devices: map[string]map[string]string{
			"shared":    {"type": "disk", "pool": "chasm-test", "source": "agent-sandbox-shared", "path": sharedPath, "readonly": "false"},
			"workspace": {"type": "disk", "pool": "chasm-test", "source": "agent-sandbox-workspace-" + name, "path": workspacePath, "readonly": "false"},
		},
	}
}

func fakeStateFor(name string) (map[string]interface{}, error) {
	return map[string]interface{}{"status": "Running", "pid": float64(4242)}, nil
}

func TestPrefixedInventoryListsOwnInstancesAndIgnoresForeignOnes(t *testing.T) {
	mustBindPrefixedGuestSite(t)
	site := siteconfig.Active()
	if site.MetadataKey("managed") != "user.agent-sandbox.managed" {
		t.Fatalf("fixture did not bind the agent-sandbox prefix: %q", site.ResourcePrefix)
	}
	own := managedSummaryForSite("sandbox-demo", "Running")
	foreign := managedLifecycleSummary("sandbox-legacy", "Running")
	foreign.Config["user.chasm.workspace-volume"] = "chasm-workspace-sandbox-legacy"

	instances, err := sandboxInstancesFromSummaries([]incusInstanceSummary{own, foreign}, fakeStateFor)
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 1 || instances[0].Name != "sandbox-demo" {
		t.Fatalf("prefixed inventory must list only the prefixed instance, got %v", instances)
	}

	// And vice versa: under the default prefix the agent-sandbox-keyed
	// instance is the foreign one.
	mustBindGuestSite(t)
	defaultSite := siteconfig.Active()
	if defaultSite.ResourcePrefix != "chasm" {
		t.Fatalf("expected the default chasm prefix, got %q", defaultSite.ResourcePrefix)
	}
	ownDefault := managedSummaryForSite("sandbox-demo", "Running")
	foreignPrefixed := agentSandboxLifecycleSummary("sandbox-prefixed", "Running")
	instances, err = sandboxInstancesFromSummaries([]incusInstanceSummary{ownDefault, foreignPrefixed}, fakeStateFor)
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 1 || instances[0].Name != "sandbox-demo" {
		t.Fatalf("default inventory must list only the chasm-keyed instance, got %v", instances)
	}
}

func TestPrefixedStartAdmissionAcceptsOwnAndRefusesForeignRunning(t *testing.T) {
	const memTotalMiB = 8192
	mustBindPrefixedGuestSite(t)

	// The prefixed target is admissible with prefixed metadata.
	status, err := admitSandboxStartWith("sandbox-demo", []incusInstanceSummary{managedSummaryForSite("sandbox-demo", "Stopped")}, memTotalMiB)
	if err != nil || status != "Stopped" {
		t.Fatalf("prefixed target: status=%q err=%v", status, err)
	}

	// A running instance carrying the OTHER prefix's metadata is not a
	// managed peer: admission must fail closed instead of counting it.
	_, err = admitSandboxStartWith("sandbox-demo",
		[]incusInstanceSummary{managedSummaryForSite("sandbox-demo", "Stopped"), managedLifecycleSummary("sandbox-legacy", "Running")},
		memTotalMiB)
	if err == nil {
		t.Fatal("a foreign-keyed running instance was admitted as managed")
	}
	var protocolErr *ProtocolError
	if !errors.As(err, &protocolErr) || protocolErr.Code != "unsafe_state" {
		t.Fatalf("err=%v is not an unsafe_state refusal", err)
	}
}

func TestPrefixedDeleteTearsDownOwnAndRefusesForeign(t *testing.T) {
	mustBindPrefixedGuestSite(t)

	presentInstances := []incusInstanceSummary{managedSummaryForSite("sandbox-demo", "Stopped")}
	volumeStillThere := true
	deleted := []string{}
	environment := deleteSandboxEnvironment{
		inventory: func() ([]incusInstanceSummary, error) {
			return append([]incusInstanceSummary{}, presentInstances...), nil
		},
		deleteInstance: func() error {
			deleted = append(deleted, "instance")
			presentInstances = nil
			return nil
		},
		volumePresent:  func() (bool, error) { return volumeStillThere, nil },
		volumeMetadata: func() (bool, error) { return true, nil },
		deleteVolume: func() error {
			deleted = append(deleted, "volume")
			volumeStillThere = false
			return nil
		},
		catalogPresent: func() (bool, error) { return false, nil },
		removeCatalog:  func() error { return nil },
	}
	result, err := deleteSandboxWith("sandbox-demo", environment)
	if err != nil {
		t.Fatal(err)
	}
	if result["instance_deleted"] != true || result["workspace_volume_deleted"] != true {
		t.Fatalf("prefixed delete did not tear down the instance and volume: %v", result)
	}

	// The same instance name carrying the other prefix's managed marker is
	// refused as not managed by this site.
	foreign := managedLifecycleSummary("sandbox-demo", "Stopped")
	foreignInventory := []incusInstanceSummary{foreign}
	environment.inventory = func() ([]incusInstanceSummary, error) { return foreignInventory, nil }
	if _, err := deleteSandboxWith("sandbox-demo", environment); err == nil {
		t.Fatal("a foreign-keyed instance was deleted by the prefixed site")
	}
}

func TestPrefixedDescriptionMetadataNamespace(t *testing.T) {
	mustBindPrefixedGuestSite(t)
	site := siteconfig.Active()

	// The description is read and written through the derived key.
	metadataFor := func(description string) map[string]interface{} {
		return map[string]interface{}{
			"config": map[string]interface{}{site.MetadataKey("description"): description},
		}
	}
	descriptionNow := ""
	result, err := setSandboxDescriptionWith("sandbox-demo", "prefixed note", func(string) (map[string]interface{}, error) {
		return metadataFor(descriptionNow), nil
	}, func(instance, description string) error {
		if description != "prefixed note" {
			t.Fatalf("mutate received %q", description)
		}
		descriptionNow = description
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result["description"] != "prefixed note" || result["changed"] != true {
		t.Fatalf("result=%v", result)
	}

	// A description stored under the other prefix's key is invisible here.
	foreign := map[string]interface{}{
		"config": map[string]interface{}{"user.chasm.description": "legacy note"},
	}
	description, err := sandboxDescriptionFromMetadata(foreign)
	if err != nil {
		t.Fatal(err)
	}
	if description != "" {
		t.Fatalf("foreign-prefix description leaked through: %q", description)
	}
}

func TestPrefixedBudgetMetadataKey(t *testing.T) {
	mustBindPrefixedGuestSite(t)
	if got := budgetMetadataKey(); got != "user.agent-sandbox.budget-memory-mib" {
		t.Fatalf("budget key=%q", got)
	}
	budget, err := sandboxBudgetFromConfig(map[string]string{"user.agent-sandbox.budget-memory-mib": "2048"})
	if err != nil || budget != 2048 {
		t.Fatalf("budget=%d err=%v", budget, err)
	}
	// The default-prefix key is not read under the prefixed site.
	budget, err = sandboxBudgetFromConfig(map[string]string{"user.chasm.budget-memory-mib": "4096"})
	if err != nil || budget != protocol.DefaultSandboxBudgetMemoryMiB {
		t.Fatalf("foreign budget key must fall back to the default, got %d err=%v", budget, err)
	}
}
