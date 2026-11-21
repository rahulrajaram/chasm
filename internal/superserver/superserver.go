// Package superserver implements the unprivileged supervisor service that
// forwards sandbox lifecycle operations to the executor over a Unix socket
// and persists performance samples under its state root.
package superserver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"

	"github.com/rahulrajaram/chasm/internal/execserver"
	"github.com/rahulrajaram/chasm/internal/executor"
	"github.com/rahulrajaram/chasm/internal/protocol"
	"github.com/rahulrajaram/chasm/internal/siteconfig"
)

const (
	maxResponseBytes = 2 * 1024 * 1024
)

var responseWriteTimeout = 5 * time.Second

// Error mirrors SupervisorError: a delivered supervisor disposition.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code }

func supervisorError(code, message string) *Error { return &Error{Code: code, Message: message} }

// Supervisor is the request handler, mirroring the Python Supervisor class.
type Supervisor struct {
	executorSocket string
	stateRoot      string
	// executorCallFn is the transport seam: Dispatch sends every executor
	// payload through it, so tests can capture the exact payload forwarded
	// across the supervisor→executor boundary.
	executorCallFn func(payload map[string]interface{}) (map[string]interface{}, error)
}

// New builds a Supervisor bound to the given executor socket (fail-closed
// until bound, exactly like the Python Supervisor's call_executor).
func New(stateRoot string, executorSocket string) *Supervisor {
	supervisor := &Supervisor{executorSocket: executorSocket, stateRoot: stateRoot}
	supervisor.executorCallFn = supervisor.callExecutor
	return supervisor
}

func executorRequestID(requestID, operation string) string {
	digest := sha256.Sum256([]byte(requestID + ":" + operation))
	return "supervisor-" + hex.EncodeToString(digest[:])
}

// executorCall mirrors call_executor: forward a payload to the executor
// socket and decode its response with the Go strict client.
func (s *Supervisor) executorCall(payload map[string]interface{}) (map[string]interface{}, error) {
	return s.executorCallFn(payload)
}

func (s *Supervisor) callExecutor(payload map[string]interface{}) (map[string]interface{}, error) {
	if s.executorSocket == "" {
		return nil, supervisorError("operation_failed", "executor transport is not configured")
	}
	result, err := executor.Call(s.executorSocket, payload)
	if err != nil {
		var responseError *protocol.ResponseError
		var decodeError protocol.DecoderError
		switch {
		case errors.As(err, &responseError):
			return nil, supervisorError(responseError.Code, responseError.Message)
		case errors.As(err, &decodeError):
			return nil, supervisorError("operation_failed", "executor request failed closed: "+decodeError.Code)
		default:
			return nil, supervisorError("operation_failed", "executor is unavailable")
		}
	}
	return result, nil
}

// executorOperationFor binds one supervisor control operation to its exact
// executor operation. Unknown operations fail closed at the dispatch site.
func executorOperationFor(operation string) (string, bool) {
	binding, ok := map[string]string{
		"create_sandbox":          "create_sandbox",
		"describe_sandbox":        "describe_sandbox",
		"set_sandbox_description": "set_sandbox_description",
		"inspect_sandbox":         "inspect_sandbox",
		"start_sandbox":           "ensure_guest_running",
		"stop_sandbox":            "stop_guest",
		"pause_sandbox":           "pause_guest",
		"resume_sandbox":          "resume_guest",
		"delete_sandbox":          "delete_sandbox",
		"admit_launch":            "admit_launch",
		"resize_sandbox":          "resize_sandbox",
	}[operation]
	return binding, ok
}

// optionalExecutorFields is the one per-operation table of optional fields
// the supervisor forwards verbatim to the executor: every field ParsePublic
// accepts for an operation is carried across the boundary, so a flag can
// never be silently dropped at this hop (create forwards its budget fields;
// resize forwards all three targets).
var optionalExecutorFields = map[string][]string{
	"create_sandbox": {"budget_memory_mib", "budget_cpu"},
	"resize_sandbox": {"budget_memory_mib", "budget_cpu", "root_disk_mib"},
}

