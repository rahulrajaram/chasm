package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rahulrajaram/chasm/internal/guestexec"
)

func TestParseExecPreservesGuestArguments(t *testing.T) {
	request, fd, err := parseExec([]string{"research", "--cwd", "/workspace/some dir", "--env", "KEY=a=b",
		"--env", "EMPTY=", "--status-fd", "9", "--", "/bin/echo", "", "--help", "a; b", "--"})
	if err != nil || fd != 9 {
		t.Fatalf("parseExec status fd %d, error %v", fd, err)
	}
	if !reflect.DeepEqual(request.Argv, []string{"/bin/echo", "", "--help", "a; b", "--"}) ||
		request.Cwd != "/workspace/some dir" || !reflect.DeepEqual(request.Environment, map[string]string{"KEY": "a=b", "EMPTY": ""}) {
		t.Fatalf("payload changed: %+v", request)
	}
}

func TestParseExecRefusesUnsafeOrUnboundedRequests(t *testing.T) {
	for _, arguments := range [][]string{
		{"sandbox-research", "--", "true"},
		{"research", "true"},
		{"research", "--"},
		{"research", "--", ""},
		{"research", "--", "x\x00y"},
		{"research", "--", "\xff"},
		{"research", "--cwd", "relative", "--", "true"},
		{"research", "--env", "1key=value", "--", "true"},
		{"research", "--env", "DUP=x", "--env", "DUP=y", "--", "true"},
		{"research", "--status-fd", "2", "--", "true"},
		{"research", "--status-fd", "3", "--status-fd", "4", "--", "true"},
		{"research", "--user", "0", "--", "true"},
		{"research", "--", "true", strings.Repeat("x", guestexec.MaxValue+1)},
	} {
		if _, _, err := parseExec(arguments); err == nil {
			t.Errorf("accepted invalid request (argc %d)", len(arguments))
		}
	}
}

