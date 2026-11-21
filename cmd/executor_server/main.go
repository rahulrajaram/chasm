// Command executor_server is the privileged chasm executor served
// over a unix socket.
//
// Production: run under systemd socket activation (LISTEN_PID/LISTEN_FDS,
// fd 3). For staging and parity: --socket PATH binds a fresh unix socket
// directly, with --expected-uid overriding the supervisor uid the peer
// must present.
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"

	"github.com/rahulrajaram/chasm/internal/execserver"
	"github.com/rahulrajaram/chasm/internal/hostidentity"
	"github.com/rahulrajaram/chasm/internal/siteconfig"
)

func main() {
	socketPath := flag.String("socket", "", "direct unix socket path (staging/parity); empty = systemd socket activation")
	expectedUID := flag.Int("expected-uid", 0, "expected supervisor peer uid (0 = resolve from passwd)")
	socketGID := flag.Int("socket-gid", -1, "direct-socket group id (default: expected peer uid)")
	flag.Parse()

	if err := siteconfig.InitDefault(); err != nil {
		fmt.Fprintf(os.Stderr, "executor site config failed: %v\n", err)
		os.Exit(1)
	}

	if os.Geteuid() != 0 && *socketPath == "" {
		fmt.Fprintln(os.Stderr, "executor server must run as root unless --socket staging mode is used")
		os.Exit(1)
	}
	// The pinned site identity is verified for the production socket. The
	// --socket staging mode is deliberately exempt: it exists so an
	// unprivileged developer box can exercise the wire contract against a
	// socket it owns, and it performs no host-storage action that the caller
	// could not already perform itself. Every production deployment runs the
	// default socket, which requires the marker.
	if *socketPath == "" {
		if err := hostidentity.RequireSiteHost(); err != nil {
			fmt.Fprintf(os.Stderr, "executor host identity failed: %v\n", err)
			os.Exit(1)
		}
	}

	var listener net.Listener
	var err error
	if *socketPath != "" {
		_ = os.Remove(*socketPath)
		listener, err = net.Listen("unix", *socketPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "executor startup failed: %v\n", err)
			os.Exit(1)
		}
		// Staging mirrors the production socket contract (root-owned,
		// supervisor-group, 0660) when run as root on a site host; on a
		// developer box in parity mode the foreground user owns it.
		if os.Geteuid() == 0 {
			uid := *expectedUID
			if uid == 0 {
				uid = execserver.SupervisorUID()
			}
			gid := *socketGID
			if gid < 0 {
				gid = uid
			}
			if err := os.Chown(*socketPath, -1, gid); err != nil {
				fmt.Fprintf(os.Stderr, "executor socket chown failed: %v\n", err)
				os.Exit(1)
			}
		}
		if err := os.Chmod(*socketPath, 0o660); err != nil {
			fmt.Fprintf(os.Stderr, "executor socket chmod failed: %v\n", err)
			os.Exit(1)
		}
	} else {
		if os.Getenv("LISTEN_PID") != strconv.Itoa(os.Getpid()) {
			fmt.Fprintln(os.Stderr, "executor requires systemd socket activation")
			os.Exit(1)
		}
		if os.Getenv("LISTEN_FDS") != "1" {
			fmt.Fprintln(os.Stderr, "executor requires exactly one systemd socket")
			os.Exit(1)
		}
		file := os.NewFile(3, "chasm-executor.sock")
		if file == nil {
			fmt.Fprintln(os.Stderr, "systemd socket fd is not available")
			os.Exit(1)
		}
		defer file.Close()
		listener, err = net.FileListener(file)
		if err != nil {
			fmt.Fprintf(os.Stderr, "executor startup failed: %v\n", err)
			os.Exit(1)
		}
		defer listener.Close()
	}

	unixListener, ok := listener.(*net.UnixListener)
	if !ok {
		fmt.Fprintln(os.Stderr, "executor socket is not a unix stream socket")
		os.Exit(1)
	}

	uid := *expectedUID
	if uid == 0 {
		uid = execserver.SupervisorUID()
	}
	audit := execserver.JSONLAudit(execserver.AuditRootDir(), execserver.AuditFileName())
	dispatch := execserver.DurableDispatch(execserver.NewOutcomeStore(execserver.OutcomeRootDir()), execserver.Dispatch)
	if err := execserver.Serve(unixListener, uid, dispatch, audit); err != nil {
		fmt.Fprintf(os.Stderr, "executor server failed: %v\n", err)
		os.Exit(1)
	}
}
