// Command supervisor_server is the unprivileged chasm
// supervisor served over a systemd-activated unix socket. It forwards
// sandbox lifecycle operations to the executor over the executor socket
// and persists performance samples under its state root.
//
// Production: systemd socket activation on the supervisor socket. For
// staging/parity: --socket PATH binds a fresh socket directly.
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"

	"github.com/rahulrajaram/chasm/internal/siteconfig"
	"github.com/rahulrajaram/chasm/internal/superserver"
	"github.com/rahulrajaram/chasm/internal/supervisor"
)

func main() {
	socketPath := flag.String("socket", "", "direct unix socket path (staging); empty = systemd socket activation")
	stagingPeerUID := flag.Int("staging-peer-uid", -1, "direct-socket peer uid; must equal the staging server uid")
	stateRoot := flag.String("state-root", siteconfig.SupervisorStateRoot, "supervisor state root")
	executorSocket := flag.String("executor-socket", siteconfig.ExecutorSocket, "executor unix socket")
	flag.Parse()

	if err := siteconfig.InitDefault(); err != nil {
		fmt.Fprintf(os.Stderr, "supervisor site config failed: %v\n", err)
		os.Exit(1)
	}
	writerLease, err := supervisor.AcquireWriterLease(*stateRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "supervisor writer lease failed: %v\n", err)
		os.Exit(1)
	}
	defer writerLease.Close()

	var listener net.Listener
	err = nil
	if *socketPath != "" {
		if *stagingPeerUID >= 0 && *stagingPeerUID != os.Geteuid() {
			fmt.Fprintln(os.Stderr, "staging peer uid must equal the direct-socket server uid")
			os.Exit(1)
		}
		_ = os.Remove(*socketPath)
		listener, err = net.Listen("unix", *socketPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "supervisor startup failed: %v\n", err)
			os.Exit(1)
		}
		mode := os.FileMode(0o660)
		if *stagingPeerUID >= 0 {
			// A parity server runs as the real supervisor identity and admits
			// only that same identity over a private scratch socket.
			mode = 0o600
		} else {
			group, err := superserver.OperatorGID()
			if err != nil {
				fmt.Fprintf(os.Stderr, "supervisor group lookup failed: %v\n", err)
				os.Exit(1)
			}
			if err := os.Chown(*socketPath, -1, group); err != nil {
				fmt.Fprintf(os.Stderr, "supervisor socket chown failed: %v\n", err)
				os.Exit(1)
			}
		}
		if err := os.Chmod(*socketPath, mode); err != nil {
			fmt.Fprintf(os.Stderr, "supervisor socket chmod failed: %v\n", err)
			os.Exit(1)
		}
	} else {
		if os.Getenv("LISTEN_PID") != strconv.Itoa(os.Getpid()) {
			fmt.Fprintln(os.Stderr, "supervisor requires systemd socket activation")
			os.Exit(1)
		}
		if os.Getenv("LISTEN_FDS") != "1" {
			fmt.Fprintln(os.Stderr, "supervisor requires exactly one systemd socket")
			os.Exit(1)
		}
		file := os.NewFile(3, "chasm-supervisor.sock")
		if file == nil {
			fmt.Fprintln(os.Stderr, "systemd socket fd is not available")
			os.Exit(1)
		}
		defer file.Close()
		listener, err = net.FileListener(file)
		if err != nil {
			fmt.Fprintf(os.Stderr, "supervisor startup failed: %v\n", err)
			os.Exit(1)
		}
		defer listener.Close()
	}

	unixListener, ok := listener.(*net.UnixListener)
	if !ok {
		fmt.Fprintln(os.Stderr, "supervisor socket is not a unix stream socket")
		os.Exit(1)
	}

	allowed := map[int]bool{}
	if *stagingPeerUID >= 0 {
		allowed[*stagingPeerUID] = true
	} else {
		allowed, err = superserver.AllowedOperatorUIDs()
		if err != nil {
			fmt.Fprintf(os.Stderr, "supervisor startup failed: %v\n", err)
			os.Exit(1)
		}
	}
	service := superserver.New(*stateRoot, *executorSocket)
	if err := service.Serve(unixListener, allowed); err != nil {
		fmt.Fprintf(os.Stderr, "supervisor server failed: %v\n", err)
		os.Exit(1)
	}
}
