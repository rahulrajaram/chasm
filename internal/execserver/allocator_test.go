package execserver

import (
	"fmt"
	"io"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/rahulrajaram/chasm/internal/siteconfig"
)

// The allocator must never hand out an address the product derives statically,
// because an instance without an explicit eth0 override holds the profile's
// fallback address, and the sealed-template builder holds its own. Handing the
// same address to a managed sandbox would collide with it. It must also honor
// every claim source the launcher enforces (eth0 override,
// user.<prefix>.instance-ipv4 metadata, live address of a RUNNING/FROZEN
// instance), so a create is never refused repeatedly on an address the
// launcher will not accept.

func TestAllocatorReservesStaticallyDerivedAddresses(t *testing.T) {
	site := siteconfig.Active()

	first, err := selectSandboxIPv4(site, map[string]bool{})
	if err != nil {
		t.Fatalf("empty claim set: %v", err)
	}
	if want := site.SandboxIPv4(2); first != want {
		t.Fatalf("first allocation = %q, want %q", first, want)
	}

	// Claim every address up to the profile fallback: the next allocation must
	// skip .10 (profile fallback) and .11 (template builder).
	claimed := map[string]bool{}
	for last := 2; last <= 9; last++ {
		claimed[site.SandboxIPv4(last)] = true
	}
	next, err := selectSandboxIPv4(site, claimed)
	if err != nil {
		t.Fatalf("claimed set: %v", err)
	}
	if want := site.SandboxIPv4(12); next != want {
		t.Fatalf("allocation after .2-.9 = %q, want %q (the profile fallback and builder addresses are reserved)", next, want)
	}

	alternate := site
	alternate.BridgeIPv4 = site.SandboxIPv4(2) + "/24"
	if got, err := selectSandboxIPv4(alternate, nil); err != nil || got != site.SandboxIPv4(3) {
		t.Fatalf("allocation with gateway at .2 = %q, %v; want .3", got, err)
	}

	exhausted := map[string]bool{}
	for last := 2; last <= 254; last++ {
		exhausted[site.SandboxIPv4(last)] = true
	}
	if _, err := selectSandboxIPv4(site, exhausted); err == nil {
		t.Fatal("an exhausted pool must fail closed")
	}
}

// parseSandboxAddressInventory must read the inventory Incus actually renders:
// an instance with several IPv4 addresses carries them newline-joined inside
// ONE quoted CSV field, so the record spans physical lines and a line-based
// parser would drop every address after the first.
func TestParseSandboxAddressInventoryReadsQuotedMultiLineField(t *testing.T) {
	inventory := "sandbox-stopped,Stopped,\n" +
		"sandbox-development,Running,\"172.17.0.1 (docker0)\n10.42.0.6 (enp5s0)\"\n"
	claims, err := parseSandboxAddressInventory(inventory)
	if err != nil {
		t.Fatalf("parse quoted multi-line inventory: %v", err)
	}
	if len(claims) != 2 {
		t.Fatalf("claims = %d records, want 2", len(claims))
	}
	stopped := claims[0]
	if stopped.name != "sandbox-stopped" || stopped.status != "Stopped" || len(stopped.live) != 0 {
		t.Fatalf("stopped record = %+v, want an addressless Stopped instance", stopped)
	}
	multi := claims[1]
	if multi.name != "sandbox-development" || multi.status != "Running" {
		t.Fatalf("multi-address record = %+v, want sandbox-development Running", multi)
	}
	want := []string{"172.17.0.1", "10.42.0.6"}
	if !reflect.DeepEqual(multi.live, want) {
		t.Fatalf("live addresses = %v, want %v (both addresses, annotations stripped)", multi.live, want)
	}
	if err != nil || len(collectSandboxIPv4Claims(claims, "")) != 2 {
		t.Fatalf("live claims = %v, want exactly the two live addresses", collectSandboxIPv4Claims(claims, ""))
	}

	if _, err := parseSandboxAddressInventory("sandbox-x,Running,\"172.17.0.1 (docker0)"); err == nil {
		t.Fatal("an unterminated quoted field must fail closed")
	}
}

// The launcher refuses an address claimed through user.<prefix>.instance-ipv4
// metadata even when the instance carries no eth0 override, so the allocator
// must skip that address too — otherwise every create is refused on the same
// candidate and retries never advance.
func TestAllocatorSkipsMetadataClaimWithoutOverride(t *testing.T) {
	site := siteconfig.Active()
	claims := []sandboxAddressClaim{
		// Renumbered through its override alone: the stale metadata still
		// records .2, and the launcher refuses on it.
		{name: "sandbox-renumbered", status: "Stopped", override: site.SandboxIPv4(3), metadata: site.SandboxIPv4(2)},
	}
	address, err := selectSandboxIPv4(site, collectSandboxIPv4Claims(claims, ""))
	if err != nil {
		t.Fatalf("select with a metadata claim: %v", err)
	}
	if want := site.SandboxIPv4(4); address != want {
		t.Fatalf("allocation = %q, want %q (the metadata-claimed .2 and the override .3 are both taken)", address, want)
	}
}

