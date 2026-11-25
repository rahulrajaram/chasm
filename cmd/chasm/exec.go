package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/rahulrajaram/chasm/internal/execserver"
	"github.com/rahulrajaram/chasm/internal/guestexec"
	"github.com/rahulrajaram/chasm/internal/incuslocal"
	"github.com/rahulrajaram/chasm/internal/siteconfig"
)

func parseExec(arguments []string) (guestexec.Request, int, error) {
	request := guestexec.Request{Cwd: "/home/agent", Environment: map[string]string{}}
	statusFD := -1
	if len(arguments) == 0 {
		return request, statusFD, errors.New("exec requires a sandbox NAME and -- COMMAND")
	}
	var err error
	request.Name, err = safeLogicalName(arguments[0])
	if err != nil {
		return request, statusFD, err
	}
	cwdSeen := false
	for index := 1; index < len(arguments); index += 2 {
		option := arguments[index]
		if option == "--" {
			request.Argv = arguments[index+1:]
			return request, statusFD, request.Validate()
		}
		if index+1 == len(arguments) || arguments[index+1] == "--" {
			return request, statusFD, errors.New("exec option requires a value before -- COMMAND")
		}
		value := arguments[index+1]
		switch option {
		case "--cwd":
			if cwdSeen {
				return request, statusFD, errors.New("exec accepts at most one --cwd")
			}
			request.Cwd, cwdSeen = value, true
		case "--env":
			key, envValue, found := strings.Cut(value, "=")
			if !found || !guestexec.ValidEnvName(key) {
				return request, statusFD, errors.New("exec environment must use a valid KEY=VALUE")
			}
			if _, duplicate := request.Environment[key]; duplicate {
				return request, statusFD, errors.New("exec environment keys must be unique")
			}
			request.Environment[key] = envValue
		case "--status-fd":
			fd, err := strconv.ParseInt(value, 10, 31)
			if statusFD >= 0 || err != nil || fd < 3 || !isDecimalDigits(value) {
				return request, statusFD, errors.New("exec --status-fd requires one writable descriptor numbered 3 or higher")
			}
			statusFD = int(fd)
		default:
			return request, statusFD, errors.New("unknown exec option before -- COMMAND")
		}
	}
	return request, statusFD, errors.New("exec requires -- before COMMAND")
}

func runExec(arguments []string, getenv func(string) string, stdin, stdout, stderr *os.File) int {
	request, statusFD, err := parseExec(arguments)
	if err != nil {
		reportUsageError(stderr, append([]string{"exec"}, arguments...), err)
		return 2
	}
	var status *os.File
	if statusFD >= 0 {
		flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(statusFD), syscall.F_GETFL, 0)
		if errno != 0 || flags&syscall.O_ACCMODE == syscall.O_RDONLY {
			fmt.Fprintln(stderr, "chasm: exec status descriptor is not writable")
			return 2
		}
		syscall.CloseOnExec(statusFD)
		status = os.NewFile(uintptr(statusFD), "exec-status")
		defer status.Close()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGPIPE)
	defer stop()
	result := execThroughSudo(ctx, request, baseEnvironment(getenv), stdin, stdout, stderr)
	if status != nil {
		payload, err := json.Marshal(result)
		if err != nil || writeExecOutput(status, append(payload, '\n')) != nil {
			_ = writeExecOutput(stderr, []byte("chasm: exec status could not be written\n"))
			return 125
		}
	}
	if result.Code != "" && result.Code != "client_disconnected" {
		_ = writeExecOutput(stderr, []byte(fmt.Sprintf("chasm: exec %s\n", execResultMessage(result.Code, request.Name))))
	}
	if result.OutputCode != "" {
		_ = writeExecOutput(stderr, []byte("chasm: exec output delivery was incomplete\n"))
	}
	return result.ExitCode()
}

func execResultMessage(code, name string) string {
	if code == "target_initializing" {
		return fmt.Sprintf("sandbox %s is still initializing; wait for `chasm start` to finish and retry", name)
	}
	return execErrorMessage(code)
}