// Dispatch mirrors Supervisor.dispatch.
func (s *Supervisor) Dispatch(payload map[string]interface{}) (map[string]interface{}, error) {
	operation, _ := payload["operation"].(string)
	requestID, _ := payload["request_id"].(string)
	switch operation {
	case "sandbox_status":
		return s.executorCall(map[string]interface{}{
			"version":    protocol.ProtocolVersion,
			"request_id": executorRequestID(requestID, "inspect"),
			"operation":  "inspect_default_sandbox",
		})
	case "create_sandbox", "describe_sandbox", "set_sandbox_description", "inspect_sandbox", "start_sandbox", "stop_sandbox", "pause_sandbox", "resume_sandbox", "delete_sandbox", "admit_launch", "resize_sandbox":
		executorOperation, known := executorOperationFor(operation)
		if !known {
			return nil, supervisorError("operation_failed", "sandbox operation has no executor binding")
		}
		executorPayload := map[string]interface{}{
			"version":    protocol.ProtocolVersion,
			"request_id": executorRequestID(requestID, operation),
			"operation":  executorOperation,
			"instance":   payload["instance"],
		}
		if operation == "set_sandbox_description" {
			executorPayload["description"] = payload["description"]
		}
		// Only the fields the supervisor request carried are forwarded;
		// the executor's parser rejects a create/resize missing its rules.
		for _, field := range optionalExecutorFields[operation] {
			if _, present := payload[field]; present {
				executorPayload[field] = payload[field]
			}
		}
		return s.executorCall(executorPayload)
	case "list_sandboxes", "list_repositories":
		return s.executorCall(map[string]interface{}{
			"version":    protocol.ProtocolVersion,
			"request_id": executorRequestID(requestID, operation),
			"operation":  operation,
		})
	case "sandbox_layout":
		return s.executorCall(map[string]interface{}{
			"version":    protocol.ProtocolVersion,
			"request_id": executorRequestID(requestID, operation),
			"operation":  operation,
			"instance":   payload["instance"],
			"depth":      payload["depth"],
		})
	case "locate_repository":
		return s.executorCall(map[string]interface{}{
			"version":    protocol.ProtocolVersion,
			"request_id": executorRequestID(requestID, operation),
			"operation":  operation,
			"repository": payload["repository"],
		})
	case "ensure_guest":
		instance, err := defaultGuestInstance()
		if err != nil {
			return nil, err
		}
		return s.executorCall(map[string]interface{}{
			"version":    protocol.ProtocolVersion,
			"request_id": executorRequestID(requestID, "ensure"),
			"operation":  "ensure_guest_running",
			"instance":   instance,
		})
	case "stop_guest":
		instance, err := defaultGuestInstance()
		if err != nil {
			return nil, err
		}
		return s.executorCall(map[string]interface{}{
			"version":    protocol.ProtocolVersion,
			"request_id": executorRequestID(requestID, "stop"),
			"operation":  "stop_guest",
			"instance":   instance,
		})
	case "collect_performance":
		result, err := s.executorCall(map[string]interface{}{
			"version":    protocol.ProtocolVersion,
			"request_id": executorRequestID(requestID, "perf"),
			"operation":  "collect_performance",
			"phase":      payload["phase"],
		})
		if err != nil {
			return nil, err
		}
		sampleName := fmt.Sprintf("%s-%s.json", payload["phase"], requestID)
		path := s.stateRoot + "/performance/" + sampleName
		if err := os.MkdirAll(s.stateRoot+"/performance", 0o700); err != nil {
			return nil, supervisorError("unsafe_state", "performance root could not be created")
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			return nil, supervisorError("unsafe_state", "performance sample could not be encoded")
		}
		flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL | syscall.O_CLOEXEC | syscall.O_NOFOLLOW
		file, err := os.OpenFile(path, flags, 0o600)
		if err != nil {
			return nil, supervisorError("unsafe_state", "performance sample could not be written")
		}
		_, writeErr := file.Write(append(encoded, '\n'))
		syncErr := file.Sync()
		closeErr := file.Close()
		if writeErr != nil || syncErr != nil || closeErr != nil {
			return nil, supervisorError("unsafe_state", "performance sample write failed")
		}
		return map[string]interface{}{"stored": true, "sample_name": sampleName, "sample": result}, nil
	default:
		return nil, supervisorError("unknown_operation", "operation is not permitted")
	}
}

