package execserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/rahulrajaram/chasm/internal/protocol"
)

func TestLifecycleTimingPersistencePreservesOperationOutcome(t *testing.T) {
	for _, test := range []struct {
		name     string
		managed  bool
		opErr    error
		wantSave bool
	}{
		{"successful managed", true, nil, true},
		{"failed managed", true, errors.New("original failure"), true},
		{"missing instance", false, errors.New("not found"), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := protocol.Request{Operation: "ensure_guest_running", Fields: map[string]interface{}{"instance": "sandbox-missing", "request_id": "timing"}}
			calls := 0
			var log bytes.Buffer
			result, err := dispatchWithLifecycle(request,
				func(protocol.Request) (map[string]interface{}, error) {
					return map[string]interface{}{"applied": true}, test.opErr
				},
				func(string) error {
					if test.managed {
						return nil
					}
					return errors.New("not managed")
				},
				func(string, uint32, string, lifecycleObservation) error {
					calls++
					return errors.New("disk unavailable")
				}, &log)
			if err != test.opErr || result["applied"] != true {
				t.Fatalf("operation result changed: %v, %v", result, err)
			}
			if (calls == 1) != test.wantSave {
				t.Fatalf("observation calls = %d", calls)
			}
			if (log.Len() > 0) != test.wantSave {
				t.Fatalf("persistence error log = %q", log.String())
			}
		})
	}
}

func TestLifecycleLauncherUsesOnlyPrivateCompletedPhaseMarkers(t *testing.T) {
	command := exec.Command("/bin/sh", "-c", `
		printf '%s\n' 'boot begin 1.00' 'boot end 2.00'
		printf '%s\n' 'clone begin 10.05' 'clone end 11.45' \
			'guest_agent_ready begin 12.00' 'guest_agent_ready end 12.01' \
			'stopped_confirmation begin 13.00' >&3
	`)
	timings := lifecycleTimings{}
	if err := runInstrumentedLauncher(command, timings); err != nil {
		t.Fatal(err)
	}
	if want := (lifecycleTimings{"clone": 1400, "guest_agent_ready": 10}); !reflect.DeepEqual(timings, want) {
		t.Fatalf("timings = %v, want %v", timings, want)
	}
}

func TestLifecycleObservationRetentionAndAudit(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "performance")
	uid := uint32(os.Geteuid())
	const instance = "sandbox-research"
	create := lifecycleObservation{
		Operation: "create", RequestID: "create-id", ObservedAt: time.Unix(100, 0).UTC(), OK: true,
		Timings: lifecycleTimings{"clone": 300, "boot": 30, "guest_agent_ready": 500, "total": 1000},
	}
	stop := lifecycleObservation{
		Operation: "stop", RequestID: "stop-id", ObservedAt: time.Unix(200, 0).UTC(), OK: true,
		Timings: lifecycleTimings{"shutdown_request": 990, "stopped_confirmation": 10, "total": 1100},
	}
	if _, err := loadLifecycleObservations(root, uid, instance); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("read path created the observation directory")
	}
	for _, observation := range []lifecycleObservation{create, stop} {
		if err := saveLifecycleObservation(root, uid, instance, observation); err != nil {
			t.Fatal(err)
		}
	}
	items, err := loadLifecycleObservations(root, uid, instance)
	if want := []lifecycleObservation{create, stop}; err != nil || !reflect.DeepEqual(items, want) {
		t.Fatalf("observations = %#v, %v, want %#v", items, err, want)
	}

	if err := removeLifecycleObservations(root, uid, instance); err != nil {
		t.Fatal(err)
	}
	if remaining, err := loadLifecycleObservations(root, uid, instance); err != nil || len(remaining) != 0 {
		t.Fatalf("new sandbox would inherit observations %v, error %v", remaining, err)
	}

	request := protocol.Request{
		Operation: "stop_guest", Fields: map[string]interface{}{"request_id": stop.RequestID, "instance": instance},
		LifecycleTimings: stop.Timings,
	}
	audit, err := encodeAuditRecord(&request, 1000, true, "", stop.ObservedAt)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Timings map[string]int64 `json:"phase_timings_ms"`
	}
	if err := json.Unmarshal(audit, &decoded); err != nil || !reflect.DeepEqual(lifecycleTimings(decoded.Timings), stop.Timings) {
		t.Fatalf("audit timings = %s, err = %v", audit, err)
	}
}
