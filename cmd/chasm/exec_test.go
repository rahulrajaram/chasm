package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rahulrajaram/chasm/internal/guestexec"
	"github.com/rahulrajaram/chasm/internal/siteconfig"
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
	writer = guestexec.FrameWriter{Writer: &protocol}
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

type stalledExecWriter struct {
	started chan struct{}
	release chan struct{}
	bytes.Buffer
}

func (writer *stalledExecWriter) Write(payload []byte) (int, error) {
	close(writer.started)
	<-writer.release
	return writer.Buffer.Write(payload)
}

type observedCloser struct{ closed chan struct{} }

func (closer observedCloser) Close() error { close(closer.closed); return nil }

type execErrorWriter struct{ err error }

func (writer execErrorWriter) Write([]byte) (int, error) { return 0, writer.err }

func TestExecOutputErrorOnlyClosesInputOnPeerHangup(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		closed bool
	}{
		{"full file", syscall.ENOSPC, false},
		{"closed pipe", syscall.EPIPE, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			const id = "91fc886ddd08a292a4aa74e2d2517095"
			var protocol bytes.Buffer
			frames := guestexec.FrameWriter{Writer: &protocol}
			_ = frames.WriteFrame(guestexec.AcceptedFrame, []byte(id))
			_ = frames.WriteFrame(guestexec.StdoutFrame, []byte("payload"))
			status := 0
			_ = frames.WriteJSON(guestexec.ResultFrame, guestexec.Result{Version: 1, RequestID: id, Disposition: "completed", GuestStatus: &status})
			input := observedCloser{closed: make(chan struct{})}
			result, err := readExecOutput(&protocol, input, execErrorWriter{test.err}, io.Discard)
			if !errors.Is(err, test.err) || result.GuestStatus == nil || *result.GuestStatus != 0 {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			select {
			case <-input.closed:
				if !test.closed {
					t.Fatal("non-hangup error closed client input")
				}
			default:
				if test.closed {
					t.Fatal("peer hangup left client input open")
				}
			}
		})
	}
}

