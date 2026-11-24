package guestexec

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestResultExitCodes(t *testing.T) {
	value := func(n int) *int { return &n }
	for _, test := range []struct {
		name   string
		result Result
		want   int
	}{
		{"guest 255", Result{GuestStatus: value(255)}, 255},
		{"guest 126", Result{GuestStatus: value(126)}, 126},
		{"guest 127", Result{GuestStatus: value(127)}, 127},
		{"complete zero", Result{GuestStatus: value(0)}, 0},
		{"zero incomplete", Result{GuestStatus: value(0), OutputCode: "output_incomplete"}, 125},
		{"nonzero incomplete", Result{GuestStatus: value(7), OutputCode: "output_incomplete"}, 7},
		{"Incus 129", Result{Disposition: "outcome_unknown", IncusReturn: value(129)}, 124},
		{"setup", Rejected("setup_failed"), 126},
		{"rejected", Rejected("target_missing"), 125},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := test.result.ExitCode(); got != test.want {
				t.Fatalf("ExitCode() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestDecodeRequestRejectsNoncanonicalAndDuplicateFields(t *testing.T) {
	request := Request{Name: "demo", Cwd: "/", Environment: map[string]string{}, Argv: []string{"true"}}
	canonical, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	for _, payload := range [][]byte{
		append([]byte(" "), canonical...),
		[]byte(`{"name":"demo","name":"other","cwd":"/","environment":{},"argv":["true"]}`),
		[]byte(`{"name":"demo","cwd":"/","environment":{},"argv":["true"],"extra":1}`),
	} {
		if _, err := DecodeRequest(payload); err == nil {
			t.Fatalf("accepted %q", payload)
		}
	}
}

func TestFrameWriterWaitsForSlowConsumerAndClosesOnResult(t *testing.T) {
	reader, writer := net.Pipe()
	defer reader.Close()
	defer writer.Close()
	frames := &FrameWriter{Writer: writer}
	done := make(chan error, 1)
	go func() { done <- frames.WriteFrame(StdoutFrame, []byte("hello")) }()
	select {
	case err := <-done:
		t.Fatalf("write completed before consumer resumed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if kind, payload, err := ReadFrame(reader); err != nil || kind != StdoutFrame || string(payload) != "hello" {
		t.Fatalf("frame %d %q %v", kind, payload, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	resultDone := make(chan error, 1)
	go func() { resultDone <- frames.WriteJSON(ResultFrame, Rejected("done")) }()
	if kind, _, err := ReadFrame(reader); err != nil || kind != ResultFrame {
		t.Fatalf("result frame %d %v", kind, err)
	}
	if err := <-resultDone; err != nil {
		t.Fatal(err)
	}
	if err := frames.WriteFrame(StdoutFrame, []byte("late")); err == nil {
		t.Fatal("wrote output after result")
	}
}

func TestWebsocketReaderRejectsUnsafeFrames(t *testing.T) {
	for _, test := range []struct {
		name  string
		frame []byte
	}{
		{"masked", []byte{0x82, 0x80}},
		{"RSV", []byte{0xc2, 0}},
		{"control oversized", []byte{0x89, 126, 0, 126}},
		{"64 bit length", []byte{0x82, 127, 0, 0, 0, 0, 0, 1, 0, 0}},
		{"above MaxChunk", []byte{0x82, 126, 0x80, 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ws := &websocket{reader: bufio.NewReader(bytes.NewReader(test.frame))}
			if _, _, _, err := ws.read(); err == nil || err == io.EOF {
				t.Fatalf("accepted frame: %v", err)
			}
		})
	}
}

func TestWebsocketWriteRejectsOversizedPayload(t *testing.T) {
	ws := &websocket{}
	if err := ws.write(wsBinary, []byte(strings.Repeat("x", 65536))); err == nil {
		t.Fatal("accepted payload that cannot fit the 16-bit frame length")
	}
}
