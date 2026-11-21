// Package execserver implements the privileged executor socket service
// that runs as root on the site host. Socket activation, peer-credential
// gating, request parsing, dispatch and response framing live here.
//
// The response wire format is compact JSON (sorted keys, no spaces,
// no HTML escaping) + "\n", bounded by the protocol response limit.
package execserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/rahulrajaram/chasm/internal/protocol"
)

// serverTimeout mirrors SOCKET_TIMEOUT_SECONDS in executor_server.py: the
// per-receive deadline, reset before every read.
const serverTimeout = 5 // seconds

var responseWriteTimeout = serverTimeout * time.Second

// ProtocolError mirrors control_plane.ProtocolError: an operation error
// delivered in a well-formed response frame.
type ProtocolError struct {
	Code    string
	Message string
}

func (e *ProtocolError) Error() string { return e.Code }

func protocolError(code, message string) *ProtocolError {
	return &ProtocolError{Code: code, Message: message}
}

// CompactJSON is the exported compact-sorted-key JSON encoder used also by
// the supervisor server for byte parity.
func CompactJSON(payload map[string]interface{}) ([]byte, error) { return compactJSON(payload) }

// compactJSON mirrors _response_bytes: json.dumps(payload,
// sort_keys=True, separators=(",", ":")).encode("utf-8") + "\n".
// Go's encoding/json marshals map keys in sorted order and Encoder output
// is compact; SetEscapeHTML(false) disables the <,>,& and U+2028/2029
// escaping Python never performs.
func compactJSON(payload map[string]interface{}) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(payload); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// successResponse mirrors success_response.
func successResponse(requestID string, result map[string]interface{}) ([]byte, error) {
	payload := map[string]interface{}{
		"version":    protocol.ProtocolVersion,
		"request_id": requestID,
		"ok":         true,
		"result":     result,
	}
	return encodeBounded(payload)
}

// errorResponse mirrors error_response. A nil requestID (decode refusals
// and unauthorized peers) is encoded as JSON null, exactly like Python.
func errorResponse(requestID *string, code, message string) ([]byte, error) {
	payload := map[string]interface{}{
		"version":    protocol.ProtocolVersion,
		"request_id": nilToNull(requestID),
		"ok":         false,
		"error": map[string]interface{}{
			"code":    code,
			"message": message,
		},
	}
	return encodeBounded(payload)
}

func nilToNull(requestID *string) interface{} {
	if requestID == nil {
		return nil
	}
	return *requestID
}

func encodeBounded(payload map[string]interface{}) ([]byte, error) {
	encoded, err := compactJSON(payload)
	if err != nil {
		return nil, err
	}
	if len(encoded) > protocol.MaxResponseBytes {
		return nil, protocolError("response_too_large", "executor response exceeded its bound")
	}
	return encoded, nil
}

func writeFrame(conn interface {
	Write([]byte) (int, error)
	SetWriteDeadline(time.Time) error
}, frame []byte) error {
	if err := conn.SetWriteDeadline(time.Now().Add(responseWriteTimeout)); err != nil {
		return err
	}
	if os.Getenv("EXECSERVER_DEBUG") != "" {
		text := string(frame)
		if len(text) > 200 {
			text = text[:200]
		}
		fmt.Fprintf(os.Stderr, "execserver out: %s\n", text)
	}
	written, err := conn.Write(frame)
	if err != nil {
		return err
	}
	if written < len(frame) {
		return errors.New("short write of executor response")
	}
	return nil
}

func mustEncodeSuccess(requestID string, result map[string]interface{}) []byte {
	frame, err := successResponse(requestID, result)
	if err != nil {
		// Bounded payloads cannot be too large; encode errors imply a
		// program bug and are surfaced through the request decoder path.
		panic(fmt.Sprintf("execserver: encode success: %v", err))
	}
	return frame
}

func mustEncodeError(requestID *string, code, message string) []byte {
	frame, err := errorResponse(requestID, code, message)
	if err != nil {
		panic(fmt.Sprintf("execserver: encode error: %v", err))
	}
	return frame
}

// systemdListener mirrors _systemd_listener: the service requires systemd
// socket activation with exactly one fd (fd 3).
func systemdListener() (*os.File, error) {
	if os.Getenv("LISTEN_PID") != strconv.Itoa(os.Getpid()) {
		return nil, protocolError("invalid_socket", "executor requires systemd socket activation")
	}
	if os.Getenv("LISTEN_FDS") != "1" {
		return nil, protocolError("invalid_socket", "executor requires exactly one systemd socket")
	}
	file := os.NewFile(3, "chasm-executor.sock")
	if file == nil {
		return nil, protocolError("invalid_socket", "systemd socket fd is not available")
	}
	return file, nil
}
