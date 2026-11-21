// Package testsupport contains fixture helpers shared by Go tests.
package testsupport

import (
	"os"
	"path/filepath"
)

// CopySiteConfig copies a checked-in descriptor to a private test directory.
// Its mode is independent of the checkout's file mode and the caller's umask.
func CopySiteConfig(source, directory string) (string, error) {
	content, err := os.ReadFile(source)
	if err != nil {
		return "", err
	}
	path := filepath.Join(directory, "site.conf")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// TempSiteConfig is for TestMain, which has no testing.TB or TempDir.
func TempSiteConfig(source string) (string, func(), error) {
	directory, err := os.MkdirTemp("", "chasm-site-test-")
	if err != nil {
		return "", nil, err
	}
	path, err := CopySiteConfig(source, directory)
	if err != nil {
		_ = os.RemoveAll(directory)
		return "", nil, err
	}
	return path, func() { _ = os.RemoveAll(directory) }, nil
}
