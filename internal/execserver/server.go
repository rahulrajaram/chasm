package execserver

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"

	"github.com/rahulrajaram/chasm/internal/protocol"
)

// Dispatcher mirrors Python's operation_dispatch: one request yields one
// result map, or a *ProtocolError (delivered disposition) / other error
// (operation_failed).
type Dispatcher func(request protocol.Request) (map[string]interface{}, error)

// AuditFunc persists one mandatory request disposition. errorCode is empty on
// success. A nil audit is permitted only by tests; production audit failures
// are surfaced to the peer as a generic fail-closed operation error.
type AuditFunc func(request *protocol.Request, peerUID int, ok bool, errorCode string) error

// readRequest mirrors _read_request: a bounded, newline-terminated single
// request with SCM_RIGHTS descriptor collection. Errors carry a
// wire-stable code: invalid_frame / invalid_size / invalid_descriptor, or
// operation_failed on a receive timeout (Python's socket.timeout lands in
// the operation_failed path), and idle EOF is invalid_frame.
func ReadRequest(connection *net.UnixConn) ([]byte, []*os.File, error) {
	return readRequestBounded(connection, protocol.ExecutorMaxRequestBytes)
}

func readRequest(connection *net.UnixConn) ([]byte, []*os.File, error) {
	return readRequestBounded(connection, protocol.ExecutorMaxRequestBytes)
}

// ReadRequestBounded reads one request frame using the caller's protocol
// limit. The executor uses its 16 KiB surface while the public supervisor
// accepts requests up to PublicMaxRequestBytes.
func ReadRequestBounded(connection *net.UnixConn, maxRequestBytes int) ([]byte, []*os.File, error) {
	return readRequestBounded(connection, maxRequestBytes)
}

func readRequestBounded(connection *net.UnixConn, maxRequestBytes int) ([]byte, []*os.File, error) {
	// Ancillary buffer sized for two int32 SCM_RIGHTS fds, matching
	// socket.CMSG_SPACE(integers.itemsize * 2).
	ancillary := make([]byte, syscall.CmsgSpace(4*2))
	var chunks []byte
	var descriptors []*os.File
	size := 0
	bounded := func(room int) int {
		if room > 4096 {
			return 4096
		}
		return room
	}
	for {
		if err := connection.SetReadDeadline(time.Now().Add(serverTimeout * time.Second)); err != nil {
			return nil, descriptors, protocolError("invalid_frame", "request must end with a newline")
		}
		var read int
		var err error
		var flags int
		if len(chunks) == 0 {
			buffer := make([]byte, bounded(maxRequestBytes+1))
			var raw []byte
			read, _, flags, _, err = connection.ReadMsgUnix(buffer, ancillary)
			raw = buffer[:read]
			if err == nil && read > 0 {
				descriptors = collectRights(ancillary, descriptors)
			}
			if len(raw) > 0 {
				chunks = append(chunks, raw...)
				size += len(raw)
			}
			if flags&syscall.MSG_CTRUNC != 0 {
				return nil, descriptors, protocolError("invalid_descriptor", "descriptor frame is truncated")
			}
		} else {
			buffer := make([]byte, bounded(maxRequestBytes+1-size))
			read, err = connection.Read(buffer)
			if read > 0 {
				chunks = append(chunks, buffer[:read]...)
				size += read
			}
		}
		if size > maxRequestBytes {
			return nil, descriptors, protocolError("invalid_size", "request exceeds its byte limit")
		}
		if marker := bytes.IndexByte(chunks, '\n'); marker >= 0 {
			if marker != len(chunks)-1 {
				return nil, descriptors, protocolError("invalid_frame", "only one request is permitted")
			}
			return chunks[:marker], descriptors, nil
		}
		if err != nil {
			if os.IsTimeout(err) {
				return nil, descriptors, protocolError("operation_failed", "executor operation failed closed")
			}
			// A peer that hangs up mid-frame (any non-timeout read error,
			// end-of-file included) leaves a terminated frame: fail closed
			// instead of spinning on an immediate EOF with bytes buffered.
			return nil, descriptors, protocolError("invalid_frame", "request must end with a newline")
		}
	}
}

func collectRights(ancillary []byte, descriptors []*os.File) []*os.File {
	cmsgs, err := syscall.ParseSocketControlMessage(ancillary)
	if err != nil {
		return descriptors
	}
	for _, cmsg := range cmsgs {
		if cmsg.Header.Level != syscall.SOL_SOCKET || cmsg.Header.Type != syscall.SCM_RIGHTS {
			continue
		}
		rights, err := syscall.ParseUnixRights(&cmsg)
		if err != nil {
			continue
		}
		for _, fd := range rights {
			descriptors = append(descriptors, os.NewFile(uintptr(fd), "peer-scm-rights"))
		}
	}
	return descriptors
}

