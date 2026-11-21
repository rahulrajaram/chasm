// Package supervisor holds the supervisor service's state-root writer
// lease. The lease is host-kernel-enforced exclusivity for every mutation
// below one supervisor state root (the sealed-session store it used to
// guard is gone; performance sample writes remain).
package supervisor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const writerLeaseName = ".writer.lock"

// CodeError is a delivered supervisor disposition.
type CodeError struct {
	Code    string
	Message string
}

func (e CodeError) Error() string {
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}

func coded(code, message string) error { return CodeError{Code: code, Message: message} }

// ValidRoot reports whether a state root path is usable at all.
func ValidRoot(root string) error {
	if root == "" || strings.Contains(root, "\x00") {
		return coded("unsafe_state", "state root is invalid")
	}
	return nil
}

// WriterLease is a host-kernel-enforced exclusive lease for all mutations
// below one supervisor state root. The lock survives as private diagnostic
// evidence, while the kernel releases exclusivity if the holder exits.
type WriterLease struct {
	file *os.File
	once sync.Once
	err  error
}

func AcquireWriterLease(root string) (*WriterLease, error) {
	if err := ValidRoot(root); err != nil {
		return nil, err
	}
	if err := mkdirChecked(root, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(root, writerLeaseName)
	_, statErr := os.Lstat(path)
	created := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !created {
		return nil, coded("unsafe_state", "writer lease metadata is unavailable")
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, coded("unsafe_state", "writer lease open failed")
	}
	closeFailure := func(code, message string) (*WriterLease, error) {
		_ = file.Close()
		return nil, coded(code, message)
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !ownedByEffectiveUser(info) {
		return closeFailure("unsafe_state", "writer lease metadata is unsafe")
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return closeFailure("writer_busy", "another supervisor controller holds the state-root lease")
		}
		return closeFailure("unsafe_state", "writer lease acquisition failed")
	}
	evidence := fmt.Sprintf("pid=%d\nstarted_at=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339Nano))
	if err := file.Truncate(0); err != nil {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		return closeFailure("unsafe_state", "writer lease evidence truncate failed")
	}
	if _, err := file.Seek(0, 0); err != nil {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		return closeFailure("unsafe_state", "writer lease evidence seek failed")
	}
	written, writeErr := file.WriteString(evidence)
	if writeErr != nil || written != len(evidence) || file.Sync() != nil {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		return closeFailure("unsafe_state", "writer lease evidence persistence failed")
	}
	if created {
		if err := syncDirectory(root); err != nil {
			_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
			_ = file.Close()
			return nil, err
		}
	}
	return &WriterLease{file: file}, nil
}

func (lease *WriterLease) Close() error {
	if lease == nil {
		return nil
	}
	lease.once.Do(func() {
		if lease.file == nil {
			return
		}
		unlockErr := syscall.Flock(int(lease.file.Fd()), syscall.LOCK_UN)
		closeErr := lease.file.Close()
		if unlockErr != nil {
			lease.err = fmt.Errorf("release writer lease: %w", unlockErr)
		} else if closeErr != nil {
			lease.err = fmt.Errorf("close writer lease: %w", closeErr)
		}
	})
	return lease.err
}

func ownedByEffectiveUser(info os.FileInfo) bool {
	metadata, ok := info.Sys().(*syscall.Stat_t)
	return ok && metadata.Uid == uint32(os.Geteuid())
}

func mkdirChecked(path string, mode os.FileMode) error {
	created := false
	if err := os.Mkdir(path, mode); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return coded("unsafe_state", err.Error())
		}
	} else {
		created = true
		if err := os.Chmod(path, mode); err != nil {
			return coded("unsafe_state", err.Error())
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return coded("unsafe_state", err.Error())
	}
	if !info.IsDir() || info.Mode().Perm() != mode {
		return coded("unsafe_state", "directory metadata is invalid")
	}
	if created {
		if err := syncDirectory(path); err != nil {
			return err
		}
		if err := syncDirectory(filepath.Dir(path)); err != nil {
			return err
		}
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return coded("unsafe_state", "directory sync open failed")
	}
	info, statErr := directory.Stat()
	if statErr != nil || !info.IsDir() {
		_ = directory.Close()
		return coded("unsafe_state", "directory sync target is invalid")
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return coded("unsafe_state", "directory sync failed")
	}
	if err := directory.Close(); err != nil {
		return coded("unsafe_state", "directory sync close failed")
	}
	return nil
}