// The launcher refuses an address a RUNNING instance holds as a live address
// even when neither an override nor metadata records it, so the allocator
// must skip that address too.
func TestAllocatorSkipsLiveOnlyClaim(t *testing.T) {
	site := siteconfig.Active()

	running := []sandboxAddressClaim{
		{name: "sandbox-docker", status: "Running", live: []string{"172.17.0.1", site.SandboxIPv4(2)}},
	}
	address, err := selectSandboxIPv4(site, collectSandboxIPv4Claims(running, ""))
	if err != nil {
		t.Fatalf("select with a live claim: %v", err)
	}
	if want := site.SandboxIPv4(3); address != want {
		t.Fatalf("allocation = %q, want %q (the live-claimed .2 must be skipped)", address, want)
	}

	frozen := []sandboxAddressClaim{
		{name: "sandbox-paused", status: "Frozen", live: []string{site.SandboxIPv4(2)}},
	}
	if address, err = selectSandboxIPv4(site, collectSandboxIPv4Claims(frozen, "")); err != nil || address != site.SandboxIPv4(3) {
		t.Fatalf("allocation = %q err=%v, want %q (a FROZEN instance claims its live address)", address, err, site.SandboxIPv4(3))
	}

	stopped := []sandboxAddressClaim{
		{name: "sandbox-stale", status: "Stopped", live: []string{site.SandboxIPv4(2)}},
	}
	if address, err = selectSandboxIPv4(site, collectSandboxIPv4Claims(stopped, "")); err != nil || address != site.SandboxIPv4(2) {
		t.Fatalf("allocation = %q err=%v, want %q (a STOPPED instance's stale live column claims nothing)", address, err, site.SandboxIPv4(2))
	}

	if address, err = selectSandboxIPv4(site, collectSandboxIPv4Claims(running, "sandbox-docker")); err != nil || address != site.SandboxIPv4(2) {
		t.Fatalf("allocation = %q err=%v, want %q (the excluded instance's own live claim is skipped)", address, err, site.SandboxIPv4(2))
	}
}

// allocateSandboxIPv4 must gather every claim source the launcher enforces
// through the exact production invocations, so a create can never be handed
// an address the launcher will refuse. The fake rejects any argv other than
// the three production calls, so a fail-open mutation of the claim set
// cannot pass silently.
func TestAllocateSandboxIPv4HonorsEveryLauncherClaimSource(t *testing.T) {
	site := siteconfig.Active()
	metadataKey := site.MetadataKey(instanceIPv4MetadataField)

	original := runIncus
	t.Cleanup(func() { runIncus = original })
	runIncus = func(arguments []string, _ io.Reader, stdout io.Writer, _ []*os.File, _ time.Duration) (int, error) {
		switch {
		case reflect.DeepEqual(arguments, []string{"list", "--format", "csv", "-c", "ns4"}):
			// .2 is claimed ONLY through the metadata source here: no
			// override and no live address anywhere holds it, so dropping
			// the metadata source hands the address to a create and the
			// launcher then refuses it.
			fmt.Fprint(stdout, "sandbox-renumbered,Stopped,\n"+
				"sandbox-docker,Running,\"172.17.0.1 (docker0)\n"+site.SandboxIPv4(7)+" (enp5s0)\"\n")
		case len(arguments) == 6 &&
			arguments[0] == "config" && arguments[1] == "device" && arguments[2] == "get" &&
			arguments[4] == "eth0" && arguments[5] == "ipv4.address":
			// No instance carries an eth0 override in this inventory.
		case len(arguments) == 4 &&
			arguments[0] == "config" && arguments[1] == "get" && arguments[3] == metadataKey:
			if arguments[2] == "sandbox-renumbered" {
				fmt.Fprintln(stdout, site.SandboxIPv4(2))
			}
		default:
			t.Fatalf("allocator issued a non-production incus invocation: %v", arguments)
		}
		return 0, nil
	}

	address, err := allocateSandboxIPv4("sandbox-new")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if want := site.SandboxIPv4(3); address != want {
		t.Fatalf("allocation = %q, want %q (.2 is claimed only through the metadata source)", address, want)
	}
}
