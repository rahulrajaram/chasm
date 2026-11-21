package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/rahulrajaram/chasm/internal/execserver"
	"github.com/rahulrajaram/chasm/internal/protocol"
	"github.com/rahulrajaram/chasm/internal/siteconfig"
)

type commandError struct {
	OK      bool   `json:"ok"`
	Command string `json:"command"`
	Code    string `json:"code"`
	Error   string `json:"error"`
}

func main() {
	os.Exit(runCLI(os.Args[1:], os.Stdout, os.Stderr))
}

// runCLI binds the site descriptor and runs. Help invocations are
// dispatched before the descriptor is read: a host whose descriptor is
// missing, unreadable, or invalid (for example carrying an unknown key)
// still gets working help instead of a parse error.
func runCLI(args []string, stdout io.Writer, stderr io.Writer) int {
	if len(args) > 0 && isHelpInvocation(args[0]) {
		return run(args, stdout, stderr)
	}
	if err := siteconfig.InitDefault(); err != nil {
		fmt.Fprintln(stderr, "chasmctl: "+err.Error())
		return 1
	}
	return run(args, stdout, stderr)
}

func isHelpInvocation(argument string) bool {
	return argument == "help" || argument == "--help" || argument == "-h"
}

func run(args []string, stdout io.Writer, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "--local-admit-launch" {
		return runLocalLaunchAdmission(args[1:], os.Geteuid(), execserver.AdmitLaunchLocal, stderr)
	}
	return runWithTransportFactory(args, stdout, stderr, newUnixSupervisorTransport)
}

// runLocalLaunchAdmission is a private launcher entry point. It bypasses the
// supervisor transport only to avoid a nested call into the serial executor.
// The shell launcher owns the lifecycle lock surrounding this check and boot.
func runLocalLaunchAdmission(args []string, euid int, admit func(string) error, stderr io.Writer) int {
	if euid != 0 || len(args) != 1 || !protocol.ValidSandboxInstance(args[0]) {
		fmt.Fprintln(stderr, "chasmctl: local launch admission requires root and one valid instance")
		return 2
	}
	if err := admit(args[0]); err != nil {
		fmt.Fprintln(stderr, "chasmctl: launch admission refused: "+err.Error())
		return 1
	}
	return 0
}

func runWithTransportFactory(args []string, stdout io.Writer, stderr io.Writer, transportFactory func(string) supervisorTransport) int {
	if len(args) == 0 {
		writeError(stdout, "", "usage", "usage: chasmctl [--supervisor-socket PATH] COMMAND ...")
		return 2
	}
	// Help needs no transport and must not be swallowed by the flag parser
	// (whose -h path emits a JSON usage error at exit 2).
	if isHelpInvocation(args[0]) {
		if len(args) == 1 {
			return writeCommandHelp(stdout, stderr, "")
		}
		if args[0] == "help" && len(args) == 2 {
			return writeCommandHelp(stdout, stderr, args[1])
		}
		writeError(stdout, "help", "usage", "usage: chasmctl help [COMMAND]")
		return 2
	}
	global := flag.NewFlagSet("chasmctl", flag.ContinueOnError)
	global.SetOutput(stderr)
	supervisorSocket := global.String("supervisor-socket", "", "unprivileged supervisor Unix socket")
	if err := global.Parse(args); err != nil {
		writeError(stdout, "", "usage", err.Error())
		return 2
	}
	remaining := global.Args()
	if len(remaining) == 0 {
		writeError(stdout, "", "usage", "a command is required")
		return 2
	}
	if *supervisorSocket == "" {
		if !usesDefaultSupervisorSocket(remaining[0]) {
			writeError(stdout, "", "usage", "command requires --supervisor-socket PATH")
			return 2
		}
		*supervisorSocket = siteconfig.SupervisorSocket
	}
	return runSupervisorCommand(transportFactory(*supervisorSocket), remaining[0], remaining[1:], stdout, stderr)
}

