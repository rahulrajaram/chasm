package guestexec

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

const testOperationID = "52458fac-20f3-408d-a3ca-55fc8ca29d1a"

type fakeIncus struct {
	t                *testing.T
	command          chan execPost
	stdin            chan []byte
	opened           chan byte
	handshakes       chan struct{}
	dropRelease      chan struct{}
	signals          chan int
	finish           chan struct{}
	exit             int
	operationFailure string
	dropOutput       bool
	stallOutput      bool
	once             sync.Once
}

func newFakeIncus(t *testing.T) *fakeIncus {
	return &fakeIncus{
		t: t, command: make(chan execPost, 1), stdin: make(chan []byte, 1), opened: make(chan byte, 4),
		handshakes: make(chan struct{}, 4), dropRelease: make(chan struct{}),
		signals: make(chan int, 3), finish: make(chan struct{}),
	}
}

func (fake *fakeIncus) terminate() { fake.once.Do(func() { close(fake.finish) }) }

func (fake *fakeIncus) dial(ctx context.Context) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	client, server := net.Pipe()
	go fake.serve(server)
	return &handshakeConn{Conn: client, handshakes: fake.handshakes}, nil
}

type handshakeConn struct {
	net.Conn
	handshakes chan<- struct{}
}

func (conn *handshakeConn) SetDeadline(deadline time.Time) error {
	err := conn.Conn.SetDeadline(deadline)
	if err == nil && deadline.IsZero() {
		conn.handshakes <- struct{}{}
	}
	return err
}

func writeTestJSON(conn net.Conn, status string, body any) {
	payload, _ := json.Marshal(body)
	_, _ = fmt.Fprintf(conn, "HTTP/1.1 %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", status, len(payload), payload)
}

func (fake *fakeIncus) serve(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	reader := bufio.NewReader(conn)
	request, err := http.ReadRequest(reader)
	if err != nil {
		return
	}
	switch request.URL.Path {
	case "/1.0/instances/sandbox-test/exec":
		var post execPost
		if err := json.NewDecoder(request.Body).Decode(&post); err != nil {
			fake.t.Error(err)
			return
		}
		fake.command <- post
		fds := map[string]string{}
		for key, char := range map[string]string{"control": "a", "0": "b", "1": "c", "2": "d"} {
			fds[key] = strings.Repeat(char, 64)
		}
		writeTestJSON(conn, "202 Accepted", map[string]any{
			"type": "async", "status_code": 100, "operation": "/1.0/operations/" + testOperationID,
			"metadata": map[string]any{"id": testOperationID, "metadata": map[string]any{"fds": fds}},
		})
	case "/1.0/operations/" + testOperationID + "/wait":
		<-fake.finish
		status := 200
		if fake.operationFailure != "" {
			status = 400
		}
		writeTestJSON(conn, "200 OK", map[string]any{
			"type": "sync", "status_code": 200,
			"metadata": map[string]any{"id": testOperationID, "status_code": status, "err": fake.operationFailure, "metadata": map[string]any{"return": fake.exit}},
		})
	case "/1.0/operations/" + testOperationID + "/websocket":
		secret := request.URL.Query().Get("secret")
		if len(secret) != 64 {
			fake.t.Error("missing socket secret")
			return
		}
		fake.opened <- secret[0]
		digest := sha1.Sum([]byte(request.Header.Get("Sec-WebSocket-Key") + wsMagic))
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(digest[:]))
		fake.socket(conn, reader, secret[0])
	default:
		fake.t.Errorf("unexpected Incus path %s", request.URL.Path)
	}
}

