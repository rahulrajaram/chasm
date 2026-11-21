package executor

import (
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// startTrickleServer serves one connection: it trickles a complete valid
// response one byte at a time with a gap longer than trickleTimeout, then
// closes. A per-receive-deadline client accepts it; an absolute-deadline
// client would refuse. If stall is true the server goes silent after one
// byte, so the client must time out.
func startTrickleServer(t *testing.T, dir string, gap time.Duration, stall bool) string {
	t.Helper()
	path := filepath.Join(dir, "trickle.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		connection, err := listener.AcceptUnix()
		if err != nil {
			return
		}
		defer connection.Close()
		response, _ := json.Marshal(map[string]interface{}{
			"version":    1,
			"request_id": "trickle-1",
			"ok":         true,
			"result":     map[string]interface{}{"marker": "ok"},
		})
		frame := append(response, '\n')
		_, _ = connection.Write(frame[:1])
		if stall {
			time.Sleep(10 * gap)
			return
		}
		for _, b := range frame[1:] {
			time.Sleep(gap)
			if _, err := connection.Write([]byte{b}); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { listener.Close() })
	return path
}

func TestCallWithTimeoutAcceptsSlowTrickle(t *testing.T) {
	dir := t.TempDir()
	socketPath := startTrickleServer(t, dir, 300*time.Millisecond, false)
	payload := map[string]interface{}{"version": 1, "request_id": "trickle-1", "operation": "inspect_default_sandbox"}

	result, err := CallWithTimeout(socketPath, payload, 2*time.Second)
	if err != nil {
		t.Fatalf("slow-but-responsive peer must be accepted: %v", err)
	}
	if result["marker"] != "ok" {
		t.Fatalf("unexpected result: %v", result)
	}
}

func TestCallWithTimeoutFailsSilentPeer(t *testing.T) {
	dir := t.TempDir()
	socketPath := startTrickleServer(t, dir, 300*time.Millisecond, true)
	payload := map[string]interface{}{"version": 1, "request_id": "trickle-1", "operation": "inspect_default_sandbox"}

	if _, err := CallWithTimeout(socketPath, payload, 1*time.Second); err == nil {
		t.Fatal("silent peer must time out")
	}
}
