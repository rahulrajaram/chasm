package main

import (
	"fmt"
	"strings"

	"github.com/rahulrajaram/chasm/internal/protocol"
)

const overviewHelp = `Chasm manages persistent development sandboxes on this host.

Usage:
  chasm
  chasm COMMAND [OPTIONS]
  chasm help [COMMAND]

Commands:
  create      Create a named sandbox
  show        Show sandbox details or its observed filesystem layout
  describe    Show, set, or clear a sandbox description
  repos       List or locate Git repositories across sandboxes
  start       Start a sandbox
  pause       Pause a sandbox while preserving its running state
  resume      Resume a paused sandbox
  stop        Stop a sandbox
  resize      Resize a stopped sandbox's memory, CPU, or root disk
  delete      Delete a stopped sandbox and its workspace volume
  shell       Open an interactive shell inside a sandbox
  exec        Run a foreground command inside a running sandbox
  doctor      Diagnose the installed host and site

Options:
  -h, --help  Show help for Chasm or a command

Examples:
  chasm
  chasm create research
  chasm describe research --set "LLM evaluation and tooling"
  chasm start research
  chasm shell research
  chasm exec research -- id

Run 'chasm help COMMAND' for command details.
Run 'chasm help aliases' for compatibility commands.
`

// createHelp renders the budget floors, safety bounds, and defaults from
// the protocol constants so the help text can never drift from the parsers
// that enforce them. The real per-sandbox cap is host-derived (host memory
// minus host_reserve_mib for memory; the host CPU count for CPU), enforced
// by the executor.
var createHelp = fmt.Sprintf(`Create a named sandbox from the sealed template.

Usage:
  chasm create NAME [BUDGET_MEMORY_MIB] [--memory MIB] [--cpu N]

Arguments:
  NAME               Logical sandbox name: 1-32 lowercase letters, digits, or
                     hyphens
  BUDGET_MEMORY_MIB  Optional declared memory budget in MiB (floor %d,
                     default %d; --memory MIB is the same setting). The
                     budget reserves host headroom: starts are refused,
                     naming the resource, when the sum of running sandbox
                     budgets plus this budget exceeds the host's cap (host
                     memory minus host_reserve_mib). A budget above that
                     host cap is refused at create time.
  --cpu N            Optional declared CPU budget (floor %d, default %d);
                     it becomes the sandbox's limits.cpu count. A CPU budget
                     above the host CPU count is refused at create time.

Behavior:
  The new sandbox receives its own persistent /workspace volume and the global
  /shared volume. Explicit project workflows use /workspace/<projectname>;
  verification snapshots retain the run-scoped workspace namespace. Creation
  leaves the sandbox stopped.

Options:
  -h, --help  Show this help

Example:
  chasm create research
  chasm create research 2048
  chasm create research --memory 4096 --cpu 4
`,
	protocol.MinSandboxBudgetMemoryMiB,
	protocol.DefaultSandboxBudgetMemoryMiB,
	protocol.MinSandboxBudgetCPU,
	protocol.DefaultSandboxBudgetCPU)

const showHelp = `Show details for a sandbox or its observed filesystem layout.

Usage:
  chasm show [NAME] [--json]
  chasm show NAME --layout [--json]

Arguments:
  NAME        Logical sandbox name; defaults to the site's default_sandbox
              when the site declares one, and is required otherwise

Options:
  --layout    Show a filesystem observation bounded to three levels
  -h, --help  Show this help

Behavior:
  A running sandbox is observed live. A stopped sandbox returns its last saved
  observation, or reports that it has not yet been observed.

Examples:
  chasm show
  chasm show research
  chasm show research --layout
`

const describeHelp = `Show, set, or clear a sandbox's human-readable description.

Usage:
  chasm describe NAME
  chasm describe NAME --set TEXT
  chasm describe NAME --clear

Arguments:
  NAME        Logical sandbox name
  TEXT        One trimmed printable line, up to 120 characters. Literal text
              that starts with "-" is given after the end-of-options marker:
              chasm describe NAME --set -- --text

Options:
  --set TEXT  Replace the description
  --clear     Remove the description
  -h, --help  Show this help

Examples:
  chasm describe research
  chasm describe research --set "LLM evaluation and tooling"
  chasm describe research --clear
`

const reposHelp = `Discover Git repositories without entering a sandbox.

Usage:
  chasm repos
  chasm repos REPOSITORY

Arguments:
  REPOSITORY  Repository basename to locate across all sandboxes

Behavior:
  With no repository, Chasm lists every observed repository. Discovery never
  starts a stopped sandbox; results identify whether observations are live,
  last observed, or unavailable.
  Additional worktrees under /workspace/.worktrees remain visible in the
  sandbox layout and are not listed as independent projects.

Options:
  -h, --help  Show this help

Examples:
  chasm repos
  chasm repos demo
`

