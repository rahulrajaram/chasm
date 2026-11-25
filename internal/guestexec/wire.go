package guestexec

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"path"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	RequestFrame byte = iota + 1
	InputFrame
	InputEndFrame
	AcceptedFrame
	StdoutFrame
	StderrFrame
	ResultFrame

	MaxChunk   = 32 << 10
	maxRequest = 64 << 10
	MaxArgs    = 256
	MaxValue   = 16 << 10
	MaxEnv     = 64
)

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type Request struct {
	Name        string            `json:"name"`
	Cwd         string            `json:"cwd"`
	Environment map[string]string `json:"environment"`
	Argv        []string          `json:"argv"`
}

func ValidEnvName(name string) bool { return len(name) <= 128 && envName.MatchString(name) }

func validString(value string) bool {
	return len(value) <= MaxValue && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

func (request Request) Validate() error {
	if !validString(request.Cwd) || !path.IsAbs(request.Cwd) {
		return errors.New("exec working directory must be a valid absolute guest path")
	}
	if len(request.Argv) == 0 || len(request.Argv) > MaxArgs || request.Argv[0] == "" {
		return errors.New("exec requires a non-empty command and at most 256 arguments including the command")
	}
	for _, argument := range request.Argv {
		if !validString(argument) {
			return errors.New("exec command argument is invalid or exceeds 16 KiB")
		}
	}
	if len(request.Environment) > MaxEnv {
		return errors.New("exec accepts at most 64 environment entries")
	}
	for key, value := range request.Environment {
		if !ValidEnvName(key) || !validString(value) {
			return errors.New("exec environment entry is invalid or exceeds 16 KiB")
		}
	}
	raw, err := json.Marshal(request)
	if err != nil || len(raw) > maxRequest {
		return errors.New("exec request exceeds 64 KiB")
	}
	return nil
}

type Result struct {
	Version      int              `json:"version"`
	RequestID    string           `json:"request_id,omitempty"`
	Disposition  string           `json:"disposition"`
	Code         string           `json:"code,omitempty"`
	OutputCode   string           `json:"output_code,omitempty"`
	GuestStatus  *int             `json:"guest_status,omitempty"`
	Confirmation string           `json:"confirmation,omitempty"`
	IncusReturn  *int             `json:"incus_return,omitempty"`
	SignalSent   int              `json:"signal_sent,omitempty"`
	Timings      map[string]int64 `json:"timings_ms,omitempty"`
}

func Rejected(code string) Result {
	return Result{Version: 1, Disposition: "rejected", Code: code}
}

func Unknown(code string) Result {
	return Result{Version: 1, Disposition: "outcome_unknown", Code: code}
}

func (result Result) ExitCode() int {
	if result.GuestStatus != nil {
		if *result.GuestStatus == 0 && result.OutputCode != "" {
			return 125
		}
		return *result.GuestStatus
	}
	if result.Disposition == "outcome_unknown" {
		return 124
	}
	if result.Code == "setup_failed" {
		return 126
	}
	return 125
}

func frameLimit(kind byte) int {
	switch kind {
	case RequestFrame:
		return maxRequest
	case InputFrame, StdoutFrame, StderrFrame:
		return MaxChunk
	case ResultFrame:
		return 4096
	case AcceptedFrame:
		return 32
	case InputEndFrame:
		return 0
	default:
		return -1
	}
}

func ReadFrame(reader io.Reader) (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return 0, nil, err
	}
	kind := header[0]
	limit := frameLimit(kind)
	length := binary.BigEndian.Uint32(header[1:])
	if limit < 0 || length > uint32(limit) {
		return 0, nil, errors.New("invalid exec frame")
	}
	payload := make([]byte, length)
	_, err := io.ReadFull(reader, payload)
	return kind, payload, err
}

type FrameWriter struct {
	Writer io.Writer
	mu     sync.Mutex
	closed bool
}

func (writer *FrameWriter) WriteFrame(kind byte, payload []byte) error {
	if limit := frameLimit(kind); limit < 0 || len(payload) > limit {
		return errors.New("invalid exec frame")
	}
	var header [5]byte
	header[0] = kind
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.closed {
		return errors.New("exec output stream is closed")
	}
	if n, err := writer.Writer.Write(header[:]); err != nil {
		return err
	} else if n != len(header) {
		return io.ErrShortWrite
	}
	n, err := writer.Writer.Write(payload)
	if err == nil && n != len(payload) {
		return io.ErrShortWrite
	}
	if err == nil && kind == ResultFrame {
		writer.closed = true
	}
	return err
}

func (writer *FrameWriter) WriteJSON(kind byte, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writer.WriteFrame(kind, payload)
}

func DecodeRequest(payload []byte) (Request, error) {
	var request Request
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if !utf8.Valid(payload) || decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF {
		return Request{}, errors.New("invalid exec request")
	}
	// The public encoder emits this exact representation. Re-encoding also
	// rejects repeated JSON fields and malformed escaped Unicode sequences.
	canonical, err := json.Marshal(request)
	if err != nil || !bytes.Equal(payload, canonical) {
		return Request{}, errors.New("invalid exec request encoding")
	}
	return request, request.Validate()
}