func (fake *fakeIncus) socket(conn net.Conn, reader io.Reader, channel byte) {
	switch channel {
	case 'a':
		for {
			op, payload, err := readClientFrame(reader)
			if err != nil {
				return
			}
			if op != wsText {
				fake.t.Errorf("unexpected control frame %d", op)
				return
			}
			var signal struct {
				Command string
				Signal  int
			}
			if json.Unmarshal(payload, &signal) != nil || signal.Command != "signal" {
				fake.t.Errorf("invalid control packet %q", payload)
				return
			}
			fake.signals <- signal.Signal
			if signal.Signal == 9 {
				fake.terminate()
			}
		}
	case 'b':
		var input []byte
		for {
			op, payload, err := readClientFrame(reader)
			if err != nil {
				return
			}
			if op == wsText && len(payload) == 0 {
				fake.stdin <- input
				return
			}
			if op != wsBinary {
				fake.t.Errorf("unexpected stdin frame %d", op)
				return
			}
			input = append(input, payload...)
		}
	case 'c', 'd':
		<-fake.finish
		if fake.stallOutput {
			_, _ = io.Copy(io.Discard, reader)
			return
		}
		if fake.dropOutput {
			<-fake.dropRelease
			return
		}
		if channel == 'c' {
			_, _ = conn.Write([]byte{wsBinary, 2, 'a', 0, 0x80 | wsPing, 1, 'p'})
			op, payload, err := readClientFrame(reader)
			if err != nil || op != wsPong || string(payload) != "p" {
				fake.t.Errorf("ping response %d %q %v", op, payload, err)
			}
			_, _ = conn.Write([]byte{0x80 | wsContinuation, 1, 'b'})
		} else {
			_, _ = conn.Write([]byte{0x80 | wsBinary, 2, 0xff, '!'})
		}
		_, _ = conn.Write([]byte{0x80 | wsText, 0})
	}
}

func readClientFrame(reader io.Reader) (byte, []byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return 0, nil, err
	}
	if header[0]&0x80 == 0 || header[1]&0x80 == 0 {
		return 0, nil, fmt.Errorf("client frame not final or masked")
	}
	size := int(header[1] & 0x7f)
	if size == 126 {
		var extended [2]byte
		if _, err := io.ReadFull(reader, extended[:]); err != nil {
			return 0, nil, err
		}
		size = int(binary.BigEndian.Uint16(extended[:]))
	}
	var mask [4]byte
	if _, err := io.ReadFull(reader, mask[:]); err != nil {
		return 0, nil, err
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return 0, nil, err
	}
	for index := range payload {
		payload[index] ^= mask[index%4]
	}
	return header[0] & 0x0f, payload, nil
}

func TestNativeExecSeparatesStreamsAndPreservesGuestStatus(t *testing.T) {
	for _, value := range []int{0, 1, 124, 125, 126, 127, 129, 255} {
		t.Run(fmt.Sprint(value), func(t *testing.T) {
			fake := newFakeIncus(t)
			fake.exit = value
			if value == 126 {
				fake.operationFailure = "Command not executable"
			}
			if value == 127 {
				fake.operationFailure = "Command not found"
			}
			client := newClient(fake.dial)
			input := make(chan []byte, 1)
			input <- bytes.Repeat([]byte{0xff, 0}, 256)
			close(input)
			got := make(chan []byte, 1)
			go func() { got <- <-fake.stdin; fake.terminate() }()
			var stdout, stderr bytes.Buffer
			request := Request{Argv: []string{"app", "", "x; y"}, Cwd: "/workspace", Environment: map[string]string{"KEY": "guest=value"}}
			result := client.Execute(context.Background(), "sandbox-test", request, 3456, input, func(kind byte, payload []byte) error {
				if kind == StdoutFrame {
					_, _ = stdout.Write(payload)
				} else {
					_, _ = stderr.Write(payload)
				}
				return nil
			})
			if value == 129 {
				if result.GuestStatus != nil || result.IncusReturn == nil || *result.IncusReturn != 129 || result.ExitCode() != 124 {
					t.Fatalf("ambiguous status: %+v", result)
				}
			} else if result.GuestStatus == nil || *result.GuestStatus != value || result.ExitCode() != value {
				t.Fatalf("guest status expected %d, got %+v", value, result)
			}
			for _, expected := range []byte("abcd") {
				if got := <-fake.opened; got != expected {
					t.Fatalf("channel order %c != %c", got, expected)
				}
			}
			post := <-fake.command
			if post.User != 3456 || post.Group != 3456 || post.Interactive || !post.WaitForWS || post.Cwd != "/workspace" ||
				len(post.Command) != 3 || post.Command[1] != "" || post.Command[2] != "x; y" || post.Environment["KEY"] != "guest=value" ||
				post.Environment["HOME"] != "/home/agent" {
				t.Fatalf("unexpected Incus request %+v", post)
			}
			if !bytes.Equal(<-got, bytes.Repeat([]byte{0xff, 0}, 256)) || !bytes.Equal(stdout.Bytes(), []byte{'a', 0, 'b'}) ||
				!bytes.Equal(stderr.Bytes(), []byte{0xff, '!'}) {
				t.Fatal("streams were changed or merged")
			}
		})
	}
}