func execErrorMessage(code string) string {
	switch code {
	case "sudo_failed":
		return "could not start the privileged helper"
	case "helper_refused":
		return "privileged entry was refused"
	case "host_identity":
		return "host identity check failed"
	case "target_missing":
		return "managed sandbox is unavailable"
	case "target_unmanaged":
		return "target does not satisfy managed sandbox ownership checks"
	case "target_not_running":
		return "sandbox is not running"
	case "incus_input_lost":
		return "input delivery failed"
	case "incus_output_lost":
		return "output delivery failed"
	case "incus_unavailable":
		return "local Incus socket is unavailable"
	case "busy":
		return "site is at its foreground execution limit"
	case "audit_failed":
		return "audit persistence failed; guest execution may already have occurred"
	case "setup_failed":
		return "setup failed before invoking the guest"
	case "incus_rejected":
		return "request was rejected by Incus"
	default:
		return "could not confirm guest completion (" + code + ")"
	}
}

func execThroughSudo(ctx context.Context, request guestexec.Request, environment []string, stdin io.Reader, stdout, stderr io.Writer) guestexec.Result {
	// No caller data appears in argv or the host environment.
	command := exec.Command(sudoPath, "-n", "--", selfPath, "__exec")
	command.Env = environment
	command.Stderr = stderr
	// Parent signals travel through the protocol pipe, independently of
	// sudo's terminal or process-group signal forwarding policy.
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return execThroughHelper(ctx, command, request, stdin, stdout, stderr)
}

func execThroughHelper(ctx context.Context, command *exec.Cmd, request guestexec.Request, stdin io.Reader, stdout, stderr io.Writer) guestexec.Result {
	input, err := command.StdinPipe()
	if err != nil {
		return guestexec.Rejected("setup_failed")
	}
	defer input.Close()
	output, err := command.StdoutPipe()
	if err != nil {
		return guestexec.Rejected("setup_failed")
	}
	defer output.Close()
	if err := command.Start(); err != nil {
		return guestexec.Rejected("sudo_failed")
	}

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = input.Close()
		case <-done:
		}
	}()
	go func() {
		writer := guestexec.FrameWriter{Writer: input}
		if writer.WriteJSON(guestexec.RequestFrame, request) != nil {
			_ = input.Close()
			return
		}
		buffer := make([]byte, guestexec.MaxChunk)
		for {
			count, err := stdin.Read(buffer)
			if count != 0 && writer.WriteFrame(guestexec.InputFrame, buffer[:count]) != nil {
				_ = input.Close()
				return
			}
			if err != nil {
				if writer.WriteFrame(guestexec.InputEndFrame, nil) != nil {
					_ = input.Close()
				}
				// Keep the pipe open. EOF of guest stdin is not a disconnect.
				return
			}
		}
	}()

	type outcome struct {
		result guestexec.Result
		err    error
	}
	finished := make(chan outcome, 1)
	go func() {
		result, err := readExecOutput(output, input, stdout, stderr)
		finished <- outcome{result, err}
	}()
	var observed outcome
	select {
	case observed = <-finished:
	case <-ctx.Done():
		select {
		case observed = <-finished:
		case <-time.After(15 * time.Second):
			_ = output.Close()
			go func() { _ = command.Wait() }()
			return guestexec.Unknown("helper_completion_lost")
		}
	}
	_ = input.Close()
	waitErr := command.Wait()
	if observed.err != nil {
		if observed.result.Version != 0 {
			observed.result.OutputCode = "output_incomplete"
			return observed.result
		}
		if observed.result.RequestID == "" && waitErr != nil && observed.err == io.EOF {
			return guestexec.Rejected("sudo_failed")
		}
		result := guestexec.Unknown("helper_completion_lost")
		result.RequestID = observed.result.RequestID
		return result
	}
	return observed.result
}