const startHelp = `Start a sandbox and wait for its guest agent.

Usage:
  chasm start [NAME]

Arguments:
  NAME        Logical sandbox name; defaults to the site's default_sandbox
              when the site declares one, and is required otherwise

Behavior:
  Named sandboxes may run together when their declared memory budgets fit
  the host's cap: host memory minus host_reserve_mib.

Options:
  -h, --help  Show this help

Examples:
  chasm start
  chasm start research
`

const stopHelp = `Stop a sandbox without deleting its persistent state.

Usage:
  chasm stop [NAME]

Arguments:
  NAME        Logical sandbox name; defaults to the site's default_sandbox
              when the site declares one, and is required otherwise

Behavior:
  Stop asks the guest to shut down and waits for the stopped state.

Options:
  -h, --help  Show this help

Examples:
  chasm stop research
  chasm stop
`

const pauseHelp = `Pause a sandbox while preserving its VM state.

Usage:
  chasm pause NAME

Arguments:
  NAME        Logical sandbox name; required (pause addresses no default
              sandbox)

Options:
  -h, --help  Show this help

Use 'chasm resume NAME' to continue the paused VM.
`

const resumeHelp = `Resume a paused sandbox.

Usage:
  chasm resume NAME

Arguments:
  NAME        Logical sandbox name; required (resume addresses no default
              sandbox)

Options:
  -h, --help  Show this help

Use 'chasm pause NAME' to freeze the VM first.
`

const deleteHelp = `Delete a stopped sandbox and its workspace volume.

Usage:
  chasm delete NAME

Arguments:
  NAME        Logical sandbox name; the managed sandbox- instance form is
              refused (it would double-prefix into a phantom name)

Behavior:
  Deletion is refused while the sandbox is running; stop it first. The
  teardown removes the
  instance, its per-sandbox /workspace volume, and its catalog snapshot,
  in that order, with each step proven before the delete succeeds. The
  global /shared volume is never touched. Deletion is idempotent: an
  already-absent sandbox with no residue reports already_deleted.

Options:
  -h, --help  Show this help

Example:
  chasm delete perf-cycle-1
`

const shellHelp = `Open an interactive login shell as the unprivileged agent user.

Usage:
  chasm shell [NAME]

Arguments:
  NAME        Logical sandbox name; defaults to the site's default_sandbox
              when the site declares one, and is required otherwise

Behavior:
  The shell opens in /home/agent as the site's guest uid and gid (see
  guest_uid in the site descriptor). Chasm does not grant general Incus or
  root access.

  Local desktop sessions can bridge application text copies (xclip on X11,
  wl-copy on Wayland, when installed) by wrapping the shell with the
  optional clipboard plugin: run
  chasm-clipboard -- chasm shell NAME
  on the desktop machine. Core chasm shell is a plain terminal session.

Options:
  -h, --help  Show this help

Examples:
  chasm shell
  chasm shell research
`

const resizeHelp = `Resize a stopped sandbox's memory, CPU, or root disk.

Usage:
  chasm resize NAME [--memory SIZE] [--cpu N] [--root-disk SIZE]

Arguments:
  NAME        Logical sandbox name; required (resize addresses no default
              sandbox)
  SIZE        Plain MiB integers (2048) or explicit suffixes (2GiB, 2048MiB),
              matching how create reads a memory budget

Options:
  --memory SIZE      New declared memory budget in MiB; it becomes the
                     sandbox's limits.memory ceiling
  --cpu N            New declared CPU count; it becomes the sandbox's
                     limits.cpu count
  --root-disk SIZE   New root disk size; grow only — a smaller size is
                     refused, re-applying the current size is a proven
                     no-op, and the size may not exceed the storage
                     pool's configured size
  -h, --help         Show this help

Behavior:
  The sandbox must be stopped: chasm refuses a live resize and points at
  'chasm stop NAME'. Memory and CPU targets are refused above the host's
  cap (host memory minus host_reserve_mib; the host CPU count). The resize
  is applied in a fixed order — memory, CPU, root disk — and every change
  is proven by reading the applied value back; a failure part-way reports
  exactly which changes were applied and which were not. Re-applying the
  same values is a no-op that still proves the current values.

  A memory resize also repairs drift: when the recorded budget and the
  enforced limit differ (for example after a change made outside chasm),
  'chasm resize NAME --memory X' brings both to X.

Examples:
  chasm stop research
  chasm resize research --memory 8GiB --cpu 4
  chasm resize research --memory 2048
  chasm resize research --root-disk 100GiB
`

const aliasesHelp = `Compatibility commands retained for migration.

Preferred command                  Compatibility form
  chasm                            chasm list
  chasm show [NAME]                chasm status [NAME]
  chasm show NAME                  chasm inspect NAME
  chasm show NAME --layout         chasm layout NAME
  chasm repos REPOSITORY           chasm locate REPOSITORY

Compatibility commands remain functional but are intentionally omitted from
the primary command list. New scripts and documentation should use the
preferred forms.
`

// aliasUsageHelp renders a compatibility command's help: its own usage
// stanza first (so argument errors quote that command's line, not the
// overview's), followed by the shared compatibility table.
func aliasUsageHelp(usage string) string {
	return "Usage:\n  " + usage + "\n\n" + aliasesHelp
}