func usesDefaultSupervisorSocket(command string) bool {
	switch command {
	case "sandbox-status", "ensure-guest", "stop-guest", "collect-performance",
		"create-sandbox", "describe-sandbox", "set-sandbox-description", "list-sandboxes", "inspect-sandbox", "start-sandbox", "stop-sandbox", "pause-sandbox", "resume-sandbox", "delete-sandbox", "admit-launch", "resize-sandbox",
		"sandbox-layout", "list-repositories", "locate-repository":
		return true
	default:
		return false
	}
}

// commandSynopsis pairs a command name with the synopsis line the help
// surface prints.
type commandSynopsis struct {
	name     string
	synopsis string
}

var supervisorCommandSynopses = []commandSynopsis{
	{"create-sandbox", "create-sandbox INSTANCE [BUDGET_MEMORY_MIB]"},
	{"describe-sandbox", "describe-sandbox INSTANCE"},
	{"set-sandbox-description", "set-sandbox-description INSTANCE DESCRIPTION"},
	{"list-sandboxes", "list-sandboxes"},
	{"inspect-sandbox", "inspect-sandbox INSTANCE"},
	{"start-sandbox", "start-sandbox INSTANCE"},
	{"stop-sandbox", "stop-sandbox INSTANCE"},
	{"pause-sandbox", "pause-sandbox INSTANCE"},
	{"resume-sandbox", "resume-sandbox INSTANCE"},
	{"delete-sandbox", "delete-sandbox INSTANCE"},
	{"admit-launch", "admit-launch INSTANCE"},
	{"resize-sandbox", "resize-sandbox INSTANCE [--memory SIZE] [--cpu N] [--root-disk SIZE] (SIZE: MiB integer, or GiB/MiB suffix)"},
	{"sandbox-layout", "sandbox-layout INSTANCE DEPTH (1-5)"},
	{"list-repositories", "list-repositories"},
	{"locate-repository", "locate-repository REPOSITORY"},
	{"sandbox-status", "sandbox-status"},
	{"ensure-guest", "ensure-guest"},
	{"stop-guest", "stop-guest"},
	{"collect-performance", "collect-performance PHASE"},
}

// writeCommandHelp prints the command inventory, the transport model, the
// default-socket allowlist, and the human-output toggle. With a non-empty
// command it prints only the matching synopsis lines and reports unknown
// names at exit 2.
func writeCommandHelp(stdout, stderr io.Writer, command string) int {
	var help strings.Builder
	if command != "" {
		for _, item := range supervisorCommandSynopses {
			if item.name == command {
				fmt.Fprintln(&help, item.synopsis)
			}
		}
		if help.Len() == 0 {
			writeError(stdout, command, "usage", "unknown command: "+command)
			return 2
		}
	} else {
		fmt.Fprintf(&help, `chasmctl — sandbox control-plane operator CLI

usage: chasmctl [--supervisor-socket PATH] COMMAND ...

Transport model:
  Commands talk to the unprivileged supervisor over its Unix socket. The
  socket defaults to %s for every command
  below; pass --supervisor-socket PATH to override it.

Output: JSON by default; human-readable when SANDBOXING_OUTPUT=human (the
chasm CLI sets it automatically when delegating).

Supervisor commands:
`, siteconfig.SupervisorSocket)
		for _, item := range supervisorCommandSynopses {
			fmt.Fprintln(&help, "  "+item.synopsis)
		}
		fmt.Fprint(&help, "\nRun 'chasmctl help COMMAND' for a command's synopsis.\n")
	}
	if _, err := io.WriteString(stdout, help.String()); err != nil {
		fmt.Fprintf(stderr, "chasmctl: writing help: %v\n", err)
		return 1
	}
	return 0
}

func writeJSON(stdout io.Writer, value interface{}) error {
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func writeError(stdout io.Writer, command string, code string, message string) {
	_ = writeJSON(stdout, commandError{OK: false, Command: command, Code: code, Error: message})
}