func TestNativeExecCancellationEscalates(t *testing.T) {
	fake := newFakeIncus(t)
	fake.exit = 137
	client := newClient(fake.dial)
	client.grace = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	input := make(chan []byte)
	done := make(chan Result, 1)
	go func() {
		done <- client.Execute(ctx, "sandbox-test", Request{Argv: []string{"app"}, Cwd: "/"}, 3456, input, func(byte, []byte) error { return nil })
	}()
	for range 4 {
		<-fake.opened
	}
	cancel()
	select {
	case result := <-done:
		if result.Disposition != "interrupted" || result.GuestStatus == nil || *result.GuestStatus != 137 || result.SignalSent != 9 {
			t.Fatalf("unexpected cancellation result %+v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("remote cancellation did not finish")
	}
	if term, kill := <-fake.signals, <-fake.signals; term != 15 || kill != 9 {
		t.Fatalf("sent signals %d then %d", term, kill)
	}
}

func TestNativeExecDoesNotTrustSuccessWithoutOutputBarrier(t *testing.T) {
	fake := newFakeIncus(t)
	fake.dropOutput = true
	fake.terminate()
	go func() {
		for range 4 {
			<-fake.handshakes
		}
		close(fake.dropRelease)
	}()
	input := make(chan []byte)
	close(input)
	result := newClient(fake.dial).Execute(context.Background(), "sandbox-test", Request{Argv: []string{"app"}, Cwd: "/"}, 3456, input, func(byte, []byte) error { return nil })
	if result.GuestStatus == nil || *result.GuestStatus != 0 || result.OutputCode != "output_incomplete" || result.ExitCode() != 125 {
		t.Fatalf("guest status and output integrity were not separated: %+v", result)
	}
}

func TestNativeExecOutputErrorDoesNotCancelGuestUnlessPeerClosed(t *testing.T) {
	fake := newFakeIncus(t)
	fake.terminate()
	input := make(chan []byte)
	close(input)
	result := newClient(fake.dial).Execute(context.Background(), "sandbox-test", Request{Argv: []string{"app"}, Cwd: "/"}, 3456, input,
		func(byte, []byte) error { return errors.New("output disk full") })
	if result.GuestStatus == nil || *result.GuestStatus != 0 || result.OutputCode != "output_incomplete" || result.Disposition != "completed" || result.ExitCode() != 125 {
		t.Fatalf("output error changed guest completion: %+v", result)
	}
	select {
	case signal := <-fake.signals:
		t.Fatalf("output error sent guest signal %d", signal)
	default:
	}
}

func TestNativeExecBoundsOutputDrainAfterOperationCompletes(t *testing.T) {
	fake := newFakeIncus(t)
	fake.stallOutput = true
	fake.exit = 7
	fake.terminate()
	client := newClient(fake.dial)
	client.drain = 20 * time.Millisecond
	input := make(chan []byte)
	close(input)
	result := client.Execute(context.Background(), "sandbox-test", Request{Argv: []string{"app"}, Cwd: "/"}, 3456, input, func(byte, []byte) error { return nil })
	if result.GuestStatus == nil || *result.GuestStatus != 7 || result.OutputCode != "output_incomplete" {
		t.Fatalf("did not report incomplete stream delivery: %+v", result)
	}
}
