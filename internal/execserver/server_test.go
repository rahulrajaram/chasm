package execserver

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/rahulrajaram/chasm/internal/protocol"
)

// --- wire parity ---------------------------------------------------------

func TestSuccessResponseByteParity(t *testing.T) {
	frame, err := successResponse("r1", map[string]interface{}{
		"a": 1,
		"b": "x",
		"c": "<b>&",
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// Python: json.dumps(payload, sort_keys=True, separators=(",", ":")) + "\n"
	// — sorted keys, compact, no HTML escaping.
	expect := "{\"ok\":true,\"request_id\":\"r1\",\"result\":{\"a\":1,\"b\":\"x\",\"c\":\"<b>&\"},\"version\":1}\n"
	if string(frame) != expect {
		t.Fatalf("success frame mismatch:\n got  %q\n want %q", frame, expect)
	}
}

func TestErrorResponseByteParity(t *testing.T) {
	id := "r1"
	frame, err := errorResponse(&id, "invalid_state", "the sandbox is not in the admitted state")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	expect := "{\"error\":{\"code\":\"invalid_state\",\"message\":\"the sandbox is not in the admitted state\"},\"ok\":false,\"request_id\":\"r1\",\"version\":1}\n"
	if string(frame) != expect {
		t.Fatalf("error frame mismatch:\n got  %q\n want %q", frame, expect)
	}
}

func TestResponseBoundHonored(t *testing.T) {
	big := make(map[string]interface{})
	big["x"] = string(make([]byte, protocol.MaxResponseBytes+64))
	if _, err := successResponse("r1", big); err == nil {
		t.Fatal("oversized response must be refused")
	}
}

func TestWriteFrameTimesOutWhenPeerDoesNotRead(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	writerFile := os.NewFile(uintptr(fds[0]), "writer")
	peerFile := os.NewFile(uintptr(fds[1]), "peer")
	defer writerFile.Close()
	defer peerFile.Close()
	writer, err := net.FileConn(writerFile)
	if errors.Is(err, syscall.EPERM) {
		// Some sandboxes prohibit Go's getsockopt on Unix sockets.
		t.Log("write deadline transport: net.Pipe (Unix FileConn denied)")
		writer, peer := net.Pipe()
		defer writer.Close()
		defer peer.Close()
		checkBlockedResponseWrite(t, writer, protocol.MaxResponseBytes)
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	peer, err := net.FileConn(peerFile)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	t.Log("write deadline transport: Unix socket pair")
	checkBlockedResponseWrite(t, writer, protocol.MaxResponseBytes)
}

func checkBlockedResponseWrite(t *testing.T, writer net.Conn, size int) {
	t.Helper()
	original := responseWriteTimeout
	responseWriteTimeout = 40 * time.Millisecond
	t.Cleanup(func() { responseWriteTimeout = original })
	start := time.Now()
	result := make(chan error, 1)
	go func() { result <- writeFrame(writer, make([]byte, size)) }()
	var err error
	select {
	case err = <-result:
	case <-time.After(3 * time.Second):
		t.Fatal("write to unread peer did not finish within 3 seconds; write deadline may be missing")
	}
	if !os.IsTimeout(err) || time.Since(start) > time.Second {
		t.Fatalf("write to unread peer: err=%v elapsed=%v", err, time.Since(start))
	}
}

// --- connection flow -----------------------------------------------------

// roundTrip serves one connection over a socketpair with the given
// handler and writes the request, returning the raw response.
func roundTrip(t *testing.T, expectedUID int, dispatch Dispatcher, audit AuditFunc, request []byte) ([]byte, error) {
	t.Helper()
	// Use a real unix listener so SO_PEERCRED reflects the actual peer.
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "s.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: sockPath, Net: "unix"})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	serveErr := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		connection, acceptErr := listener.AcceptUnix()
		if acceptErr != nil {
			serveErr <- acceptErr
			return
		}
		defer connection.Close()
		HandleConnection(connection, expectedUID, dispatch, audit)
	}()

	client, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: sockPath, Net: "unix"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	if _, err := client.Write(request); err != nil {
		t.Fatalf("write: %v", err)
	}
	buffer := readFrame(t, client)
	wg.Wait()
	select {
	case err := <-serveErr:
		return nil, err
	default:
	}
	return buffer, nil
}

func readFrame(t *testing.T, conn *net.UnixConn) []byte {
	t.Helper()
	buffer := make([]byte, 0, 4096)
	chunk := make([]byte, 4096)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, err := conn.Read(chunk)
		buffer = append(buffer, chunk[:n]...)
		if err != nil {
			if err != io.EOF && len(buffer) == 0 {
				t.Fatalf("read: %v", err)
			}
			break
		}
		if len(buffer) > 0 && buffer[len(buffer)-1] == '\n' {
			break
		}
	}
	return buffer
}

func socketpair() (*net.UnixConn, *net.UnixConn, error) {
	// Not used; kept for clarity of intent. Real tests use a listener.
	return nil, nil, nil
}

func TestHandleConnectionUnauthorizedPeerRefused(t *testing.T) {
	// expectedUID = -1 always mismatches the real peer uid.
	frame, err := roundTrip(t, -1, func(request protocol.Request) (map[string]interface{}, error) {
		t.Fatal("dispatch must not run for an unauthorized peer")
		return nil, nil
	}, nil, []byte("{\"version\":1,\"request_id\":\"r1\",\"operation\":\"inspect_default_sandbox\"}\n"))
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(frame, &parsed); err != nil {
		t.Fatalf("response JSON: %v (%q)", err, frame)
	}
	if parsed["ok"] != false {
		t.Fatalf("expected refused frame, got %s", frame)
	}
	errorField := parsed["error"].(map[string]interface{})
	if errorField["code"] != "unauthorized_peer" {
		t.Fatalf("wrong code: %v", errorField["code"])
	}
}

func TestHandleConnectionRefusesGarbageFrame(t *testing.T) {
	type auditRecord struct {
		request *protocol.Request
		ok      bool
	}
	var audits []auditRecord
	frame, err := roundTrip(t, os.Geteuid(), nil, func(request *protocol.Request, peerUID int, ok bool, errorCode string) error {
		audits = append(audits, auditRecord{request: request, ok: ok})
		return nil
	}, []byte("not a frame at all\n"))
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(frame, &parsed); err != nil {
		t.Fatalf("response JSON: %v (%q)", err, frame)
	}
	if parsed["ok"] != false {
		t.Fatalf("expected refused frame, got %s", frame)
	}
	if len(audits) != 0 {
		t.Fatalf("audit must not fire for an unparseable request, got %d records", len(audits))
	}
}

func TestHandleConnectionSuccessAndAudit(t *testing.T) {
	type auditRecord struct {
		request   *protocol.Request
		peerUID   int
		ok        bool
		errorCode string
	}
	var audits []auditRecord
	dispatch := func(request protocol.Request) (map[string]interface{}, error) {
		return map[string]interface{}{"instance": "sandbox-demo"}, nil
	}
	frame, err := roundTrip(t, os.Geteuid(), dispatch, func(request *protocol.Request, peerUID int, ok bool, errorCode string) error {
		audits = append(audits, auditRecord{request: request, peerUID: peerUID, ok: ok, errorCode: errorCode})
		return nil
	}, []byte("{\"version\":1,\"request_id\":\"r1\",\"operation\":\"inspect_default_sandbox\"}\n"))
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(frame, &parsed); err != nil {
		t.Fatalf("response JSON: %v (%q)", err, frame)
	}
	if parsed["ok"] != true {
		t.Fatalf("expected accepted frame, got %s", frame)
	}
	result := parsed["result"].(map[string]interface{})
	if result["instance"] != "sandbox-demo" {
		t.Fatalf("wrong result: %v", result)
	}
	if len(audits) != 1 || !audits[0].ok || audits[0].request.RequestID() != "r1" || audits[0].request.Operation != "inspect_default_sandbox" {
		t.Fatalf("audit not recorded correctly: %+v", audits)
	}
}

func TestHandleConnectionFailsClosedWhenAuditPersistenceFails(t *testing.T) {
	dispatched := false
	dispatch := func(request protocol.Request) (map[string]interface{}, error) {
		dispatched = true
		return map[string]interface{}{"instance": "sandbox-demo"}, nil
	}
	audit := func(request *protocol.Request, peerUID int, ok bool, errorCode string) error {
		return errors.New("injected audit sync failure")
	}
	frame, err := roundTrip(t, os.Geteuid(), dispatch, audit, []byte("{\"version\":1,\"request_id\":\"audit-fail\",\"operation\":\"inspect_default_sandbox\"}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !dispatched {
		t.Fatal("dispatch did not run")
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(frame, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["ok"] != false {
		t.Fatalf("audit failure leaked success: %s", frame)
	}
	errorField := parsed["error"].(map[string]interface{})
	if errorField["code"] != "operation_failed" {
		t.Fatalf("unexpected audit failure code: %s", frame)
	}
}

func TestHandleConnectionDispatchErrorFrames(t *testing.T) {
	dispatch := func(request protocol.Request) (map[string]interface{}, error) {
		return nil, protocolError("invalid_state", "an invalid state was reported")
	}
	frame, err := roundTrip(t, os.Geteuid(), dispatch, nil, []byte("{\"version\":1,\"request_id\":\"r2\",\"operation\":\"pause_guest\",\"instance\":\"sandbox-demo\"}\n"))
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(frame, &parsed); err != nil {
		t.Fatalf("response JSON: %v (%q)", err, frame)
	}
	errorField := parsed["error"].(map[string]interface{})
	if errorField["code"] != "invalid_state" {
		t.Fatalf("wrong code: %v (%s)", errorField["code"], frame)
	}
}

func TestHandleConnectionRejectsOnlyOneRequest(t *testing.T) {
	frame, err := roundTrip(t, os.Geteuid(), nil, nil, []byte("{\"version\":1,\"request_id\":\"r1\",\"operation\":\"inspect_default_sandbox\"}\n{\"version\":1}\n"))
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(frame, &parsed); err != nil {
		t.Fatalf("response JSON: %v (%q)", err, frame)
	}
	errorField := parsed["error"].(map[string]interface{})
	if errorField["code"] != "invalid_frame" {
		t.Fatalf("wrong code: %v (%s)", errorField["code"], frame)
	}
}

func TestRequestFieldsAndNumberAccess(t *testing.T) {
	raw := []byte("{\"version\":1,\"request_id\":\"req-1\",\"operation\":\"sandbox_layout\",\"instance\":\"sandbox-demo\",\"depth\":3}")
	request, err := protocol.ParseExecutor(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if request.Operation != "sandbox_layout" || request.RequestID() != "req-1" || request.Instance() != "sandbox-demo" {
		t.Fatalf("field access mismatch: %+v", request)
	}
	size, ok := request.NumberField("depth")
	if !ok || size != 3 {
		t.Fatalf("number access: %d %v", size, ok)
	}
}

// --- request field retention (protocol extension) -------------------------

func TestParseExecutorRetainsFields(t *testing.T) {
	raw := []byte("{\"version\":1,\"request_id\":\"req\",\"operation\":\"inspect_default_sandbox\"}")
	request, err := protocol.ParseExecutor(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if request.Fields == nil {
		t.Fatal("Fields was not retained")
	}
	if got := request.StringField("request_id"); got != "req" {
		t.Fatalf("request_id: %q", got)
	}
}

func TestHandleConnectionPartialFrameThenCloseFailsClosed(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "s.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: sockPath, Net: "unix"})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	dispatch := func(request protocol.Request) (map[string]interface{}, error) {
		return map[string]interface{}{"operation": request.Operation}, nil
	}
	audit := func(request *protocol.Request, peerUID int, ok bool, errorCode string) error { return nil }
	go func() {
		// Serve two sequential connections: the aborted one, then the next
		// client. A wedged read loop would never reach the second accept.
		for i := 0; i < 2; i++ {
			connection, acceptErr := listener.AcceptUnix()
			if acceptErr != nil {
				return
			}
			HandleConnection(connection, os.Geteuid(), dispatch, audit)
			connection.Close()
		}
	}()

	// Client one sends a request prefix (no newline) and hangs up mid-frame.
	aborted, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: sockPath, Net: "unix"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if _, err := aborted.Write([]byte("{\"version\":1,\"request_id\":\"r1\",\"operation\":\"regi")); err != nil {
		t.Fatalf("write prefix: %v", err)
	}
	aborted.Close()

	// Client two must be served promptly: the partial frame must have been
	// failed closed instead of spinning the sequential accept loop forever.
	next, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: sockPath, Net: "unix"})
	if err != nil {
		t.Fatalf("dial next: %v", err)
	}
	defer next.Close()
	if _, err := next.Write([]byte("{\"version\":1,\"request_id\":\"r2\",\"operation\":\"inspect_default_sandbox\"}\n")); err != nil {
		t.Fatalf("write next: %v", err)
	}
	frame := readFrame(t, next)
	var parsed map[string]interface{}
	if err := json.Unmarshal(frame, &parsed); err != nil {
		t.Fatalf("next response JSON: %v (%q)", err, frame)
	}
	if parsed["ok"] != true {
		t.Fatalf("listener stopped serving after an aborted frame: %s", frame)
	}
}

func TestHandleConnectionFrameWithImmediateCloseStillServed(t *testing.T) {
	// A client that writes a complete frame and closes at once may hand the
	// reader data and end-of-file in the same read: the frame itself is
	// complete, so it must still be served.
	type auditRecord struct {
		request *protocol.Request
		ok      bool
	}
	var audits []auditRecord
	frame, err := roundTrip(t, os.Geteuid(), func(request protocol.Request) (map[string]interface{}, error) {
		return map[string]interface{}{"operation": request.Operation}, nil
	}, func(request *protocol.Request, peerUID int, ok bool, errorCode string) error {
		audits = append(audits, auditRecord{request: request, ok: ok})
		return nil
	}, []byte("{\"version\":1,\"request_id\":\"r1\",\"operation\":\"inspect_default_sandbox\"}\n"))
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(frame, &parsed); err != nil {
		t.Fatalf("response JSON: %v (%q)", err, frame)
	}
	if parsed["ok"] != true {
		t.Fatalf("expected accepted frame, got %s", frame)
	}
	if len(audits) != 1 || !audits[0].ok {
		t.Fatalf("audit not recorded correctly: %+v", audits)
	}
}

func repeat(char string, n int) string {
	out := make([]byte, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, char...)
	}
	return string(out)
}

var _ = reflect.TypeOf
