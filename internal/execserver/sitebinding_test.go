package execserver

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/rahulrajaram/chasm/internal/siteconfig"
	"github.com/rahulrajaram/chasm/internal/testsupport"
)

// TestMain binds the neutral testsite descriptor before any test in this
// package runs. Tests that address the default guest rebind a site that
// declares a default_sandbox.
func TestMain(m *testing.M) {
	path, cleanup, err := testsupport.TempSiteConfig(filepath.Join(moduleRoot(), "testdata", "fixtures", "testsite", "site.conf"))
	if err != nil {
		panic(err)
	}
	if err := siteconfig.Init(path); err != nil {
		panic(fmt.Sprintf("bind testsite in TestMain: %v", err))
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func testSitePath(t *testing.T, site string) string {
	t.Helper()
	path, err := testsupport.CopySiteConfig(filepath.Join(moduleRoot(), "testdata", "fixtures", site, "site.conf"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// mustBindTestSite rebinds the plain fixture (no default_sandbox) for tests
// that exercise the fail-closed paths, since the helpers below leave a
// different site bound.
func mustBindTestSite(t *testing.T) {
	t.Helper()
	if err := siteconfig.Init(testSitePath(t, "testsite")); err != nil {
		t.Fatal(err)
	}
}

func mustBindGuestSite(t *testing.T) {
	t.Helper()
	site, err := siteconfig.Load(testSitePath(t, "testsite"))
	if err != nil {
		t.Fatal(err)
	}
	site.DefaultSandbox = "demo"
	if err := siteconfig.Set(site); err != nil {
		t.Fatal(err)
	}
}