type helpRequest struct {
	topic     string
	requested bool
	err       error
}

func requestedHelp(arguments []string) (string, bool, error) {
	request := parseHelpRequest(arguments)
	return request.topic, request.requested, request.err
}

func parseHelpRequest(arguments []string) helpRequest {
	if len(arguments) == 0 {
		return helpRequest{}
	}
	if arguments[0] == "help" {
		switch len(arguments) {
		case 1:
			return helpRequest{requested: true}
		case 2:
			return helpRequest{topic: arguments[1], requested: true}
		default:
			return helpRequest{requested: true, err: fmt.Errorf("help accepts at most one command name")}
		}
	}
	if isHelpFlag(arguments[0]) {
		if len(arguments) == 1 {
			return helpRequest{requested: true}
		}
		return helpRequest{requested: true, err: fmt.Errorf("%s does not accept arguments", arguments[0])}
	}
	if arguments[0] == "--" {
		// A leading end-of-options marker makes every remaining argument
		// operand data (chasm -- --help stores the literal "--help"), so no
		// help request can exist; the command parser reports the usage
		// error instead of an unknown help topic.
		return helpRequest{}
	}
	// A help flag anywhere inside a command's arguments shows that
	// command's help (chasm create NAME --help shows create help). A
	// literal "--" ends the scan: everything after it is operand data
	// (chasm describe NAME --set -- --help stores the text "--help").
	for _, argument := range arguments[1:] {
		if argument == "--" {
			break
		}
		if isHelpFlag(argument) {
			return helpRequest{topic: arguments[0], requested: true}
		}
	}
	return helpRequest{}
}

const execHelp = `Run a command in a running sandbox as the unprivileged guest user.

Usage:
  chasm exec NAME [--cwd DIR] [--env KEY=VALUE ...] [--status-fd FD] -- COMMAND [ARG ...]

Options:
  --cwd DIR         Absolute guest path. Defaults to /home/agent.
  --env KEY=VALUE   Set a guest environment entry. Keys must be unique.
  --status-fd FD    Write one JSON result to an inherited writable fd >= 3.
  -h, --help        Show this help.

The mandatory -- separates Chasm options from the guest command. Stdin,
stdout, and stderr are separate byte streams. Chasm forwards up to 256
arguments, 64 environment entries, and 64 KiB of encoded request data.
Individual values are limited to 16 KiB. Eight execs may run per site.

Confirmed guest exits are returned as process exit statuses. Chasm uses
124 when completion is unknown, 125 for a rejected request, and 126 for
local setup failure. Use --status-fd to distinguish these from equal guest
codes. Incus can report 129 after a VM disconnect, so an Incus return of
129 has unknown completion. Ctrl-C requests TERM, followed by KILL if needed.
`

func isHelpFlag(argument string) bool {
	return argument == "--help" || argument == "-h"
}

func helpText(topic string) (string, bool) {
	switch topic {
	case "":
		return overviewHelp, true
	case "create":
		return createHelp, true
	case "show":
		return showHelp, true
	case "doctor":
		return "Diagnose the installed host and site without making changes.\n\nUsage:\n  chasm doctor [--json]\n", true
	case "describe":
		return describeHelp, true
	case "repos":
		return reposHelp, true
	case "start":
		return startHelp, true
	case "pause":
		return pauseHelp, true
	case "resume":
		return resumeHelp, true
	case "stop":
		return stopHelp, true
	case "resize":
		return resizeHelp, true
	case "delete":
		return deleteHelp, true
	case "shell":
		return shellHelp, true
	case "exec":
		return execHelp, true
	case "aliases":
		return aliasesHelp, true
	case "list":
		return aliasUsageHelp("chasm list"), true
	case "status":
		return aliasUsageHelp("chasm status [NAME]"), true
	case "inspect":
		return aliasUsageHelp("chasm inspect NAME"), true
	case "layout":
		return aliasUsageHelp("chasm layout NAME"), true
	case "locate":
		return aliasUsageHelp("chasm locate REPOSITORY"), true
	default:
		return "", false
	}
}

// usageBlock extracts a command's "Usage:" stanza from its help text, so
// argument errors quote the same lines the help shows and the two cannot
// drift apart. Every command, including each compatibility alias, carries
// its own stanza; unknown topics fall back to the overview's stanza.
func usageBlock(topic string) (string, bool) {
	text, _ := helpText(topic)
	block, found := usageLines(text)
	if !found {
		block, found = usageLines(overviewHelp)
	}
	return block, found
}

func usageLines(text string) (string, bool) {
	lines := strings.Split(text, "\n")
	block := []string{}
	inUsage := false
	for _, line := range lines {
		if !inUsage {
			if line == "Usage:" {
				inUsage = true
				block = append(block, line)
			}
			continue
		}
		if line == "" {
			break
		}
		block = append(block, line)
	}
	if len(block) == 0 {
		return "", false
	}
	return strings.Join(block, "\n") + "\n\n", true
}
