package execserver

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/rahulrajaram/chasm/internal/protocol"
)

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
