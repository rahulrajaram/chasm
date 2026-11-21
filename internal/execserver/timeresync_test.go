package execserver

import (
	"errors"
	"io"
	"os"
	"testing"
	"time"
)

// The guest time resync is a best-effort post-lifecycle courtesy: a frozen
// VM keeps its pre-pause wall clock, and f-2570 observed a resumed guest
// lagging the host by 2h18m with no active NTP service. These tests hold
// the contract: the resync issues one bounded, in-guest
// `systemctl restart systemd-timesyncd` through the same Incus exec seam
// the executor already uses, reports its outcome as an informational
// "time_resync" field on the resume and start responses, and never fails
// the surrounding lifecycle operation.

func stubRunIncusForTimeResync(t *testing.T, code int, err error) *[][]string {
	t.Helper()
	original := runIncus
	t.Cleanup(func() { runIncus = original })
	calls := &[][]string{}
	runIncus = func(arguments []string, _ io.Reader, _ io.Writer, _ []*os.File, _ time.Duration) (int, error) {
		*calls = append(*calls, arguments)
		return code, err
	}
	return calls
}

func TestRequestGuestTimeResyncIssuesBoundedInGuestRestart(t *testing.T) {
	calls := stubRunIncusForTimeResync(t, 0, nil)

	outcome := requestGuestTimeResync("sandbox-demo")

	if outcome != "requested" {
		t.Fatalf("time resync outcome = %q, want requested", outcome)
	}
	if len(*calls) != 1 {
		t.Fatalf("resync issued %d incus calls, want 1", len(*calls))
	}
	want := []string{"exec", "sandbox-demo", "--", "systemctl", "restart", "systemd-timesyncd"}
	got := (*calls)[0]
	if len(got) != len(want) {
		t.Fatalf("resync command = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("resync command = %v, want %v", got, want)
		}
	}
}

func TestRequestGuestTimeResyncTimesOutBounded(t *testing.T) {
	original := runIncus
	t.Cleanup(func() { runIncus = original })
	var observed time.Duration
	runIncus = func(_ []string, _ io.Reader, _ io.Writer, _ []*os.File, timeout time.Duration) (int, error) {
		observed = timeout
		return 0, nil
	}

	if outcome := requestGuestTimeResync("sandbox-demo"); outcome != "requested" {
		t.Fatalf("time resync outcome = %q, want requested", outcome)
	}
	if observed <= 0 || observed > 30*time.Second {
		t.Fatalf("resync timeout = %v, want a positive bound of at most 30s", observed)
	}
}

func TestRequestGuestTimeResyncReportsFailureWithoutError(t *testing.T) {
	for name, stub := range map[string]struct {
		code int
		err  error
	}{
		"nonzero exit": {code: 5, err: nil},
		"exec error":   {code: 0, err: errors.New("incus unavailable")},
	} {
		t.Run(name, func(t *testing.T) {
			stubRunIncusForTimeResync(t, stub.code, stub.err)
			if outcome := requestGuestTimeResync("sandbox-demo"); outcome != "failed" {
				t.Fatalf("time resync outcome = %q, want failed", outcome)
			}
		})
	}
}

func TestResumeSandboxWithRequestsGuestTimeResync(t *testing.T) {
	states := []map[string]interface{}{
		{"status": "Frozen", "pid": float64(4242)},
		{"status": "Running", "pid": float64(4242)},
	}
	reads := 0
	resumeCalls := 0
	resyncInstances := []string{}
	result, err := resumeSandboxWith("sandbox-research", func() (map[string]interface{}, error) {
		state := states[reads]
		reads++
		return state, nil
	}, func() (int, error) {
		resumeCalls++
		return 0, nil
	}, func(instance string) string {
		resyncInstances = append(resyncInstances, instance)
		return "requested"
	})
	if err != nil {
		t.Fatalf("resume refused: %v", err)
	}
	if resumeCalls != 1 {
		t.Fatalf("resume issued %d incus resume calls, want 1", resumeCalls)
	}
	if len(resyncInstances) != 1 || resyncInstances[0] != "sandbox-research" {
		t.Fatalf("resync was not requested for the resumed instance: %v", resyncInstances)
	}
	if result["time_resync"] != "requested" {
		t.Fatalf("resume response time_resync = %v, want requested", result["time_resync"])
	}
	if result["resumed"] != true || result["instance_status"] != "Running" {
		t.Fatalf("resume response lost its lifecycle fields: %v", result)
	}
}