func TestExecAuditHasNoArgvTailOrEnvironmentValues(t *testing.T) {
	request := guestexec.Request{Name: "research", Cwd: "/", Environment: map[string]string{"KEY": "secret-env"}, Argv: []string{"shell", "secret-arg", ""}}
	record := newExecAudit(1234, 3456, request)
	payload, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"argv0":"shell"`, `"argc":3`, `"environment_keys":["KEY"]`, `"operator_uid":1234`, `"guest_gid":3456`} {
		if !bytes.Contains(payload, []byte(want)) {
			t.Fatalf("record lacks %q: %s", want, payload)
		}
	}
	if bytes.Contains(payload, []byte("secret")) || len(record.ArgvSHA256) != 64 {
		t.Fatalf("unsafe record %s", payload)
	}
	request.Argv[1] += "x"
	if next := newExecAudit(1234, 3456, request); next.ArgvSHA256 == record.ArgvSHA256 {
		t.Fatal("different argv had the same audit digest")
	}
}

func TestReadExecOutputDistinguishesGuestResultFromTransport(t *testing.T) {
	const id = "91fc886ddd08a292a4aa74e2d2517095"
	var protocol, stdout, stderr bytes.Buffer
	writer := guestexec.FrameWriter{Writer: &protocol}
	if err := writer.WriteFrame(guestexec.AcceptedFrame, []byte(id)); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteFrame(guestexec.StdoutFrame, []byte{0, 0xff}); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteFrame(guestexec.StderrFrame, []byte("private-stdout")); err != nil {
		t.Fatal(err)
	}
	code := 255
	if err := writer.WriteJSON(guestexec.ResultFrame, guestexec.Result{Version: 1, RequestID: id, Disposition: "completed", GuestStatus: &code}); err != nil {
		t.Fatal(err)
	}
	result, err := readExecOutput(&protocol, io.NopCloser(strings.NewReader("")), &stdout, &stderr)
	if err != nil || result.ExitCode() != 255 || !bytes.Equal(stdout.Bytes(), []byte{0, 0xff}) || stderr.String() != "private-stdout" {
		t.Fatalf("output or result changed: %v %+v", err, result)
	}
	if err := writer.WriteFrame(guestexec.AcceptedFrame, []byte(id)); err != nil {
		t.Fatal(err)
	}
	result, err = readExecOutput(&protocol, io.NopCloser(strings.NewReader("")), io.Discard, io.Discard)
	if err != io.EOF || result.RequestID != id || result.GuestStatus != nil {
		t.Fatalf("expected unconfirmed acceptance: %v %+v", err, result)
	}
}

func TestExecPeerHangupDetectedWithUnreadStdin(t *testing.T) {
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if _, err := writer.Write([]byte("queued input")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchExecPeer(ctx, input.Fd(), cancel)
	select {
	case <-ctx.Done():
		t.Fatal("open stdin was treated as disconnected")
	case <-time.After(20 * time.Millisecond):
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("lost peer was hidden by pending input")
	}
}

func TestExecSlotLimitAndRelease(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "slots")
	var open []*os.File
	defer func() {
		for _, slot := range open {
			_ = slot.Close()
		}
	}()
	for index := 0; index < maxConcurrentExecs; index++ {
		slot, err := acquireExecSlot(directory)
		if err != nil {
			t.Fatal(err)
		}
		open = append(open, slot)
	}
	if slot, err := acquireExecSlot(directory); err == nil {
		_ = slot.Close()
		t.Fatal("accepted session beyond limit")
	}
	_ = open[0].Close()
	slot, err := acquireExecSlot(directory)
	if err != nil {
		t.Fatal(err)
	}
	_ = slot.Close()
}

func TestForegroundExecThroughHelperPipes(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=^TestForegroundExecPipePeer$")
	command.Env = append(os.Environ(), "CHASM_TEST_EXEC_PEER=1")
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	request := guestexec.Request{Name: "research", Cwd: "/", Argv: []string{"literal;name", ""}, Environment: map[string]string{"KEY": "private"}}
	result := execThroughHelper(ctx, command, request, bytes.NewReader([]byte{0, 0xfe}), &stdout, &stderr)
	if result.GuestStatus == nil || *result.GuestStatus != 125 || !bytes.Equal(stdout.Bytes(), []byte{0, 0xfe}) || stderr.String() != "separate" {
		t.Fatalf("helper streams or guest exit changed: %+v stdout=%q stderr=%q", result, &stdout, &stderr)
	}
}

func TestForegroundExecPipePeer(t *testing.T) {
	if os.Getenv("CHASM_TEST_EXEC_PEER") != "1" {
		return
	}
	if err := serveTestExecPipePeer(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func serveTestExecPipePeer() error {
	kind, payload, err := guestexec.ReadFrame(os.Stdin)
	if err != nil || kind != guestexec.RequestFrame {
		return fmt.Errorf("missing request: %v", err)
	}
	request, err := guestexec.DecodeRequest(payload)
	if err != nil || !reflect.DeepEqual(request.Argv, []string{"literal;name", ""}) || request.Environment["KEY"] != "private" {
		return fmt.Errorf("request payload changed")
	}
	const id = "91fc886ddd08a292a4aa74e2d2517095"
	output := guestexec.FrameWriter{Writer: os.Stdout}
	if err := output.WriteFrame(guestexec.AcceptedFrame, []byte(id)); err != nil {
		return err
	}
	for {
		kind, payload, err := guestexec.ReadFrame(os.Stdin)
		if err != nil {
			return err
		}
		if kind == guestexec.InputEndFrame {
			break
		}
		if kind != guestexec.InputFrame {
			return fmt.Errorf("unexpected input frame %d", kind)
		}
		if err := output.WriteFrame(guestexec.StdoutFrame, payload); err != nil {
			return err
		}
	}
	if err := output.WriteFrame(guestexec.StderrFrame, []byte("separate")); err != nil {
		return err
	}
	status := 125
	return output.WriteJSON(guestexec.ResultFrame, guestexec.Result{Version: 1, RequestID: id, Disposition: "completed", GuestStatus: &status})
}