func TestSlowExecOutputDoesNotCloseClientInput(t *testing.T) {
	const id = "91fc886ddd08a292a4aa74e2d2517095"
	var protocol bytes.Buffer
	writer := guestexec.FrameWriter{Writer: &protocol}
	if err := writer.WriteFrame(guestexec.AcceptedFrame, []byte(id)); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteFrame(guestexec.StdoutFrame, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	code := 0
	if err := writer.WriteJSON(guestexec.ResultFrame, guestexec.Result{Version: 1, RequestID: id, Disposition: "completed", GuestStatus: &code}); err != nil {
		t.Fatal(err)
	}
	consumer := &stalledExecWriter{started: make(chan struct{}), release: make(chan struct{})}
	input := observedCloser{closed: make(chan struct{})}
	done := make(chan error, 1)
	go func() { _, err := readExecOutput(&protocol, input, consumer, io.Discard); done <- err }()
	<-consumer.started
	select {
	case <-input.closed:
		t.Fatal("slow consumer disconnected the guest")
	case err := <-done:
		t.Fatalf("read finished while output was stalled: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(consumer.release)
	if err := <-done; err != nil || consumer.String() != "hello" {
		t.Fatalf("delivery = %q, %v", consumer.String(), err)
	}
}

func TestClosedClientStdinSendsInputEnd(t *testing.T) {
	stdin, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	_ = stdin.Close()
	command := exec.Command(os.Args[0], "-test.run=^TestForegroundExecPipePeer$")
	command.Env = append(os.Environ(), "CHASM_TEST_EXEC_PEER=1")
	request := guestexec.Request{Name: "research", Cwd: "/", Argv: []string{"literal;name", ""}, Environment: map[string]string{"KEY": "private"}}
	result := execThroughHelper(context.Background(), command, request, stdin, io.Discard, io.Discard)
	if result.GuestStatus == nil || *result.GuestStatus != 125 || result.Code != "" {
		t.Fatalf("closed stdin cancelled guest: %+v", result)
	}
}

func TestPrivilegedExecRefusesInvalidBoundaryRequests(t *testing.T) {
	operator := siteconfig.Active().OperatorUser
	for _, test := range []struct {
		name       string
		args       []string
		uid        int
		env        map[string]string
		payload    []byte
		closeInput bool
	}{
		{"arguments", []string{"extra"}, 0, map[string]string{"SUDO_USER": operator, "SUDO_UID": "1000"}, nil, true},
		{"not root", nil, 1000, map[string]string{"SUDO_USER": operator, "SUDO_UID": "1000"}, nil, true},
		{"wrong user", nil, 0, map[string]string{"SUDO_USER": "other", "SUDO_UID": "1000"}, nil, true},
		{"missing user", nil, 0, map[string]string{"SUDO_UID": "1000"}, nil, true},
		{"bad uid", nil, 0, map[string]string{"SUDO_USER": operator, "SUDO_UID": "bad"}, nil, true},
		{"zero uid", nil, 0, map[string]string{"SUDO_USER": operator, "SUDO_UID": "0"}, nil, true},
		{"noncanonical request", nil, 0, map[string]string{"SUDO_USER": operator, "SUDO_UID": "1000"}, []byte(` {"name":"demo"}`), true},
		{"duplicate request field", nil, 0, map[string]string{"SUDO_USER": operator, "SUDO_UID": "1000"}, []byte(`{"name":"demo","name":"other"}`), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			input, inputWriter, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			output, outputWriter, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer output.Close()
			if test.payload != nil {
				if err := (&guestexec.FrameWriter{Writer: inputWriter}).WriteFrame(guestexec.RequestFrame, test.payload); err != nil {
					t.Fatal(err)
				}
			}
			if test.closeInput {
				_ = inputWriter.Close()
			} else {
				defer inputWriter.Close()
			}
			hostCalls := 0
			code := runPrivilegedExec(test.args, test.uid, testEnvironment(test.env), func() error { hostCalls++; return nil }, input, outputWriter)
			_ = outputWriter.Close()
			kind, payload, err := guestexec.ReadFrame(output)
			if code != 0 || err != nil || kind != guestexec.ResultFrame {
				t.Fatalf("reply code=%d kind=%d err=%v", code, kind, err)
			}
			var result guestexec.Result
			if err := json.Unmarshal(payload, &result); err != nil || result.Code != "helper_refused" || hostCalls > 1 {
				t.Fatalf("reply=%+v err=%v hostCalls=%d", result, err, hostCalls)
			}
		})
	}
}

func TestPrivilegedExecRequiresFIFOs(t *testing.T) {
	input, inputWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer inputWriter.Close()
	output, outputWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	defer outputWriter.Close()
	ordinary := captureFile(t)
	if code := runPrivilegedExec(nil, 0, testEnvironment(nil), func() error { return nil }, ordinary, outputWriter); code != 1 {
		t.Fatalf("regular stdin code = %d", code)
	}
	if code := runPrivilegedExec(nil, 0, testEnvironment(nil), func() error { return nil }, input, ordinary); code != 1 {
		t.Fatalf("regular stdout code = %d", code)
	}
}

func TestPrivilegedExecRequestTimeoutRefuses(t *testing.T) {
	originalUmask := syscall.Umask(0)
	syscall.Umask(originalUmask)
	defer syscall.Umask(originalUmask)
	input, inputWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer inputWriter.Close()
	output, outputWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	defer outputWriter.Close()
	started := time.Now()
	operator := siteconfig.Active().OperatorUser
	code := runPrivilegedExec(nil, 0, testEnvironment(map[string]string{"SUDO_USER": operator, "SUDO_UID": "1000"}), func() error { return nil }, input, outputWriter)
	if elapsed := time.Since(started); elapsed < 5*time.Second {
		t.Fatalf("request timed out too early: %v", elapsed)
	}
	kind, payload, err := guestexec.ReadFrame(output)
	var result guestexec.Result
	if code != 0 || err != nil || kind != guestexec.ResultFrame || json.Unmarshal(payload, &result) != nil || result.Code != "helper_refused" {
		t.Fatalf("timeout reply code=%d kind=%d result=%+v err=%v", code, kind, result, err)
	}
}

func TestPrivilegedExecAuditFailureAfterDispatchReportsUnknown(t *testing.T) {
	originalUmask := syscall.Umask(0)
	syscall.Umask(originalUmask)
	defer syscall.Umask(originalUmask)
	oldAudit, oldDispatch, oldSlots := appendExecAudit, executeGuestExec, execSlotDirectory
	defer func() { appendExecAudit, executeGuestExec, execSlotDirectory = oldAudit, oldDispatch, oldSlots }()
	execSlotDirectory = filepath.Join(t.TempDir(), "slots")
	audits := 0
	appendExecAudit = func(any) error {
		audits++
		if audits == 2 {
			return errors.New("audit disk full")
		}
		return nil
	}
	dispatched := false
	executeGuestExec = func(_ context.Context, _ string, _ guestexec.Request, _ int, _ <-chan []byte, _ *guestexec.FrameWriter) guestexec.Result {
		dispatched = true
		code := 0
		return guestexec.Result{Version: 1, Disposition: "completed", GuestStatus: &code}
	}
	input, inputWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer inputWriter.Close()
	output, outputWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	defer outputWriter.Close()
	request := guestexec.Request{Name: "demo", Cwd: "/", Environment: map[string]string{}, Argv: []string{"true"}}
	protocol := guestexec.FrameWriter{Writer: inputWriter}
	if err := protocol.WriteJSON(guestexec.RequestFrame, request); err != nil {
		t.Fatal(err)
	}
	if err := protocol.WriteFrame(guestexec.InputEndFrame, nil); err != nil {
		t.Fatal(err)
	}
	operator := siteconfig.Active().OperatorUser
	code := runPrivilegedExec(nil, 0, testEnvironment(map[string]string{"SUDO_USER": operator, "SUDO_UID": "1000"}), func() error { return nil }, input, outputWriter)
	if code != 0 {
		t.Fatalf("helper process code = %d", code)
	}
	if kind, _, err := guestexec.ReadFrame(output); err != nil || kind != guestexec.AcceptedFrame {
		t.Fatalf("acceptance frame %d, %v", kind, err)
	}
	kind, payload, err := guestexec.ReadFrame(output)
	var result guestexec.Result
	if err != nil || kind != guestexec.ResultFrame || json.Unmarshal(payload, &result) != nil || result.Disposition != "outcome_unknown" || result.Code != "audit_failed" || !dispatched || audits != 2 {
		t.Fatalf("post-dispatch audit result = %+v, frame=%d, process=%d, audits=%d, dispatched=%v, err=%v", result, kind, code, audits, dispatched, err)
	}
}

func TestPrivilegedExecSlotRefusalPrecedesAcceptedAudit(t *testing.T) {
	originalUmask := syscall.Umask(0)
	syscall.Umask(originalUmask)
	defer syscall.Umask(originalUmask)
	oldAudit, oldSlots := appendExecAudit, execSlotDirectory
	defer func() { appendExecAudit, execSlotDirectory = oldAudit, oldSlots }()
	execSlotDirectory = filepath.Join(t.TempDir(), "slots")
	var slots []*os.File
	defer func() {
		for _, slot := range slots {
			_ = slot.Close()
		}
	}()
	for range maxConcurrentExecs {
		slot, err := acquireExecSlot(execSlotDirectory)
		if err != nil {
			t.Fatal(err)
		}
		slots = append(slots, slot)
	}
	audits := 0
	appendExecAudit = func(any) error { audits++; return nil }
	input, inputWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer inputWriter.Close()
	output, outputWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	defer outputWriter.Close()
	request := guestexec.Request{Name: "demo", Cwd: "/", Environment: map[string]string{}, Argv: []string{"true"}}
	if err := (&guestexec.FrameWriter{Writer: inputWriter}).WriteJSON(guestexec.RequestFrame, request); err != nil {
		t.Fatal(err)
	}
	operator := siteconfig.Active().OperatorUser
	code := runPrivilegedExec(nil, 0, testEnvironment(map[string]string{"SUDO_USER": operator, "SUDO_UID": "1000"}), func() error { return nil }, input, outputWriter)
	kind, payload, err := guestexec.ReadFrame(output)
	var result guestexec.Result
	if code != 0 || err != nil || kind != guestexec.ResultFrame || json.Unmarshal(payload, &result) != nil || result.Code != "busy" || audits != 0 {
		t.Fatalf("busy reply=%+v code=%d audits=%d err=%v", result, code, audits, err)
	}
}

type refusedExecClient struct {
	metadata map[string]any
	t        *testing.T
}

func (c refusedExecClient) Instance(context.Context, string) (map[string]any, error) {
	return c.metadata, nil
}
func (c refusedExecClient) Execute(context.Context, string, guestexec.Request, int, <-chan []byte, guestexec.Output) guestexec.Result {
	c.t.Fatal("guest command dispatched before initialization")
	return guestexec.Result{}
}

func TestExecTargetChecks(t *testing.T) {
	site := siteconfig.Active()
	instance := managedInstanceName("demo")
	volume := site.WorkspaceVolumeName(instance)
	metadata := map[string]any{
		"name": instance, "type": "virtual-machine", "status": "Running",
		"config": map[string]any{
			site.MetadataKey("managed"): "true", site.MetadataKey("logical-name"): "demo", site.MetadataKey("initialized"): "true",
			site.MetadataKey("workspace-volume"): volume, site.MetadataKey("shared-volume"): site.SharedVolumeName(),
		},
		"devices": map[string]any{
			"shared":    map[string]any{"type": "disk", "pool": site.StoragePool, "source": site.SharedVolumeName(), "path": "/shared", "readonly": "false"},
			"workspace": map[string]any{"type": "disk", "pool": site.StoragePool, "source": volume, "path": "/workspace", "readonly": "false"},
		},
	}
	for _, test := range []struct {
		name string
		meta map[string]any
		err  error
		want string
	}{
		{"missing", nil, guestexec.ErrInstanceNotFound, "target_missing"},
		{"unavailable", nil, errors.New("dial failed"), "incus_unavailable"},
		{"unmanaged", map[string]any{"name": instance, "type": "virtual-machine", "status": "Running"}, nil, "target_unmanaged"},
		{"not running", func() map[string]any { clone := maps.Clone(metadata); clone["status"] = "Stopped"; return clone }(), nil, "target_not_running"},
		{"running", metadata, nil, ""},
		{"initializing", func() map[string]any {
			clone := maps.Clone(metadata)
			config := maps.Clone(metadata["config"].(map[string]any))
			delete(config, site.MetadataKey("initialized"))
			clone["config"] = config
			return clone
		}(), nil, "target_initializing"},
		{"ready initializing", func() map[string]any {
			clone := maps.Clone(metadata)
			clone["status"] = "Ready"
			config := maps.Clone(metadata["config"].(map[string]any))
			config[site.MetadataKey("initialized")] = "false"
			clone["config"] = config
			return clone
		}(), nil, "target_initializing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := execTargetError(instance, test.meta, test.err); got != test.want {
				t.Fatalf("target code = %q, want %q", got, test.want)
			}
			if test.want == "target_initializing" {
				result := dispatchResolvedExec(context.Background(), refusedExecClient{test.meta, t}, instance, guestexec.Request{Name: "demo"}, 1001, nil, nil, time.Now())
				if result.Disposition != "rejected" || result.Code != "target_initializing" || result.ExitCode() != 125 {
					t.Fatalf("unexpected refusal: %+v", result)
				}
			}

		})
	}
}

