package main

import (
	"bytes"
	"errors"
	"io"
	"strconv"
	"strings"

	"github.com/rahulrajaram/chasm/internal/protocol"
)

// parseSizeToMib converts one operator-supplied byte size to whole MiB: a
// plain integer counts MiB (the create form), a "MiB" suffix is explicit
// MiB, and a "GiB" suffix is converted. Anything else is refused so a typo
// can never become a resize target.
func parseSizeToMib(value string) (int64, bool) {
	if value == "" {
		return 0, false
	}
	digits, scale := value, int64(1)
	if strings.HasSuffix(value, "MiB") {
		digits = strings.TrimSuffix(value, "MiB")
	} else if strings.HasSuffix(value, "GiB") {
		digits = strings.TrimSuffix(value, "GiB")
		scale = 1024
	} else if !isPlainDigits(value) {
		return 0, false
	}
	if !isPlainDigits(digits) {
		return 0, false
	}
	parsed, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || parsed > (1<<62)/scale {
		return 0, false
	}
	return parsed * scale, true
}

func isPlainDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func runSupervisorCommand(transport supervisorTransport, command string, args []string, stdout io.Writer, stderr io.Writer) int {
	switch command {
	case "create-sandbox":
		if len(args) < 1 || len(args) > 4 {
			writeError(stdout, command, "usage", "create-sandbox requires INSTANCE [BUDGET_MEMORY_MIB] [--cpu N]")
			return 2
		}
		payload := map[string]interface{}{"instance": args[0]}
		rest := args[1:]
		// --cpu N is the single flag; it may appear before or after the
		// optional positional BUDGET_MEMORY_MIB.
		cpu := int64(0)
		for index := 0; index < len(rest); index++ {
			if rest[index] == "--cpu" {
				if index+1 >= len(rest) {
					writeError(stdout, command, "usage", "create-sandbox's --cpu requires a value")
					return 2
				}
				value, err := strconv.ParseInt(rest[index+1], 10, 64)
				if err != nil || !protocol.ValidCpuBudget(value) {
					writeError(stdout, command, "invalid_cpu_budget", protocol.CpuBudgetRangeMessage())
					return 2
				}
				cpu = value
				rest = append(rest[:index:index], rest[index+2:]...)
				index--
			}
		}
		if len(rest) == 1 {
			budget, err := strconv.ParseInt(rest[0], 10, 64)
			if err != nil || !protocol.ValidMemoryBudget(budget) {
				writeError(stdout, command, "invalid_budget", protocol.BudgetRangeMessage())
				return 2
			}
			payload["budget_memory_mib"] = budget
		}
		if cpu != 0 {
			payload["budget_cpu"] = cpu
		}
		if len(rest) > 1 {
			writeError(stdout, command, "usage", "create-sandbox requires INSTANCE [BUDGET_MEMORY_MIB] [--cpu N]")
			return 2
		}
		return writeRemoteResult(transport, command, "create_sandbox", payload, stdout)
	case "resize-sandbox":
		if len(args) < 1 {
			writeError(stdout, command, "usage", "resize-sandbox requires INSTANCE [--memory SIZE] [--cpu N] [--root-disk SIZE]")
			return 2
		}
		if !protocol.ValidSandboxInstance(args[0]) {
			writeError(stdout, command, "invalid_instance", "resize-sandbox requires a managed sandbox instance name")
			return 2
		}
		payload := map[string]interface{}{"instance": args[0]}
		// Each flag may appear at most once; SIZE accepts plain MiB integers
		// (the create form) and explicit MiB/GiB suffixes. Validation happens
		// locally before any transport, and the executor re-validates.
		seen := map[string]bool{}
		for index := 1; index < len(args); index += 2 {
			flag := args[index]
			if flag != "--memory" && flag != "--cpu" && flag != "--root-disk" || index+1 >= len(args) {
				writeError(stdout, command, "usage", "resize-sandbox requires INSTANCE [--memory SIZE] [--cpu N] [--root-disk SIZE]")
				return 2
			}
			if seen[flag] {
				writeError(stdout, command, "usage", "resize-sandbox accepts each flag at most once")
				return 2
			}
			seen[flag] = true
			value := args[index+1]
			switch flag {
			case "--memory":
				mib, ok := parseSizeToMib(value)
				if !ok || !protocol.ValidMemoryBudget(mib) {
					writeError(stdout, command, "invalid_budget", protocol.BudgetRangeMessage())
					return 2
				}
				payload["budget_memory_mib"] = mib
			case "--cpu":
				cpu, err := strconv.ParseInt(value, 10, 64)
				if err != nil || !protocol.ValidCpuBudget(cpu) {
					writeError(stdout, command, "invalid_cpu_budget", protocol.CpuBudgetRangeMessage())
					return 2
				}
				payload["budget_cpu"] = cpu
			case "--root-disk":
				mib, ok := parseSizeToMib(value)
				if !ok || !protocol.ValidRootDiskMib(mib) {
					writeError(stdout, command, "invalid_root_disk", protocol.RootDiskRangeMessage())
					return 2
				}
				payload["root_disk_mib"] = mib
			}
		}
		if len(payload) == 1 {
			writeError(stdout, command, "usage", "resize-sandbox requires at least one of --memory SIZE, --cpu N, --root-disk SIZE")
			return 2
		}
		return writeRemoteResult(transport, command, "resize_sandbox", payload, stdout)
	case "describe-sandbox", "inspect-sandbox", "start-sandbox", "stop-sandbox", "pause-sandbox", "resume-sandbox", "admit-launch":
		if len(args) != 1 {
			writeError(stdout, command, "usage", command+" requires INSTANCE")
			return 2
		}
		return writeRemoteResult(transport, command, strings.ReplaceAll(command, "-", "_"), map[string]interface{}{"instance": args[0]}, stdout)
	case "delete-sandbox":
		if len(args) != 1 {
			writeError(stdout, command, "usage", "delete-sandbox requires INSTANCE")
			return 2
		}
		// Validate before the transport is touched: a delete is destructive,
		// so an operand that is not a canonical managed instance name must be
		// refused locally rather than forwarded to the executor.
		if !protocol.ValidSandboxInstance(args[0]) {
			writeError(stdout, command, "invalid_instance", "delete-sandbox requires a managed sandbox instance name")
			return 2
		}
		return writeRemoteResult(transport, command, "delete_sandbox", map[string]interface{}{"instance": args[0]}, stdout)
	case "set-sandbox-description":
		if len(args) != 2 {
			writeError(stdout, command, "usage", "set-sandbox-description requires INSTANCE DESCRIPTION")
			return 2
		}
		if !protocol.ValidSandboxInstance(args[0]) || !protocol.ValidSandboxDescription(args[1]) {
			writeError(stdout, command, "invalid_description", "description must be one trimmed printable line of at most 120 characters")
			return 2
		}
		return writeRemoteResult(transport, command, "set_sandbox_description", map[string]interface{}{"instance": args[0], "description": args[1]}, stdout)
	case "list-sandboxes", "list-repositories":
		if len(args) != 0 {
			writeError(stdout, command, "usage", command+" accepts no arguments")
			return 2
		}
		return writeRemoteResult(transport, command, strings.ReplaceAll(command, "-", "_"), nil, stdout)
	case "sandbox-layout":
		if len(args) != 2 {
			writeError(stdout, command, "usage", "sandbox-layout requires INSTANCE DEPTH")
			return 2
		}
		depth, err := strconv.Atoi(args[1])
		if err != nil || depth < 1 || depth > 5 {
			writeError(stdout, command, "invalid_depth", "layout depth must be between 1 and 5")
			return 2
		}
		return writeRemoteResult(transport, command, "sandbox_layout", map[string]interface{}{"instance": args[0], "depth": depth}, stdout)
	case "locate-repository":
		if len(args) != 1 {
			writeError(stdout, command, "usage", "locate-repository requires REPOSITORY")
			return 2
		}
		return writeRemoteResult(transport, command, "locate_repository", map[string]interface{}{"repository": args[0]}, stdout)
	case "sandbox-status":
		if len(args) != 0 {
			writeError(stdout, command, "usage", "sandbox-status accepts no arguments")
			return 2
		}
		return writeRemoteResult(transport, command, "sandbox_status", nil, stdout)
	case "ensure-guest":
		if len(args) != 0 {
			writeError(stdout, command, "usage", "ensure-guest accepts no arguments")
			return 2
		}
		return writeRemoteResult(transport, command, "ensure_guest", nil, stdout)
	case "stop-guest":
		if len(args) != 0 {
			writeError(stdout, command, "usage", "stop-guest accepts no arguments")
			return 2
		}
		return writeRemoteResult(transport, command, "stop_guest", nil, stdout)
	case "collect-performance":
		if len(args) != 1 {
			writeError(stdout, command, "usage", "collect-performance requires PHASE")
			return 2
		}
		return writeRemoteResult(transport, command, "collect_performance", map[string]interface{}{"phase": args[0]}, stdout)
	default:
		writeError(stdout, command, "usage", "unsupported supervisor command")
		return 2
	}
}

func writeRemoteResult(transport supervisorTransport, command string, operation string, fields map[string]interface{}, stdout io.Writer) int {
	result, err := transport.call(operation, fields)
	if err != nil {
		if humanOutputEnabled() {
			writeHumanError(stdout, command, err)
			return 1
		}
		writeRemoteError(stdout, command, err)
		return 1
	}
	if humanOutputEnabled() {
		var rendered bytes.Buffer
		handled, renderErr := renderHumanResult(command, result, &rendered, currentHumanOutputStyle(stdout))
		if renderErr != nil {
			writeHumanError(stdout, command, renderErr)
			return 1
		}
		if handled {
			if _, err := rendered.WriteTo(stdout); err != nil {
				return 1
			}
			return 0
		}
	}
	if err := writeJSON(stdout, result); err != nil {
		return 1
	}
	return 0
}

func writeRemoteError(stdout io.Writer, command string, err error) {
	var remote transportError
	if errors.As(err, &remote) {
		writeError(stdout, command, remote.Code, remote.Message)
		return
	}
	writeError(stdout, command, "operation_failed", err.Error())
}