func readExecOutput(output io.Reader, input io.Closer, stdout, stderr io.Writer) (guestexec.Result, error) {
	var writeErr error
	var requestID string
	for {
		kind, payload, err := guestexec.ReadFrame(output)
		if err != nil {
			return guestexec.Result{RequestID: requestID}, err
		}
		var stream io.Writer
		switch kind {
		case guestexec.AcceptedFrame:
			if requestID != "" || len(payload) != 32 {
				return guestexec.Result{RequestID: requestID}, errors.New("invalid exec acceptance frame")
			}
			if _, err := hex.DecodeString(string(payload)); err != nil {
				return guestexec.Result{}, errors.New("invalid exec request ID")
			}
			requestID = string(payload)
			continue
		case guestexec.StdoutFrame:
			stream = stdout
		case guestexec.StderrFrame:
			stream = stderr
		case guestexec.ResultFrame:
			var result guestexec.Result
			if json.Unmarshal(payload, &result) != nil || result.Version != 1 ||
				result.RequestID != requestID || !validExecResult(result) {
				return guestexec.Result{RequestID: requestID}, errors.New("invalid exec result")
			}
			return result, writeErr
		default:
			return guestexec.Result{RequestID: requestID}, errors.New("invalid exec output frame")
		}
		if requestID == "" {
			return guestexec.Result{}, errors.New("exec output preceded acceptance")
		}
		if writeErr == nil {
			writeErr = writeExecOutput(stream, payload)
			if execOutputHangup(writeErr) {
				_ = input.Close()
			}
		}
	}
}

func execOutputHangup(err error) bool {
	return errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, os.ErrClosed) || errors.Is(err, io.ErrClosedPipe)
}

// A slow consumer is not a disconnect. The caller owns output backpressure.
func writeExecOutput(stream io.Writer, payload []byte) error {
	n, err := stream.Write(payload)
	if err == nil && n != len(payload) {
		return io.ErrShortWrite
	}
	return err
}

func validExecResult(result guestexec.Result) bool {
	switch result.Disposition {
	case "completed", "interrupted":
		return result.RequestID != "" && result.GuestStatus != nil && *result.GuestStatus >= 0 && *result.GuestStatus <= 255
	case "rejected", "outcome_unknown":
		return result.Code != "" && result.GuestStatus == nil
	default:
		return false
	}
}

type execAudit struct {
	At          time.Time `json:"at"`
	OperatorUID int       `json:"operator_uid"`
	Sandbox     string    `json:"sandbox"`
	GuestUID    int       `json:"guest_uid"`
	GuestGID    int       `json:"guest_gid"`
	Cwd         string    `json:"cwd,omitempty"`
	EnvKeys     []string  `json:"environment_keys,omitempty"`
	Argv0       string    `json:"argv0,omitempty"`
	Argc        int       `json:"argc"`
	ArgvSHA256  string    `json:"argv_sha256,omitempty"`
	guestexec.Result
}

func newExecAudit(operator, guest int, request guestexec.Request) execAudit {
	audit := execAudit{At: time.Now().UTC(), OperatorUID: operator, Sandbox: request.Name, GuestUID: guest, GuestGID: guest, Cwd: request.Cwd, Argc: len(request.Argv)}
	for key := range request.Environment {
		audit.EnvKeys = append(audit.EnvKeys, key)
	}
	sort.Strings(audit.EnvKeys)
	if len(request.Argv) != 0 {
		audit.Argv0 = request.Argv[0]
		hash := sha256.New()
		for _, argument := range request.Argv {
			var length [4]byte
			binary.BigEndian.PutUint32(length[:], uint32(len(argument)))
			_, _ = hash.Write(length[:])
			_, _ = io.WriteString(hash, argument)
		}
		audit.ArgvSHA256 = hex.EncodeToString(hash.Sum(nil))
	}
	return audit
}

const maxConcurrentExecs = 8

var appendExecAudit = execserver.AppendGuestExecAudit
var executeGuestExec = dispatchExec
var execSlotDirectory = "/run/chasm-exec"

