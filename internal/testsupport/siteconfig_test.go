package testsupport

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rahulrajaram/chasm/internal/siteconfig"
)

func TestCopySiteConfigIgnoresSourceMode(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", "testsite", "site.conf"))
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "site.conf")
	if err := os.WriteFile(source, fixture, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(source, 0o664); err != nil {
		t.Fatal(err)
	}
	copyPath, err := CopySiteConfig(source, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("copied site mode = %03o, want 600", got)
	}
	if _, err := siteconfig.Load(copyPath); err != nil {
		t.Fatalf("load copied site: %v", err)
	}
}
