package guestexec

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"syscall"
	"time"

	"github.com/rahulrajaram/chasm/internal/incuslocal"
)

const (
	operationTimeout = 5 * time.Second
	cleanupGrace     = 2 * time.Second
	cleanupTimeout   = 5 * time.Second
)

type Client struct {
	dial  socketDial
	http  *http.Client
	grace time.Duration
	drain time.Duration
}

func NewClient(socket string) *Client {
	return newClient(func(ctx context.Context) (net.Conn, error) {
		conn, err := (&net.Dialer{Timeout: operationTimeout}).DialContext(ctx, "unix", socket)
		if err != nil {
			return nil, err
		}
		if err := incuslocal.RootPeer(conn); err == nil {
			return conn, nil
		}
		_ = conn.Close()
		return nil, errors.New("Incus socket peer is not authenticated as root")
	})
}

func newClient(dial socketDial) *Client {
	return &Client{
		dial:  dial,
		grace: cleanupGrace,
		drain: cleanupTimeout,
		http: &http.Client{
			Transport: &http.Transport{
				DialContext:            func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
				DisableKeepAlives:      true,
				MaxResponseHeaderBytes: 16 << 10,
			},
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

type envelope struct {
	Type       string          `json:"type"`
	StatusCode int             `json:"status_code"`
	Operation  string          `json:"operation"`
	Metadata   json.RawMessage `json:"metadata"`
}

type operation struct {
	ID         string `json:"id"`
	StatusCode int    `json:"status_code"`
	Error      string `json:"err"`
	Metadata   struct {
		FDs    map[string]string `json:"fds"`
		Output map[string]string `json:"output"`
		Return *int              `json:"return"`
	} `json:"metadata"`
}

type daemonError struct{ status int }

func (daemonError) Error() string { return "Incus refused the API request" }

var ErrInstanceNotFound = errors.New("managed Incus instance not found")

func (client *Client) query(ctx context.Context, method, path string, body any) (envelope, error) {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return envelope{}, err
		}
		reader = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://unix.socket"+path, reader)
	if err != nil {
		return envelope{}, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.http.Do(request)
	if err != nil {
		return envelope{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusAccepted {
		return envelope{}, daemonError{status: response.StatusCode}
	}
	const max = 2 << 20
	payload, err := io.ReadAll(io.LimitReader(response.Body, max+1))
	var reply envelope
	if err != nil || len(payload) > max || json.Unmarshal(payload, &reply) != nil {
		return envelope{}, errors.New("invalid Incus API response")
	}
	return reply, nil
}

func (client *Client) Instance(ctx context.Context, name string) (map[string]any, error) {
	return client.Observe(ctx, "/1.0/instances/"+name)
}

// Observe reads metadata from the authenticated local daemon.
func (client *Client) Observe(ctx context.Context, path string) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	reply, err := client.query(ctx, http.MethodGet, path, nil)
	if err != nil {
		var responseError daemonError
		if errors.As(err, &responseError) && responseError.status == http.StatusNotFound {
			return nil, ErrInstanceNotFound
		}
		return nil, err
	}
	var metadata map[string]any
	if reply.Type != "sync" || reply.StatusCode != 200 || json.Unmarshal(reply.Metadata, &metadata) != nil || metadata == nil {
		return nil, errors.New("invalid Incus metadata response")
	}
	return metadata, nil
}

type execPost struct {
	Command     []string          `json:"command"`
	WaitForWS   bool              `json:"wait-for-websocket"`
	Interactive bool              `json:"interactive"`
	Environment map[string]string `json:"environment"`
	User        int               `json:"user"`
	Group       int               `json:"group"`
	Cwd         string            `json:"cwd"`
	// RecordOutput lets a stream-less exec run: Incus 6.0 VMs return -1
	// without running the command when neither websockets nor recording is set.
	RecordOutput bool `json:"record-output,omitempty"`
}

var operationID = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
var websocketSecret = regexp.MustCompile(`^[a-f0-9]{64}$`)

func (client *Client) start(ctx context.Context, name string, request Request, uid int) (operation, error) {
	environment := map[string]string{
		"HOME": "/home/agent", "USER": "agent", "LOGNAME": "agent",
		"SHELL": "/usr/bin/zsh", "PATH": "/usr/local/bin:/usr/bin:/bin", "LANG": "C.UTF-8",
	}
	for key, value := range request.Environment {
		environment[key] = value
	}
	post := execPost{Command: request.Argv, WaitForWS: true, Environment: environment, User: uid, Group: uid, Cwd: request.Cwd}
	reply, err := client.query(ctx, http.MethodPost, "/1.0/instances/"+name+"/exec", post)
	if err != nil {
		return operation{}, err
	}
	var op operation
	if reply.Type != "async" || reply.StatusCode != 100 || json.Unmarshal(reply.Metadata, &op) != nil ||
		!operationID.MatchString(op.ID) || !validOperationPath(reply.Operation, op.ID) {
		return operation{}, errors.New("invalid Incus exec operation")
	}
	for _, key := range []string{"control", "0", "1", "2"} {
		if !websocketSecret.MatchString(op.Metadata.FDs[key]) {
			return operation{}, errors.New("missing Incus exec channel")
		}
	}
	return op, nil
}

func validOperationPath(value, id string) bool {
	path := "/1.0/operations/" + id
	return value == path || value == path+"?project=default"
}

func (client *Client) wait(ctx context.Context, id string) Result {
	reply, err := client.query(ctx, http.MethodGet, "/1.0/operations/"+id+"/wait", nil)
	if err == nil && reply.Type == "error" {
		// Incus 6.0.4 answers /wait for a failed operation (for example
		// "Command not found" for exit 127) with a bare error envelope; the
		// finished operation still carries its return value.
		reply, err = client.query(ctx, http.MethodGet, "/1.0/operations/"+id, nil)
	}
	var op operation
	if err != nil || reply.Type != "sync" || reply.StatusCode != 200 ||
		json.Unmarshal(reply.Metadata, &op) != nil || op.ID != id || op.Metadata.Return == nil {
		return Unknown("invalid_incus_completion")
	}
	exit := *op.Metadata.Return
	switch {
	case op.StatusCode == 400 && exit == 126 && op.Error == "Command not executable":
	case op.StatusCode == 400 && exit == 127 && op.Error == "Command not found":
	case op.StatusCode != 200 || op.Error != "" || exit < 0 || exit > 255:
		return Unknown("invalid_incus_completion")
	case exit == 129:
		result := Unknown("ambiguous_incus_return")
		result.IncusReturn = &exit
		return result
	}
	return Result{Version: 1, Disposition: "completed", GuestStatus: &exit}
}

type Output func(kind byte, payload []byte) error

// Execute only receives validated inputs and uses independently bounded setup
// and cancellation contexts. Guest stdin EOF is an Incus text barrier.
func (client *Client) Execute(ctx context.Context, name string, request Request, uid int, input <-chan []byte, output Output) Result {
	started := time.Now()
	setupCtx, stopSetup := context.WithTimeout(ctx, operationTimeout)
	defer stopSetup()
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return Rejected("setup_failed")
	}
	token := hex.EncodeToString(nonce[:])
	request.Argv = client.wrappedArgv(token, request.Argv)
	op, err := client.start(setupCtx, name, request, uid)
	if err != nil {
		var rejected daemonError
		if errors.As(err, &rejected) && rejected.status >= 400 && rejected.status < 500 {
			return Rejected("incus_rejected")
		}
		return Unknown("incus_dispatch_unconfirmed")
	}
	operationPath := "/1.0/operations/" + op.ID
	sockets := make([]*websocket, 0, 4)
	defer func() {
		for _, socket := range sockets {
			_ = socket.conn.Close()
		}
	}()
	for _, key := range []string{"control", "0", "1", "2"} {
		socket, err := openWebsocket(setupCtx, client.dial, operationPath, op.Metadata.FDs[key])
		if err != nil {
			return Unknown("incus_stream_setup")
		}
		sockets = append(sockets, socket)
	}
	ready := time.Now()
	stopSetup()
	result := client.stream(ctx, op.ID, sockets, input, output, started, ready)
	if result.Code == "ambiguous_incus_return" && client.confirmMarker(name, token, uid) {
		status := 129
		result.GuestStatus = &status
		result.Disposition = "completed"
		result.Code = ""
		result.Confirmation = "status_marker"
	}
	return result
}

func (client *Client) stream(ctx context.Context, id string, sockets []*websocket, input <-chan []byte, output Output, started, ready time.Time) Result {
	control, stdin, stdout, stderr := sockets[0], sockets[1], sockets[2], sockets[3]
	stdin.writeTimeout = 0
	streamCtx, stop := context.WithCancel(context.Background())
	defer stop()
	controlDone := make(chan struct{})
	go func() { _ = control.receive(nil); close(controlDone) }()
	go func() { _ = stdin.receive(nil) }()
	inDone := make(chan error, 1)
	go func() {
		for {
			select {
			case <-streamCtx.Done():
				return
			case payload, more := <-input:
				if !more {
					inDone <- stdin.write(wsText, nil)
					return
				}
				if err := stdin.write(wsBinary, payload); err != nil {
					inDone <- err
					return
				}
			}
		}
	}()
	type outputResult struct {
		err      error
		writeErr error
		first    int64
		kind     byte
	}
	outDone := make(chan outputResult, 2)
	publicOutputLost := make(chan error, 2)
	receive := func(ws *websocket, kind byte) {
		first := int64(-1)
		writing := true
		var writeErr error
		err := ws.receive(func(payload []byte) error {
			if first < 0 && len(payload) != 0 {
				first = time.Since(ready).Milliseconds()
			}
			if writing {
				writeErr = output(kind, payload)
			}
			if writing && writeErr != nil {
				writing = false
				publicOutputLost <- writeErr
			}
			return nil
		})
		if !writing && err == nil {
			err = writeErr
		}
		outDone <- outputResult{err: err, writeErr: writeErr, first: first, kind: kind}
	}
	go receive(stdout, StdoutFrame)
	go receive(stderr, StderrFrame)
	completed := make(chan Result, 1)
	go func() { completed <- client.wait(streamCtx, id) }()

	timings := map[string]int64{"spawn": ready.Sub(started).Milliseconds()}
	var result Result
	var cancelReason string
	var cancelAt time.Time
	var signalSent int
	var signalAttempted int
	outputLost := false
	var timer *time.Timer
	var timerChannel <-chan time.Time
	var completionTimer *time.Timer
	var completionDeadline <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
		if completionTimer != nil {
			completionTimer.Stop()
		}
	}()
	awaitCompletion := func() {
		if completionTimer == nil {
			completionTimer = time.NewTimer(client.drain)
			completionDeadline = completionTimer.C
		}
	}
	sendSignal := func(value int) {
		payload, _ := json.Marshal(struct {
			Command string `json:"command"`
			Signal  int    `json:"signal"`
		}{Command: "signal", Signal: value})
		signalAttempted = value
		if err := control.write(wsText, payload); err != nil {
			// The daemon kills a command if its control socket disappears.
			_ = control.conn.Close()
		} else {
			signalSent = value
		}
	}
	beginCancel := func(reason string) {
		if cancelReason != "" || result.GuestStatus != nil {
			return
		}
		cancelReason, cancelAt = reason, time.Now()
		sendSignal(int(syscall.SIGTERM))
		timer = time.NewTimer(client.grace + 250*time.Millisecond)
		timerChannel = timer.C
	}
	clientDone := ctx.Done()
	outputs := 0
	cleanupFinished := false
	for result.Version == 0 || outputs != 2 || (cancelReason != "" && result.GuestStatus == nil && !cleanupFinished) {
		select {
		case <-clientDone:
			clientDone = nil
			beginCancel("client_disconnected")
		case <-controlDone:
			// Incus closes control before recording ordinary completion.
			controlDone = nil
			awaitCompletion()
		case err := <-publicOutputLost:
			outputLost = true
			if outputPeerClosed(err) {
				beginCancel("client_disconnected")
			}
		case <-completionDeadline:
			completionDeadline = nil
			if cancelReason == "" {
				if result.GuestStatus != nil {
					outputLost = true
				} else if result.Version == 0 {
					result = Unknown("incus_completion_lost")
				}
				outputs = 2
			}
		case stream := <-outDone:
			outputs++
			if stream.first >= 0 {
				key := "first_stdout"
				if stream.kind == StderrFrame {
					key = "first_stderr"
				}
				timings[key] = stream.first
			}
			if stream.err != nil {
				outputLost = true
				if outputPeerClosed(stream.err) || stream.writeErr == nil || !errors.Is(stream.err, stream.writeErr) {
					beginCancel("incus_output_lost")
				}
			}
		case err := <-inDone:
			inDone = nil
			if err != nil {
				var networkErr net.Error
				if errors.As(err, &networkErr) && networkErr.Timeout() {
					beginCancel("incus_input_lost")
				} else {
					// A command can complete while input is still arriving.
					awaitCompletion()
				}
			}
		case result = <-completed:
			completed = nil
			awaitCompletion()
			if result.Code != "ambiguous_incus_return" && result.GuestStatus == nil {
				beginCancel(result.Code)
			}
		case <-timerChannel:
			if result.GuestStatus == nil && signalAttempted == int(syscall.SIGTERM) {
				sendSignal(int(syscall.SIGKILL))
				timer.Reset(cleanupTimeout)
			} else {
				if result.GuestStatus != nil {
					outputLost = true
				} else if result.IncusReturn == nil {
					result = Unknown("cancellation_unconfirmed")
				}
				outputs = 2
				cleanupFinished = true
			}
		}
	}
	if cancelReason != "" {
		timings["termination"] = time.Since(cancelAt).Milliseconds()
		if result.GuestStatus != nil {
			result.Disposition = "interrupted"
			result.Code = cancelReason
		}
		result.SignalSent = signalSent
	}
	if outputLost {
		result.OutputCode = "output_incomplete"
	}
	timings["execution"] = time.Since(ready).Milliseconds()
	timings["total"] = time.Since(started).Milliseconds()
	result.Timings = timings
	return result
}

func outputPeerClosed(err error) bool {
	return errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, net.ErrClosed) || errors.Is(err, os.ErrClosed) || errors.Is(err, io.ErrClosedPipe)
}