// HandleConnection mirrors supervisor handle_connection.
func (s *Supervisor) HandleConnection(connection *net.UnixConn, allowedUIDs map[int]bool) {
	_, peerUID, _, err := execserver.PeerCredentials(connection)
	if err != nil {
		_ = writeFrame(connection, mustEncodeError(nil, "operation_failed", "supervisor connection failed"))
		return
	}
	if !allowedUIDs[peerUID] {
		_ = writeFrame(connection, mustEncodeError(nil, "unauthorized_peer", "peer is not an authorized operator"))
		return
	}
	raw, descriptors, readErr := execserver.ReadRequestBounded(connection, protocol.PublicMaxRequestBytes)
	var payload map[string]interface{}
	var requestID *string
	if readErr != nil {
		payload = nil
	} else {
		request, parseErr := protocol.ParsePublic(raw)
		if parseErr != nil {
			readErr = parseErr
		} else {
			value := request.RequestID()
			requestID = &value
			payload = request.Fields
		}
	}
	if readErr != nil {
		code, message := codeOf(readErr)
		_ = writeFrame(connection, mustEncodeError(nil, code, message))
		execserver.CloseDescriptors(descriptors)
		return
	}
	if len(descriptors) != 0 {
		_ = writeFrame(connection, mustEncodeError(requestID, "invalid_descriptor", "descriptor count is invalid"))
		execserver.CloseDescriptors(descriptors)
		return
	}
	result, dispatchErr := s.Dispatch(payload)
	execserver.CloseDescriptors(descriptors)
	if dispatchErr != nil {
		code, message, ok := asDelivered(dispatchErr)
		if !ok {
			message = "supervisor operation failed closed"
		}
		_ = writeFrame(connection, mustEncodeError(requestID, code, message))
		return
	}
	_ = writeFrame(connection, mustEncodeSuccess(*requestID, result))
}

// Serve runs the accept loop, mirroring the Python supervisor main loop.
func (s *Supervisor) Serve(listener *net.UnixListener, allowedUIDs map[int]bool) error {
	for {
		connection, err := listener.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		s.HandleConnection(connection, allowedUIDs)
		_ = connection.Close()
	}
}

func writeFrame(conn interface {
	Write([]byte) (int, error)
	SetWriteDeadline(time.Time) error
}, frame []byte) error {
	if err := conn.SetWriteDeadline(time.Now().Add(responseWriteTimeout)); err != nil {
		return err
	}
	written, err := conn.Write(frame)
	if err != nil {
		return err
	}
	if written < len(frame) {
		return errors.New("short write of supervisor response")
	}
	return nil
}

func encodeBounded(payload map[string]interface{}) ([]byte, bool) {
	encoded, err := execserver.CompactJSON(payload)
	if err != nil || len(encoded) > maxResponseBytes {
		return nil, false
	}
	return encoded, true
}

func mustEncodeSuccess(requestID string, result map[string]interface{}) []byte {
	frame, ok := encodeBounded(map[string]interface{}{
		"version":    protocol.ProtocolVersion,
		"request_id": requestID,
		"ok":         true,
		"result":     result,
	})
	if !ok {
		return mustEncodeError(&requestID, "response_too_large", "supervisor response is oversized")
	}
	return frame
}

