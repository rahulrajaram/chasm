package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/rahulrajaram/chasm/internal/protocol"
)

const (
	supervisorResponseLimit = 2 * 1024 * 1024
	supervisorTimeout       = 980 * time.Second
)

type supervisorTransport interface {
	call(operation string, fields map[string]interface{}) (map[string]interface{}, error)
}

type transportError struct {
	Code    string
	Message string
}

func (e transportError) Error() string {
	return e.Code + ": " + e.Message
}

type unixSupervisorTransport struct {
	socketPath string
	now        func() time.Time
	pid        int
	sequence   uint64
}

func newUnixSupervisorTransport(socketPath string) supervisorTransport {
	return &unixSupervisorTransport{socketPath: socketPath, now: time.Now, pid: os.Getpid()}
}

func (transport *unixSupervisorTransport) call(operation string, fields map[string]interface{}) (map[string]interface{}, error) {
	transport.sequence++
	requestID := fmt.Sprintf(
		"chasmctl-%s-%d-%d",
		transport.now().UTC().Format("20060102T150405Z"),
		transport.pid,
		transport.sequence,
	)
	payload := make(map[string]interface{}, len(fields)+3)
	payload["version"] = protocol.ProtocolVersion
	payload["request_id"] = requestID
	payload["operation"] = operation
	for key, value := range fields {
		payload[key] = value
	}
	return callSupervisor(transport.socketPath, requestID, payload, supervisorTimeout)
}

func callSupervisor(socketPath string, requestID string, payload map[string]interface{}, timeout time.Duration) (map[string]interface{}, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, transportError{Code: "operation_failed", Message: "supervisor request could not be encoded"}
	}
	encoded = append(encoded, '\n')
	connection, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		return nil, transportError{Code: "operation_failed", Message: "supervisor is unavailable"}
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(timeout)); err != nil {
		return nil, transportError{Code: "operation_failed", Message: "supervisor deadline could not be set"}
	}
	sent, err := connection.Write(encoded)
	if err != nil {
		return nil, transportError{Code: "operation_failed", Message: "supervisor request could not be sent"}
	}
	if sent < len(encoded) {
		if _, err := connection.Write(encoded[sent:]); err != nil {
			return nil, transportError{Code: "operation_failed", Message: "supervisor request could not be sent"}
		}
	}
	if err := connection.CloseWrite(); err != nil {
		return nil, transportError{Code: "operation_failed", Message: "supervisor request could not be finalized"}
	}
	raw, err := readSupervisorResponse(connection, timeout)
	if err != nil {
		return nil, err
	}
	return parseSupervisorResponse(raw, requestID)
}

func readSupervisorResponse(connection *net.UnixConn, timeout time.Duration) ([]byte, error) {
	raw := make([]byte, 0, 4096)
	for {
		if err := connection.SetReadDeadline(time.Now().Add(timeout)); err != nil {
			return nil, transportError{Code: "operation_failed", Message: "supervisor deadline could not be set"}
		}
		remaining := supervisorResponseLimit + 1 - len(raw)
		if remaining <= 0 {
			return nil, transportError{Code: "response_too_large", Message: "supervisor response is oversized"}
		}
		if remaining > 64*1024 {
			remaining = 64 * 1024
		}
		chunk := make([]byte, remaining)
		read, err := connection.Read(chunk)
		raw = append(raw, chunk[:read]...)
		if len(raw) > supervisorResponseLimit {
			return nil, transportError{Code: "response_too_large", Message: "supervisor response is oversized"}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, transportError{Code: "operation_failed", Message: "supervisor response could not be read"}
		}
	}
	return raw, nil
}

func parseSupervisorResponse(raw []byte, requestID string) (map[string]interface{}, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var response map[string]interface{}
	if err := decoder.Decode(&response); err != nil {
		return nil, transportError{Code: "invalid_response", Message: "supervisor response is invalid"}
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, transportError{Code: "invalid_response", Message: "supervisor response is invalid"}
	}
	actualID, idOK := response["request_id"].(string)
	if !idOK || actualID != requestID {
		return nil, transportError{Code: "invalid_response", Message: "supervisor response is invalid"}
	}
	if response["ok"] != true {
		errorObject, ok := response["error"].(map[string]interface{})
		if !ok {
			return nil, transportError{Code: "invalid_response", Message: "supervisor response is invalid"}
		}
		code, codeOK := errorObject["code"].(string)
		message, messageOK := errorObject["message"].(string)
		if !codeOK || !messageOK {
			return nil, transportError{Code: "invalid_response", Message: "supervisor response is invalid"}
		}
		return nil, transportError{Code: code, Message: message}
	}
	result, ok := response["result"].(map[string]interface{})
	if !ok {
		return nil, transportError{Code: "invalid_response", Message: "supervisor result is invalid"}
	}
	return result, nil
}

func integerResult(value interface{}) (int64, bool) {
	switch typed := value.(type) {
	case json.Number:
		parsed, err := typed.Int64()
		return parsed, err == nil
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	default:
		return 0, false
	}
}