func acquireExecSlot(dir string) (*os.File, error) {
	if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return nil, errors.New("unsafe exec slot directory")
	}
	for slot := 0; slot < maxConcurrentExecs; slot++ {
		path := fmt.Sprintf("%s/slot-%d", dir, slot)
		file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
		if err != nil {
			return nil, err
		}
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
			_ = file.Close()
			return nil, errors.New("unsafe exec slot file")
		}
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return file, nil
		} else if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = file.Close()
			return nil, err
		}
		_ = file.Close()
	}
	return nil, errors.New("all exec slots are in use")
}

func runPrivilegedExec(arguments []string, effectiveUID int, getenv func(string) string, requireHost func() error, stdin, stdout *os.File) int {
	for _, descriptor := range []*os.File{stdin, stdout} {
		info, err := descriptor.Stat()
		if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
			return 1
		}
	}
	writer := &guestexec.FrameWriter{Writer: stdout}
	reply := func(result guestexec.Result) int {
		if writer.WriteJSON(guestexec.ResultFrame, result) != nil {
			return 1
		}
		return 0
	}
	operator, err := strconv.Atoi(getenv("SUDO_UID"))
	if len(arguments) != 0 || effectiveUID != 0 || getenv("SUDO_USER") != siteconfig.Active().OperatorUser || err != nil || operator <= 0 {
		return reply(guestexec.Rejected("helper_refused"))
	}
	// The helper's private files must not inherit the operator's umask.
	syscall.Umask(0077)
	started := time.Now()
	if err := requireHost(); err != nil {
		return reply(guestexec.Rejected("host_identity"))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGPIPE)
	defer stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go watchExecPeer(ctx, stdin.Fd(), cancel)
	initial := make(chan []byte, 1)
	go func() {
		kind, payload, err := guestexec.ReadFrame(stdin)
		if err != nil || kind != guestexec.RequestFrame {
			payload = nil
		}
		initial <- payload
	}()
	var payload []byte
	select {
	case payload = <-initial:
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
	}
	request, err := guestexec.DecodeRequest(payload)
	name, nameErr := safeLogicalName(request.Name)
	if err != nil || nameErr != nil {
		return reply(guestexec.Rejected("helper_refused"))
	}
	validation := time.Since(started).Milliseconds()
	guest := siteconfig.Active().GuestUID
	audit := newExecAudit(operator, guest, request)
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return reply(guestexec.Rejected("setup_failed"))
	}
	requestID := hex.EncodeToString(nonce[:])
	slot, err := acquireExecSlot(execSlotDirectory)
	if err != nil {
		return reply(guestexec.Rejected("busy"))
	}
	defer slot.Close()
	audit.Result = guestexec.Result{Version: 1, RequestID: requestID, Disposition: "accepted"}
	if appendExecAudit(audit) != nil {
		return reply(guestexec.Rejected("audit_failed"))
	}
	if writer.WriteFrame(guestexec.AcceptedFrame, []byte(requestID)) != nil {
		audit.At = time.Now().UTC()
		audit.Result = guestexec.Rejected("client_disconnected")
		audit.RequestID = requestID
		_ = appendExecAudit(audit)
		return 1
	}
	input := make(chan []byte, 2)
	go receiveExecInput(ctx, cancel, stdin, input)
	result := executeGuestExec(ctx, managedInstanceName(name), request, guest, input, writer)
	result.RequestID = requestID
	if result.Timings == nil {
		result.Timings = make(map[string]int64)
	}
	result.Timings["validation"] = validation
	result.Timings["total"] = time.Since(started).Milliseconds()
	audit.At = time.Now().UTC()
	audit.Result = result
	if appendExecAudit(audit) != nil {
		result = guestexec.Unknown("audit_failed")
		result.RequestID = requestID
	}
	return reply(result)
}