func mustEncodeError(requestID *string, code, message string) []byte {
	frame, ok := encodeBounded(map[string]interface{}{
		"version":    protocol.ProtocolVersion,
		"request_id": nilToNull(requestID),
		"ok":         false,
		"error": map[string]interface{}{
			"code":    code,
			"message": message,
		},
	})
	if !ok {
		return []byte("{\"error\":{\"code\":\"response_too_large\",\"message\":\"supervisor response is oversized\"},\"ok\":false,\"request_id\":null,\"version\":1}\n")
	}
	return frame
}

func nilToNull(requestID *string) interface{} {
	if requestID == nil {
		return nil
	}
	return *requestID
}

func codeOf(err error) (string, string) {
	var peerError *Error
	if errors.As(err, &peerError) {
		return peerError.Code, peerError.Message
	}
	var frameError *execserver.ProtocolError
	if errors.As(err, &frameError) {
		return frameError.Code, frameError.Message
	}
	var decodeError protocol.DecoderError
	if errors.As(err, &decodeError) {
		if decodeError.Message != "" {
			return decodeError.Code, decodeError.Message
		}
		return decodeError.Code, "supervisor request is invalid"
	}
	return "operation_failed", "supervisor operation failed closed"
}

func asDelivered(err error) (string, string, bool) {
	var peerError *Error
	if errors.As(err, &peerError) {
		return peerError.Code, peerError.Message, true
	}
	return "", "", false
}

// defaultGuestInstance is the managed instance the default-guest operations
// address: the site's default sandbox. An empty default fails closed naming
// the missing key rather than addressing an unnamed guest.
func defaultGuestInstance() (string, *Error) {
	instance := siteconfig.Active().DefaultInstanceName()
	if instance == "" {
		return "", supervisorError("missing_default_sandbox", "site config key default_sandbox is unset; cannot address an unnamed guest")
	}
	return instance, nil
}

// Account lookup seam: the installer's preflight accepts any operator
// account the host's NSS resolves (`getent passwd`), so runtime
// authorization uses the same lookup contract — not a flat
// /etc/passwd scan, which would silently drop NSS-only (SSSD/LDAP)
// operators from the allowed-UID set. The default implementation is
// NSS-aware WITHOUT cgo: it shells out to the fixed getent executable the
// core preflight already uses (see account.go), with an explicit argv
// vector, a wall-clock bound, and a strictly validating passwd-record
// parser, so a CGO_ENABLED=0 static build still resolves NSS-only
// operators. Tests inject this seam to simulate NSS-only operators and
// lookup failures without touching the host's account database.
// Note the contract is the lookup mechanism, not its environment: the
// installed supervisor unit permits only Unix-domain sockets
// (RestrictAddressFamilies=AF_UNIX), so network-backed NSS resolves at
// runtime only through a local Unix-socket proxy whose socket is
// accessible under the full unit; direct network-socket backends pass
// preflight (which runs outside the unit) but fail closed here.
var lookupOperatorAccount = getentAccountLookup

// OperatorGID returns the operator user's primary group id, resolved
// through the host's NSS-aware account lookup. A lookup failure is a
// clear error, never a silent zero.
func OperatorGID() (int, error) {
	operator := siteconfig.Active().OperatorUser
	_, gid, err := lookupOperatorAccount(operator)
	if err != nil {
		return 0, fmt.Errorf("operator user %s could not be resolved through the system account lookup (NSS): %w", operator, err)
	}
	return gid, nil
}

// AllowedOperatorUIDs mirrors the supervisor main: root and the operator,
// with the operator resolved through the same NSS-aware lookup the
// installer's preflight uses. Root is admitted regardless of the
// operator's resolution (peer-UID authorization itself is unchanged); an
// unresolvable operator is a startup error, not a silent lockout.
func AllowedOperatorUIDs() (map[int]bool, error) {
	operator := siteconfig.Active().OperatorUser
	uid, _, err := lookupOperatorAccount(operator)
	if err != nil {
		return nil, fmt.Errorf("operator user %s could not be resolved through the system account lookup (NSS): %w", operator, err)
	}
	return map[int]bool{0: true, uid: true}, nil
}