func CloseDescriptors(descriptors []*os.File) { closeDescriptors(descriptors) }

func closeDescriptors(descriptors []*os.File) {
	for _, descriptor := range descriptors {
		if descriptor != nil {
			_ = descriptor.Close()
		}
	}
}

// HandleConnection mirrors handle_connection: peer-credential gate,
// bounded read, descriptor count check (an executor request carries no
// descriptors), dispatch, audit, and a byte-parity response frame.
func HandleConnection(connection *net.UnixConn, expectedUID int, dispatch Dispatcher, audit AuditFunc) {
	_, peerUID, _, err := peerCredentials(connection)
	if err != nil {
		_ = writeFrame(connection, mustEncodeError(nil, "operation_failed", "executor connection failed"))
		return
	}
	if peerUID != expectedUID {
		_ = writeFrame(connection, mustEncodeError(nil, "unauthorized_peer", "peer is not the sandbox supervisor"))
		return
	}

	raw, descriptors, readErr := readRequest(connection)
	var parsed *protocol.Request
	var requestID *string
	if readErr != nil {
		parsed = nil
	} else {
		item, parseErr := protocol.ParseExecutor(raw)
		if parseErr != nil {
			readErr = parseErr
		} else {
			value := item.RequestID()
			requestID = &value
			parsed = &item
		}
	}
	if readErr != nil {
		code, message, _ := codeOf(readErr)
		_ = writeFrame(connection, mustEncodeError(nil, code, message))
		closeDescriptors(descriptors)
		return
	}

	if len(descriptors) != 0 {
		code, message, _ := codeOf(protocolError("invalid_descriptor", "descriptor count is invalid"))
		if audit != nil {
			if err := audit(parsed, peerUID, false, code); err != nil {
				code, message = "operation_failed", "executor audit persistence failed closed"
			}
		}
		_ = writeFrame(connection, mustEncodeError(requestID, code, message))
		closeDescriptors(descriptors)
		return
	}

	if lifecycleOperation(parsed.Operation) != "" {
		parsed.LifecycleTimings = make(map[string]int64)
	}
	result, dispatchErr := dispatch(*parsed)
	closeDescriptors(descriptors)
	if dispatchErr != nil {
		code, message, isProtocol := codeOf(dispatchErr)
		if !isProtocol {
			message = "executor operation failed closed"
		}
		if audit != nil {
			if err := audit(parsed, peerUID, false, code); err != nil {
				code, message = "operation_failed", "executor audit persistence failed closed"
			}
		}
		_ = writeFrame(connection, mustEncodeError(requestID, code, message))
		return
	}
	if audit != nil {
		if err := audit(parsed, peerUID, true, ""); err != nil {
			_ = writeFrame(connection, mustEncodeError(requestID, "operation_failed", "executor audit persistence failed closed"))
			return
		}
	}
	_ = writeFrame(connection, mustEncodeSuccess(*requestID, result))
}

// Serve runs the accept loop for one listener, mirroring the Python
// service main loop: each connection is handled once and closed. A panic
// inside a handler must never take the service down; it is recovered per
// connection (fail-closed: no response) and logged.
func Serve(listener *net.UnixListener, expectedUID int, dispatch Dispatcher, audit AuditFunc) error {
	for {
		connection, err := listener.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					fmt.Fprintf(os.Stderr, "executor handler panic: %v\n", recovered)
				}
			}()
			HandleConnection(connection, expectedUID, dispatch, audit)
		}()
		_ = connection.Close()
	}
}

// codeOf maps an error to a wire-stable (code, message, isProtocol)
// triple: *ProtocolError and protocol.DecoderError carry their own codes;
// everything else becomes operation_failed (matching Python's catch of
// OSError/SubprocessError/ValueError).
func codeOf(err error) (code, message string, isProtocol bool) {
	var peerError *ProtocolError
	if errors.As(err, &peerError) {
		return peerError.Code, peerError.Message, true
	}
	var decodeError protocol.DecoderError
	if errors.As(err, &decodeError) {
		message := decodeError.Message
		if message == "" {
			message = "executor request is invalid"
		}
		return decodeError.Code, message, false
	}
	return "operation_failed", "executor operation failed closed", false
}
