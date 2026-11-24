// Package incuslocal resolves only the local Incus Unix socket. It never
// consults Incus remote configuration or chooses a network endpoint.
package incuslocal

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	SocketConfigPath = "/etc/chasm/incus-socket"
	SystemSocketPath = "/var/lib/incus/unix.socket"
	RunSocketPath    = "/run/incus/unix.socket"
)

// inspectSocket is a narrow filesystem seam for unprivileged unit tests.
var inspectSocket = func(path string) (os.FileMode, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	return info.Mode(), nil
}

// SocketPath returns the configured local socket, preferring a non-empty
// INCUS_SOCKET override, then the root-owned installation setting, then the
// standard Incus paths. A configured but invalid path is an error; it never
// silently falls back to a different daemon.
func SocketPath() (string, error) {
	if value := os.Getenv("INCUS_SOCKET"); value != "" {
		return validateSocket(value)
	}
	if configured, err := readSocketConfig(SocketConfigPath); err != nil {
		return "", err
	} else if configured != "" {
		return validateSocket(configured)
	}
	for _, candidate := range []string{SystemSocketPath, RunSocketPath} {
		if _, err := validateSocket(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no local Incus socket found (tried %s and %s)", SystemSocketPath, RunSocketPath)
}

// ConfiguredSocketPath ignores the calling process environment. It is for the
// exact sudo helper, which must not inherit an operator-controlled socket.
func ConfiguredSocketPath() (string, error) {
	if configured, err := readSocketConfig(SocketConfigPath); err != nil {
		return "", err
	} else if configured != "" {
		return validateSocket(configured)
	}
	for _, candidate := range []string{SystemSocketPath, RunSocketPath} {
		if _, err := validateSocket(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", errors.New("no local Incus socket found")
}

func readSocketConfig(path string) (string, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("open Incus socket setting: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("inspect Incus socket setting: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return "", errors.New("Incus socket setting must be a regular root-owned mode-0600 file")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != 0 {
		return "", errors.New("Incus socket setting must be root-owned")
	}
	contents, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return "", fmt.Errorf("read Incus socket setting: %w", err)
	}
	if len(contents) > 4096 {
		return "", errors.New("Incus socket setting exceeds 4096 bytes")
	}
	value := strings.TrimSuffix(string(contents), "\n")
	if value == "" {
		return "", errors.New("Incus socket setting is empty")
	}
	if strings.ContainsAny(value, "\r\n") {
		return "", errors.New("Incus socket setting must contain exactly one path")
	}
	return value, nil
}

func validateSocket(path string) (string, error) {
	if len(path) > 107 || !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\r\n\t ") {
		return "", errors.New("Incus socket path must be an absolute, clean path without whitespace")
	}
	mode, err := inspectSocket(path)
	if err != nil {
		return "", fmt.Errorf("Incus socket %s is unavailable: %w", path, err)
	}
	if mode&os.ModeSocket == 0 {
		return "", fmt.Errorf("Incus socket path %s is not a Unix socket", path)
	}
	return path, nil
}
