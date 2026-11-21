package execserver

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rahulrajaram/chasm/internal/protocol"
)

func outcomeRequest(t *testing.T, requestID, operation string) protocol.Request {
	t.Helper()
	payload := `{"version":1,"request_id":"` + requestID + `","operation":"` + operation + `"`
	payload += `,"instance":"sandbox-demo"}`
	raw := []byte(payload)
	request, err := protocol.ParseExecutor(raw)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func testOutcomeStore(t *testing.T) OutcomeStore {
	t.Helper()
	base := t.TempDir()
	if err := os.Chmod(base, 0o700); err != nil {
		t.Fatal(err)
	}
	return OutcomeStore{Root: filepath.Join(base, "outcomes"), ExpectedUID: uint32(os.Geteuid())}
}

func TestDurableDispatchReplaysSuccessWithoutRedispatch(t *testing.T) {
	store := testOutcomeStore(t)
	request := outcomeRequest(t, "stable-1", "pause_guest")
	calls := 0
	dispatch := DurableDispatch(store, func(protocol.Request) (map[string]interface{}, error) {
		calls++
		return map[string]interface{}{"already_imported": true}, nil
	})
	first, err := dispatch(request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := dispatch(request)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || first["already_imported"] != true {
		t.Fatalf("outcome was not replayed: calls=%d first=%v second=%v", calls, first, second)
	}
}

func TestOutcomeStorePersistsPrivateDisposition(t *testing.T) {
	store := testOutcomeStore(t)
	request := outcomeRequest(t, "store-probe", "pause_guest")
	if err := store.save(request, durableOutcome{OK: true, Result: map[string]interface{}{"already_imported": true}}); err != nil {
		t.Fatalf("save outcome: %v", err)
	}
	loaded, err := store.load(request)
	if err != nil || loaded == nil || !loaded.OK {
		t.Fatalf("load outcome: %+v %v", loaded, err)
	}
}

func TestDurableDispatchReplaysFailureAndRefusesRequestCollision(t *testing.T) {
	store := testOutcomeStore(t)
	request := outcomeRequest(t, "stable-2", "pause_guest")
	calls := 0
	dispatch := DurableDispatch(store, func(protocol.Request) (map[string]interface{}, error) {
		calls++
		return nil, protoError("invalid_state", "verification failed")
	})
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := dispatch(request); err == nil {
			t.Fatal("failure was not replayed")
		}
	}
	if calls != 1 {
		t.Fatalf("failed operation redispatched %d times", calls)
	}
	collisionRaw := []byte(`{"version":1,"request_id":"stable-2","operation":"pause_guest","instance":"sandbox-demo"}`)
	collision, parseErr := protocol.ParseExecutor(collisionRaw)
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	if _, err := dispatch(collision); err == nil {
		t.Fatal("request identity collision was accepted")
	}
	if calls != 1 {
		t.Fatal("collision reached dispatch")
	}
}

func TestDurableDispatchMarksPostDispatchPersistenceFailureAmbiguous(t *testing.T) {
	base := t.TempDir()
	if err := os.Chmod(base, 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "outcomes")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	store := OutcomeStore{Root: root, ExpectedUID: uint32(os.Geteuid())}
	request := outcomeRequest(t, "stable-3", "pause_guest")
	calls := 0
	dispatch := DurableDispatch(store, func(protocol.Request) (map[string]interface{}, error) {
		calls++
		if err := os.Chmod(root, 0o500); err != nil {
			t.Fatal(err)
		}
		return map[string]interface{}{"already_imported": true}, nil
	})
	_, err := dispatch(request)
	var protocolError *ProtocolError
	if calls != 1 || !errors.As(err, &protocolError) || protocolError.Code != "ambiguous_outcome" {
		t.Fatalf("expected ambiguous outcome, calls=%d err=%v", calls, err)
	}
}

func TestAuditFailureAfterPersistedOutcomeCanReplay(t *testing.T) {
	store := testOutcomeStore(t)
	calls := 0
	dispatch := DurableDispatch(store, func(protocol.Request) (map[string]interface{}, error) {
		calls++
		return map[string]interface{}{"already_imported": true}, nil
	})
	raw := []byte("{\"version\":1,\"request_id\":\"stable-4\",\"operation\":\"pause_guest\",\"instance\":\"sandbox-demo\"}\n")
	failingAudit := func(*protocol.Request, int, bool, string) error { return errors.New("injected audit failure") }
	frame, err := roundTrip(t, os.Geteuid(), dispatch, failingAudit, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(frame, []byte(`"ok":false`)) {
		t.Fatalf("audit failure leaked success: %s", frame)
	}
	frame, err = roundTrip(t, os.Geteuid(), dispatch, func(*protocol.Request, int, bool, string) error { return nil }, raw)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !bytes.Contains(frame, []byte(`"ok":true`)) {
		t.Fatalf("persisted outcome was not replayed: calls=%d frame=%s", calls, frame)
	}
}
