package incuslocal

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestValidateSocketRequiresAbsoluteExistingSocket(t *testing.T) {
	previous := inspectSocket
	inspectSocket = func(path string) (os.FileMode, error) {
		if path == "/tmp/incus.sock" {
			return os.ModeSocket | 0600, nil
		}
		return 0, errors.New("missing")
	}
	t.Cleanup(func() { inspectSocket = previous })
	path := "/tmp/incus.sock"
	if got, err := validateSocket(path); err != nil || got != path {
		t.Fatalf("validateSocket() = %q, %v", got, err)
	}
	for _, invalid := range []string{"relative.sock", "/tmp/missing.sock", "/tmp/has space.sock"} {
		if _, err := validateSocket(invalid); err == nil {
			t.Errorf("validateSocket(%q) unexpectedly succeeded", invalid)
		}
	}
}

func TestSocketPathHonorsExplicitEnvironmentWithoutFallback(t *testing.T) {
	previous := inspectSocket
	inspectSocket = func(path string) (os.FileMode, error) {
		if path == "/tmp/incus.sock" {
			return os.ModeSocket | 0600, nil
		}
		return 0, errors.New("missing")
	}
	t.Cleanup(func() { inspectSocket = previous })
	path := "/tmp/incus.sock"
	t.Setenv("INCUS_SOCKET", path)
	got, err := SocketPath()
	if err != nil || got != path {
		t.Fatalf("SocketPath() = %q, %v", got, err)
	}
	t.Setenv("INCUS_SOCKET", "/tmp/missing.sock")
	if _, err := SocketPath(); err == nil {
		t.Fatal("invalid explicit override silently fell back")
	}
}

func TestReadSocketConfigRequiresPrivateRootOwnedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "incus-socket")
	if err := os.WriteFile(path, []byte("/run/incus/unix.socket\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 {
		if _, err := readSocketConfig(path); err == nil {
			t.Fatal("non-root-owned config was accepted")
		}
		return
	}
	if got, err := readSocketConfig(path); err != nil || got != "/run/incus/unix.socket" {
		t.Fatalf("readSocketConfig() = %q, %v", got, err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readSocketConfig(path); err == nil {
		t.Fatal("world-readable socket config was accepted")
	}
}
