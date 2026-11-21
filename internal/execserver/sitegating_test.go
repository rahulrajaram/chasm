package execserver

import (
	"strings"
	"testing"

	"github.com/rahulrajaram/chasm/internal/siteconfig"
)

// The tests in this file pin the fail-closed behaviour the site gate requires:
// a site that does not enable a subsystem must refuse the operations that need
// it, naming the descriptor key that would enable it, rather than falling back
// to a historical default.

func TestGuestInstanceFailsClosedWithoutADefaultSandbox(t *testing.T) {
	mustBindTestSite(t)

	_, err := guestInstance()
	if err == nil {
		t.Fatal("a site without default_sandbox still resolved a guest instance")
	}
	protocolErr, ok := err.(*ProtocolError)
	if !ok {
		t.Fatalf("error %v is not a protocol error", err)
	}
	if protocolErr.Code != "invalid_state" {
		t.Fatalf("code = %q, want invalid_state", protocolErr.Code)
	}
	if !strings.Contains(protocolErr.Message, "default_sandbox") {
		t.Fatalf("message %q does not name the missing descriptor key", protocolErr.Message)
	}

	// The same site with the key declared resolves the derived instance.
	mustBindGuestSite(t)
	instance, err := guestInstance()
	if err != nil {
		t.Fatalf("declared default sandbox was refused: %v", err)
	}
	if instance != "sandbox-demo" {
		t.Fatalf("instance = %q, want sandbox-demo", instance)
	}
}

func TestFirewallServiceIsAlwaysReported(t *testing.T) {
	mustBindTestSite(t)
	found := false
	for _, service := range serviceNamesFor(siteconfig.Active()) {
		if service == "chasm-firewall.service" {
			found = true
		}
	}
	if !found {
		t.Fatal("the firewall service is missing from the supervised service list")
	}
}
