package execserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/rahulrajaram/chasm/internal/protocol"
)

const maxGuestExecAuditBytes = 64 << 20

// jsonlAudit returns an AuditFunc mirroring _append_audit: every request
// disposition is appended to a root-owned 0600 JSONL file under a
// root-owned 0700 directory, schema (at / peer_uid / request_id /
// operation / ok / error_code).
func JSONLAudit(dir string, file string) AuditFunc {
	return jsonlAuditWithClock(dir, file, 0, time.Now)
}

func jsonlAuditWithClock(dir string, file string, expectedUID uint32, now func() time.Time) AuditFunc {
	return func(request *protocol.Request, peerUID int, ok bool, errorCode string) error {
		encoded, err := encodeAuditRecord(request, peerUID, ok, errorCode, now())
		if err != nil {
			return fmt.Errorf("encode audit record: %w", err)
		}
		return appendAuditRecord(dir, file, expectedUID, encoded)
	}
}

// AppendGuestExecAudit stores a redacted foreground execution record in its
// own file under the executor's validated audit directory.
func AppendGuestExecAudit(record any) error {
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return appendAuditRecord(auditRoot, "exec-audit.jsonl", 0, append(encoded, '\n'))
}

func appendAuditRecord(dir, file string, expectedUID uint32, encoded []byte) error {
	createdDirectory := false
	if err := os.Mkdir(dir, 0o700); err != nil {
		if !os.IsExist(err) {
			return fmt.Errorf("create audit directory: %w", err)
		}
	} else {
		createdDirectory = true
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("set audit directory mode: %w", err)
		}
	}
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 || !ownedBy(info, expectedUID) {
		return fmt.Errorf("audit directory metadata is unsafe")
	}
	if createdDirectory {
		if err := syncAuditDirectory(dir); err != nil {
			return err
		}
		if err := syncAuditDirectory(filepath.Dir(dir)); err != nil {
			return err
		}
	}
	path := filepath.Join(dir, file)
	_, statErr := os.Lstat(path)
	createdFile := os.IsNotExist(statErr)
	if statErr != nil && !createdFile {
		return fmt.Errorf("inspect audit file: %w", statErr)
	}
	flags := os.O_WRONLY | os.O_APPEND | os.O_CREATE | syscall.O_CLOEXEC | syscall.O_NOFOLLOW
	descriptor, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return fmt.Errorf("open audit file: %w", err)
	}
	if file == "exec-audit.jsonl" {
		if err := syscall.Flock(int(descriptor.Fd()), syscall.LOCK_EX); err != nil {
			_ = descriptor.Close()
			return fmt.Errorf("lock exec audit file: %w", err)
		}
	}
	info, err := descriptor.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !ownedBy(info, expectedUID) {
		_ = descriptor.Close()
		return fmt.Errorf("audit file metadata is unsafe")
	}
	if file == "exec-audit.jsonl" && info.Size() > maxGuestExecAuditBytes-int64(len(encoded)) {
		_ = descriptor.Close()
		return fmt.Errorf("exec audit file reached its 64 MiB cap")
	}
	written, writeErr := descriptor.Write(encoded)
	if writeErr != nil || written != len(encoded) {
		_ = descriptor.Close()
		return fmt.Errorf("append audit record: %w", firstAuditError(writeErr, errors.New("short audit write")))
	}
	if err := descriptor.Sync(); err != nil {
		_ = descriptor.Close()
		return fmt.Errorf("sync audit record: %w", err)
	}
	if err := descriptor.Close(); err != nil {
		return fmt.Errorf("close audit record: %w", err)
	}
	if createdFile {
		if err := syncAuditDirectory(dir); err != nil {
			return err
		}
	}
	return nil
}

func syncAuditDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open audit directory for sync: %w", err)
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return fmt.Errorf("sync audit directory: %w", err)
	}
	if err := directory.Close(); err != nil {
		return fmt.Errorf("close audit directory: %w", err)
	}
	return nil
}

func firstAuditError(primary, fallback error) error {
	if primary != nil {
		return primary
	}
	return fallback
}

func encodeAuditRecord(request *protocol.Request, peerUID int, ok bool, errorCode string, at time.Time) ([]byte, error) {
	var deliveredError interface{}
	if errorCode != "" {
		deliveredError = errorCode
	}
	record := map[string]interface{}{
		"at":         pythonISOTime(at),
		"peer_uid":   peerUID,
		"request_id": request.RequestID(),
		"operation":  request.Operation,
		"ok":         ok,
		"error_code": deliveredError,
	}
	if instance := request.Instance(); instance != "" {
		record["instance"] = instance
	}
	if len(request.LifecycleTimings) > 0 {
		record["phase_timings_ms"] = request.LifecycleTimings
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

func pythonISOTime(value time.Time) string {
	utc := value.UTC().Truncate(time.Microsecond)
	stamp := utc.Format("2006-01-02T15:04:05")
	if microseconds := utc.Nanosecond() / int(time.Microsecond); microseconds != 0 {
		stamp += fmt.Sprintf(".%06d", microseconds)
	}
	return stamp + "+00:00"
}

func ownedBy(info os.FileInfo, expectedUID uint32) bool {
	metadata, ok := info.Sys().(*syscall.Stat_t)
	return ok && metadata.Uid == expectedUID
}

// AuditRootDir returns the executor audit directory used by JSONLAudit.
func AuditRootDir() string { return auditRoot }

// AuditFileName returns the executor audit JSONL file name.
func AuditFileName() string { return auditFileName }