func TestSudoersExecAndDoctorGrantsAreExact(t *testing.T) {
	if selfPath != "/usr/local/bin/chasm" {
		t.Fatalf("sudo re-exec path = %q, want /usr/local/bin/chasm", selfPath)
	}
	payload, err := os.ReadFile(filepath.Join(moduleRoot(), "host-config", "sudoers.d", "chasm.template"))
	if err != nil {
		t.Fatal(err)
	}
	var entries []string
	for _, line := range strings.Split(string(payload), "\n") {
		if strings.HasPrefix(line, "@OPERATOR_USER@ ALL=") {
			entries = append(entries, line)
		}
	}
	if len(entries) != 3 {
		t.Fatalf("privileged sudo entries = %v", entries)
	}
	for _, test := range []struct{ verb, want string }{
		{"__exec", "@OPERATOR_USER@ ALL=(root) NOPASSWD: NOSETENV: /usr/local/bin/chasm __exec"},
		{"__doctor", "@OPERATOR_USER@ ALL=(root) NOPASSWD: NOSETENV: /usr/local/bin/chasm __doctor"},
	} {
		found := false
		for _, line := range entries {
			if strings.Contains(line, " "+test.verb) {
				found = true
				if line != test.want || strings.Contains(line, "*") {
					t.Fatalf("unsafe %s grant: %q", test.verb, line)
				}
			}
		}
		if !found {
			t.Fatalf("missing %s grant", test.verb)
		}
	}
	if entries[0] != "@OPERATOR_USER@ ALL=(root) NOPASSWD: /usr/local/bin/chasm __shell *" {
		t.Fatalf("interactive shell grant changed: %q", entries[0])
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