func dispatchExec(ctx context.Context, instance string, request guestexec.Request, guest int, input <-chan []byte, writer *guestexec.FrameWriter) guestexec.Result {
	if ctx.Err() != nil {
		return guestexec.Rejected("client_disconnected")
	}
	setupStarted := time.Now()
	endpoint, err := incuslocal.ConfiguredSocketPath()
	if err != nil {
		return guestexec.Rejected("incus_unavailable")
	}
	return dispatchResolvedExec(ctx, guestexec.NewClient(endpoint), instance, request, guest, input, writer, setupStarted)
}

type foregroundClient interface {
	Instance(context.Context, string) (map[string]any, error)
	Execute(context.Context, string, guestexec.Request, int, <-chan []byte, guestexec.Output) guestexec.Result
}

func dispatchResolvedExec(ctx context.Context, client foregroundClient, instance string, request guestexec.Request, guest int, input <-chan []byte, writer *guestexec.FrameWriter, setupStarted time.Time) guestexec.Result {
	metadata, err := client.Instance(ctx, instance)
	if code := execTargetError(instance, metadata, err); code != "" {
		if ctx.Err() != nil {
			return guestexec.Rejected("client_disconnected")
		}
		return guestexec.Rejected(code)
	}
	if ctx.Err() != nil {
		return guestexec.Rejected("client_disconnected")
	}
	setup := time.Since(setupStarted).Milliseconds()
	result := client.Execute(ctx, instance, request, guest, input, writer.WriteFrame)
	if result.Timings == nil {
		result.Timings = make(map[string]int64)
	}
	result.Timings["target_resolution"] = setup
	return result
}

func execTargetError(instance string, metadata map[string]any, err error) string {
	if errors.Is(err, guestexec.ErrInstanceNotFound) {
		return "target_missing"
	}
	if err != nil {
		return "incus_unavailable"
	}
	if metadata["type"] != "virtual-machine" || metadata["name"] != instance || execserver.ValidateManagedSandboxMetadata(instance, metadata) != nil {
		return "target_unmanaged"
	}
	if metadata["status"] != "Running" && metadata["status"] != "Ready" {
		return "target_not_running"
	}
	config, _ := metadata["config"].(map[string]any)
	if config[siteconfig.Active().MetadataKey("initialized")] != "true" {
		return "target_initializing"
	}
	return ""
}

func receiveExecInput(ctx context.Context, cancel context.CancelFunc, stdin io.Reader, input chan []byte) {
	ended := false
	for {
		kind, payload, err := guestexec.ReadFrame(stdin)
		if err != nil {
			cancel()
			return
		}
		if ended {
			cancel()
			return
		}
		switch kind {
		case guestexec.InputFrame:
			select {
			case input <- payload:
			case <-ctx.Done():
				return
			}
		case guestexec.InputEndFrame:
			close(input)
			ended = true
		default:
			cancel()
			return
		}
	}
}

// A blocked guest stdin can back up the protocol input reader. Poll only
// hangup/error so parent death remains observable with unread bytes pending.
func watchExecPeer(ctx context.Context, fd uintptr, cancel context.CancelFunc) {
	type pollDescriptor struct {
		fd       int32
		events   int16
		returned int16
	}
	for ctx.Err() == nil {
		poll := pollDescriptor{fd: int32(fd)}
		timeout := syscall.NsecToTimespec((250 * time.Millisecond).Nanoseconds())
		_, _, err := syscall.Syscall6(syscall.SYS_PPOLL, uintptr(unsafe.Pointer(&poll)), 1, uintptr(unsafe.Pointer(&timeout)), 0, 0, 0)
		if poll.returned != 0 || (err != 0 && err != syscall.EINTR) {
			cancel()
			return
		}
	}
}

// Kept as a seam so helper tests can prove rejection precedes syscall.Exec.
var shellTargetError = func(socket, instance string) string {
	metadata, err := guestexec.NewClient(socket).Instance(context.Background(), instance)
	return execTargetError(instance, metadata, err)
}
