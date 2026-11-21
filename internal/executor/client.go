// Package executor provides the client for the chasm executor
// wire protocol: request framing, bounded response reads, and strict
// response decoding via protocol.ParseResponse.
package executor

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/rahulrajaram/chasm/internal/protocol"
)

const (
	// DefaultTimeout mirrors the Python client's 960 second socket timeout.
	// Like Python's socket.settimeout, this applies per receive operation,
	// not to the whole call: a peer trickling a valid response one byte
	// per timeout window is accepted, and only a silent window fails.
	DefaultTimeout = 960 * time.Second
)

// Call sends one request payload over the executor socket and decodes the
// response against the request's request_id. Operation errors arrive as
// *protocol.ResponseError; wire refusals as protocol.DecoderError.
func Call(
	socketPath string,
	payload map[string]interface{},
) (map[string]interface{}, error) {
	return CallWithTimeout(socketPath, payload, DefaultTimeout)
}

// CallWithTimeout is Call with an explicit per-receive timeout, mirroring
// Python's connection.settimeout semantics: the deadline resets before
// every read, so each individual receive must complete within the window.
func CallWithTimeout(
	socketPath string,
	payload map[string]interface{},
	timeout time.Duration,
) (map[string]interface{}, error) {
	requestID, _ := payload["request_id"].(string)
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	encoded = append(encoded, '\n')
	connection, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("dial executor: %w", err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(timeout)); err != nil {
		return nil, fmt.Errorf("set deadline: %w", err)
	}
	sent, err := connection.Write(encoded)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	if sent < len(encoded) {
		if _, err := connection.Write(encoded[sent:]); err != nil {
			return nil, fmt.Errorf("send request remainder: %w", err)
		}
	}
	if err := connection.CloseWrite(); err != nil {
		return nil, fmt.Errorf("shutdown write: %w", err)
	}
	raw := make([]byte, 0, 4096)
	for {
		// Per-receive deadline: reset before every read so a slow but
		// responsive peer is never failed for total elapsed time.
		if err := connection.SetReadDeadline(time.Now().Add(timeout)); err != nil {
			return nil, fmt.Errorf("set read deadline: %w", err)
		}
		limit := 65536
		if remaining := protocol.MaxResponseBytes + 1 - len(raw); remaining < limit {
			limit = remaining
		}
		chunk := make([]byte, limit)
		read, err := connection.Read(chunk)
		raw = append(raw, chunk[:read]...)
		if len(raw) > protocol.MaxResponseBytes {
			return nil, protocol.DecoderError{Code: "response_too_large"}
		}
		if err != nil {
			if os.IsTimeout(err) {
				return nil, err
			}
			break
		}
	}
	return protocol.ParseResponse(raw, &requestID)
}