func TestResumeSandboxWithReportsFailedResyncWithoutFailing(t *testing.T) {
	states := []map[string]interface{}{
		{"status": "Frozen", "pid": float64(4242)},
		{"status": "Running", "pid": float64(4242)},
	}
	reads := 0
	result, err := resumeSandboxWith("sandbox-research", func() (map[string]interface{}, error) {
		state := states[reads]
		reads++
		return state, nil
	}, func() (int, error) {
		return 0, nil
	}, func(string) string {
		return "failed"
	})
	if err != nil {
		t.Fatalf("a failed time resync must not fail the resume: %v", err)
	}
	if result["time_resync"] != "failed" {
		t.Fatalf("resume response time_resync = %v, want failed", result["time_resync"])
	}
}

func TestResumeSandboxWithRefusesWithoutRequestingResync(t *testing.T) {
	t.Run("invalid instance", func(t *testing.T) {
		resynced := false
		_, err := resumeSandboxWith("../escape", func() (map[string]interface{}, error) {
			return map[string]interface{}{"status": "Frozen"}, nil
		}, func() (int, error) { return 0, nil }, func(string) string {
			resynced = true
			return "requested"
		})
		if err == nil {
			t.Fatal("resume accepted an invalid instance name")
		}
		if resynced {
			t.Fatal("time resync was requested for a refused resume")
		}
	})
	t.Run("not frozen", func(t *testing.T) {
		resynced := false
		_, err := resumeSandboxWith("sandbox-research", func() (map[string]interface{}, error) {
			return map[string]interface{}{"status": "Stopped", "pid": float64(0)}, nil
		}, func() (int, error) { return 0, nil }, func(string) string {
			resynced = true
			return "requested"
		})
		if err == nil {
			t.Fatal("resume accepted a stopped instance")
		}
		if resynced {
			t.Fatal("time resync was requested for a refused resume")
		}
	})
	t.Run("resume failure", func(t *testing.T) {
		resynced := false
		_, err := resumeSandboxWith("sandbox-research", func() (map[string]interface{}, error) {
			return map[string]interface{}{"status": "Frozen", "pid": float64(4242)}, nil
		}, func() (int, error) { return 1, nil }, func(string) string {
			resynced = true
			return "requested"
		})
		if err == nil {
			t.Fatal("resume accepted a failed incus resume")
		}
		if resynced {
			t.Fatal("time resync was requested for a resume that never succeeded")
		}
	})
}

func TestSandboxRunningResultResyncsOnlyOnTransition(t *testing.T) {
	t.Run("fresh start or unfreeze", func(t *testing.T) {
		resyncInstances := []string{}
		result := sandboxRunningResult("sandbox-research", 4242, true, func(instance string) string {
			resyncInstances = append(resyncInstances, instance)
			return "requested"
		})
		if len(resyncInstances) != 1 || resyncInstances[0] != "sandbox-research" {
			t.Fatalf("resync was not requested for the started instance: %v", resyncInstances)
		}
		if result["time_resync"] != "requested" {
			t.Fatalf("start response time_resync = %v, want requested", result["time_resync"])
		}
		if result["started"] != true || result["agent_ready"] != true || result["instance_pid"] != int64(4242) {
			t.Fatalf("start response lost its lifecycle fields: %v", result)
		}
	})
	t.Run("already running", func(t *testing.T) {
		resynced := false
		result := sandboxRunningResult("sandbox-research", 4242, false, func(string) string {
			resynced = true
			return "requested"
		})
		if resynced {
			t.Fatal("time resync was requested for an already-running guest")
		}
		if result["time_resync"] != "skipped" {
			t.Fatalf("start response time_resync = %v, want skipped", result["time_resync"])
		}
	})
}
