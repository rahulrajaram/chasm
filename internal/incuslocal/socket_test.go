package incuslocal

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestValidateSocketRequiresAbsoluteExistingSocket(t *testing.T) {
	previous := inspectSocket
	previousTrust := socketPathTrusted
	socketPathTrusted = func(string) bool { return true }
	inspectSocket = func(path string) (os.FileMode, error) {
		if path == "/tmp/incus.sock" {
			return os.ModeSocket | 0600, nil
		}
		return 0, errors.New("missing")
	}
	t.Cleanup(func() { inspectSocket = previous; socketPathTrusted = previousTrust })
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
	previousTrust := socketPathTrusted
	socketPathTrusted = func(string) bool { return true }
	inspectSocket = func(path string) (os.FileMode, error) {
		if path == "/tmp/incus.sock" {
			return os.ModeSocket | 0600, nil
		}
		return 0, errors.New("missing")
	}
	t.Cleanup(func() { inspectSocket = previous; socketPathTrusted = previousTrust })
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

func TestConfiguredSocketPathIgnoresEnvironment(t *testing.T) {
	t.Setenv("INCUS_SOCKET", "/tmp/operator.sock")
	previous := inspectSocket
	previousTrust := socketPathTrusted
	previousConfig := configuredSocketFile
	configuredSocketFile = filepath.Join(t.TempDir(), "missing")
	inspectSocket = func(path string) (os.FileMode, error) {
		if path == SystemSocketPath {
			return os.ModeSocket | 0600, nil
		}
		return 0, os.ErrNotExist
	}
	socketPathTrusted = func(path string) bool { return path == SystemSocketPath }
	t.Cleanup(func() {
		inspectSocket = previous
		socketPathTrusted = previousTrust
		configuredSocketFile = previousConfig
	})
	if got, err := ConfiguredSocketPath(); err != nil || got != SystemSocketPath {
		t.Fatalf("configured socket = %q, %v", got, err)
	}
}

type socketInfo struct {
	mode os.FileMode
	uid  uint32
}

func (info socketInfo) Name() string       { return "test" }
func (info socketInfo) Size() int64        { return 0 }
func (info socketInfo) Mode() os.FileMode  { return info.mode }
func (info socketInfo) ModTime() time.Time { return time.Time{} }
func (info socketInfo) IsDir() bool        { return info.mode.IsDir() }
func (info socketInfo) Sys() any           { return &syscall.Stat_t{Uid: info.uid} }

func TestTrustedSocketPathChecksSocketAndEveryAncestor(t *testing.T) {
	for _, test := range []struct {
		name string
		path string
		info socketInfo
	}{
		{"trusted", "", socketInfo{}},
		{"operator socket", "/run/incus/unix.socket", socketInfo{os.ModeSocket | 0600, 1000}},
		{"writable parent", "/run/incus", socketInfo{os.ModeDir | 0777, 0}},
		{"operator parent", "/run", socketInfo{os.ModeDir | 0755, 1000}},
	} {
		t.Run(test.name, func(t *testing.T) {
			lookup := func(path string) (os.FileInfo, error) {
				if path == test.path && test.path != "" {
					return test.info, nil
				}
				if path == "/run/incus/unix.socket" {
					return socketInfo{os.ModeSocket | 0600, 0}, nil
				}
				return socketInfo{os.ModeDir | 0755, 0}, nil
			}
			got := TrustedSocketPath("/run/incus/unix.socket", lookup)
			if got != (test.path == "") {
				t.Fatalf("trusted = %v", got)
			}
		})
	}
}

func TestIncusPeerCredentialsAreChecked(t *testing.T) {
	for _, test := range []struct {
		name        string
		credentials *syscall.Ucred
		err         error
		want        bool
	}{
		{"root", &syscall.Ucred{Uid: 0}, nil, true},
		{"operator", &syscall.Ucred{Uid: 1000}, nil, false},
		{"missing", nil, nil, false},
		{"syscall failure", nil, syscall.EPERM, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := peerIsUID(test.credentials, test.err, 0); got != test.want {
				t.Fatalf("root credential verdict = %v", got)
			}
		})
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
