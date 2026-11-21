package execserver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/rahulrajaram/chasm/internal/protocol"
)

const maxOutcomeBytes = protocol.MaxResponseBytes

type durableOutcome struct {
	SchemaVersion int                    `json:"schema_version"`
	RequestDigest string                 `json:"request_digest"`
	OK            bool                   `json:"ok"`
	Result        map[string]interface{} `json:"result,omitempty"`
	ErrorCode     string                 `json:"error_code,omitempty"`
	ErrorMessage  string                 `json:"error_message,omitempty"`
}

// OutcomeStore is the root-owned idempotency ledger for executor operations
// that may mutate host or guest state. One stable request identity can have
// exactly one durable disposition.
type OutcomeStore struct {
	Root        string
	ExpectedUID uint32
}

func NewOutcomeStore(root string) OutcomeStore { return OutcomeStore{Root: root, ExpectedUID: 0} }

func OutcomeRootDir() string { return filepath.Join(auditRoot, "outcomes") }

func (store OutcomeStore) load(request protocol.Request) (*durableOutcome, error) {
	path := filepath.Join(store.Root, request.RequestID()+".json")
	file, err := openPrivateOutcome(path, store.ExpectedUID)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	metadata, err := file.Stat()
	if err != nil || metadata.Size() <= 0 || metadata.Size() > maxOutcomeBytes {
		return nil, errors.New("outcome metadata is unsafe")
	}
	decoder := json.NewDecoder(file)
	decoder.UseNumber()
	var outcome durableOutcome
	if err := decoder.Decode(&outcome); err != nil {
		return nil, errors.New("outcome is invalid")
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("outcome has trailing data")
	}
	digest, err := requestDigest(request)
	if err != nil || outcome.SchemaVersion != 1 || outcome.RequestDigest != digest || (outcome.OK && outcome.Result == nil) || (!outcome.OK && outcome.ErrorCode == "") {
		return nil, errors.New("outcome binding is invalid")
	}
	return &outcome, nil
}

func openPrivateOutcome(path string, expectedUID uint32) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, errors.New("outcome descriptor is invalid")
	}
	metadata, err := file.Stat()
	if err != nil || !metadata.Mode().IsRegular() || metadata.Mode().Perm() != 0o600 || !ownedBy(metadata, expectedUID) {
		file.Close()
		return nil, errors.New("outcome metadata is unsafe")
	}
	return file, nil
}

func requestDigest(request protocol.Request) (string, error) {
	raw, err := json.Marshal(request.Fields)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

func (store OutcomeStore) save(request protocol.Request, outcome durableOutcome) error {
	if err := ensureOutcomeDirectory(store.Root, store.ExpectedUID); err != nil {
		return err
	}
	digest, err := requestDigest(request)
	if err != nil {
		return err
	}
	outcome.SchemaVersion, outcome.RequestDigest = 1, digest
	raw, err := json.Marshal(outcome)
	if err != nil || len(raw)+1 > maxOutcomeBytes {
		return errors.New("outcome encoding failed")
	}
	raw = append(raw, '\n')
	path := filepath.Join(store.Root, request.RequestID()+".json")
	temporary := path + ".tmp-" + fmt.Sprint(os.Getpid())
	fd, err := syscall.Open(temporary, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), temporary)
	abort := func(failure error) error { file.Close(); _ = os.Remove(temporary); return failure }
	written, err := file.Write(raw)
	if err != nil || written != len(raw) {
		return abort(errors.New("outcome write failed"))
	}
	if err := file.Sync(); err != nil {
		return abort(errors.New("outcome sync failed"))
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(temporary)
		return errors.New("outcome close failed")
	}
	if err := os.Link(temporary, path); err != nil {
		_ = os.Remove(temporary)
		if errors.Is(err, os.ErrExist) {
			existing, loadErr := store.load(request)
			if loadErr != nil || !sameOutcome(existing, &outcome) {
				return errors.New("outcome conflicts with existing evidence")
			}
			return nil
		}
		return errors.New("outcome publish failed")
	}
	if err := os.Remove(temporary); err != nil {
		return errors.New("outcome cleanup failed")
	}
	return syncAuditDirectory(store.Root)
}

func sameOutcome(left, right *durableOutcome) bool {
	if left == nil || right == nil {
		return false
	}
	leftRaw, leftErr := json.Marshal(left)
	rightRaw, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftRaw) == string(rightRaw)
}

func ensureOutcomeDirectory(path string, expectedUID uint32) error {
	parent := filepath.Dir(path)
	if _, err := ensureOneOutcomeDirectory(parent, expectedUID); err != nil {
		return err
	}
	created, err := ensureOneOutcomeDirectory(path, expectedUID)
	if err != nil {
		return err
	}
	if created {
		if err := syncAuditDirectory(path); err != nil {
			return err
		}
		return syncAuditDirectory(parent)
	}
	return nil
}

func ensureOneOutcomeDirectory(path string, expectedUID uint32) (bool, error) {
	created := false
	if err := os.Mkdir(path, 0o700); err != nil {
		if !os.IsExist(err) {
			return false, errors.New("outcome directory create failed")
		}
	} else {
		created = true
	}
	metadata, err := os.Lstat(path)
	if err != nil || !metadata.IsDir() || metadata.Mode().Perm() != 0o700 || !ownedBy(metadata, expectedUID) {
		return false, errors.New("outcome directory metadata is unsafe")
	}
	if created {
		if err := syncAuditDirectory(path); err != nil {
			return false, err
		}
		if err := syncAuditDirectory(filepath.Dir(path)); err != nil {
			return false, err
		}
	}
	return created, nil
}

func outcomeOperation(operation string) bool {
	switch operation {
	case "ensure_guest_running", "stop_guest", "pause_guest", "resume_guest", "resize_sandbox":
		return true
	default:
		return false
	}
}

// DurableDispatch replays completed mutating operations and persists every
// new disposition before it can be reported or audited. A persistence failure
// after dispatch is explicitly ambiguous and must stop supervisor automation.
func DurableDispatch(store OutcomeStore, dispatch Dispatcher) Dispatcher {
	return func(request protocol.Request) (map[string]interface{}, error) {
		if !outcomeOperation(request.Operation) {
			return dispatch(request)
		}
		existing, err := store.load(request)
		if err != nil {
			return nil, protoError("unsafe_state", "executor outcome evidence is invalid")
		}
		if existing != nil {
			if existing.OK {
				return existing.Result, nil
			}
			return nil, protoError(existing.ErrorCode, existing.ErrorMessage)
		}
		result, dispatchErr := dispatch(request)
		outcome := durableOutcome{OK: dispatchErr == nil, Result: result}
		if dispatchErr != nil {
			outcome.ErrorCode, outcome.ErrorMessage, _ = codeOf(dispatchErr)
			if outcome.ErrorMessage == "" {
				outcome.ErrorMessage = "executor operation failed closed"
			}
		}
		if err := store.save(request, outcome); err != nil {
			return nil, protoError("ambiguous_outcome", "executor outcome could not be persisted after dispatch")
		}
		return result, dispatchErr
	}
}
