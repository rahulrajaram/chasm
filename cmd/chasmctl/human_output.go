package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/rahulrajaram/chasm/internal/protocol"
)

const (
	minimumWideTableWidth = 78
	defaultTerminalWidth  = 100
)

type humanSandbox struct {
	Name        string
	State       string
	Mode        string
	Description string
}

type humanOutputStyle struct {
	Color bool
	Width int
}

func humanOutputEnabled() bool { return os.Getenv("SANDBOXING_OUTPUT") == "human" }

func currentHumanOutputStyle(stdout io.Writer) humanOutputStyle {
	width := defaultTerminalWidth
	if parsed, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && parsed >= 40 && parsed <= 500 {
		width = parsed
	}
	file, isFile := stdout.(*os.File)
	isTerminal := false
	if isFile {
		if info, err := file.Stat(); err == nil {
			isTerminal = info.Mode()&os.ModeCharDevice != 0
		}
	}
	return humanOutputStyle{Color: isTerminal && os.Getenv("NO_COLOR") == "", Width: width}
}

func renderHumanResult(command string, result map[string]interface{}, stdout io.Writer, style humanOutputStyle) (bool, error) {
	switch command {
	case "list-sandboxes":
		sandboxes, err := parseHumanSandboxes(result)
		if err != nil {
			return true, err
		}
		renderSandboxList(stdout, sandboxes, style)
		return true, nil
	case "describe-sandbox":
		name, description, _, err := parseHumanDescription(result, false)
		if err != nil {
			return true, err
		}
		if description == "" {
			fmt.Fprintf(stdout, "%s — No description\n", escapeHumanText(name))
		} else {
			fmt.Fprintf(stdout, "%s — %s\n", escapeHumanText(name), description)
		}
		return true, nil
	case "set-sandbox-description":
		name, description, changed, err := parseHumanDescription(result, true)
		if err != nil {
			return true, err
		}
		if !changed {
			fmt.Fprintf(stdout, "%s description is unchanged.\n", escapeHumanText(name))
		} else if description == "" {
			fmt.Fprintf(stdout, "Cleared %s description.\n", escapeHumanText(name))
		} else {
			fmt.Fprintf(stdout, "Updated %s description.\n", escapeHumanText(name))
		}
		return true, nil
	case "list-repositories":
		inventory, err := parseHumanRepositoryInventory(result, "repositories")
		if err != nil {
			return true, err
		}
		renderRepositoryInventory(stdout, inventory, "", style)
		return true, nil
	case "locate-repository":
		repository, ok := result["repository"].(string)
		if !ok || repository == "" {
			return true, fmt.Errorf("repository location response is malformed")
		}
		inventory, err := parseHumanRepositoryInventory(result, "matches")
		if err != nil {
			return true, err
		}
		renderRepositoryInventory(stdout, inventory, repository, style)
		return true, nil
	case "sandbox-status":
		status, err := parseHumanSandboxStatus(result)
		if err != nil {
			return true, err
		}
		renderSandboxStatus(stdout, status, style)
		if resources, present := parseHumanSandboxResources(result); present {
			renderSandboxResources(stdout, resources, status.Detail.Name, style)
		}
		return true, nil
	case "inspect-sandbox":
		detail, err := parseHumanSandboxDetail(result)
		if err != nil {
			return true, err
		}
		renderSandboxDetail(stdout, detail, style)
		if resources, present := parseHumanSandboxResources(result); present {
			renderSandboxResources(stdout, resources, detail.Name, style)
		}
		return true, nil
	case "resize-sandbox":
		renderResizeResult(stdout, result)
		return true, nil
	case "sandbox-layout":
		layout, err := parseHumanSandboxLayout(result)
		if err != nil {
			return true, err
		}
		renderSandboxLayout(stdout, layout)
		return true, nil
	default:
		return false, nil
	}
}

// renderResizeResult prints one line per changed resource with its
// before/after values and the unit the values are counted in, so a CPU
// count is never labeled MiB; an unobservable before value prints as
// "unknown" with the reason the response carried.
func renderResizeResult(stdout io.Writer, result map[string]interface{}) {
	name, _ := result["instance"].(string)
	changes, _ := result["resized"].([]interface{})
	if name == "" || len(changes) == 0 {
		fmt.Fprintf(stdout, "Resize completed with no reported changes.\n")
		return
	}
	fmt.Fprintf(stdout, "Resized %s:\n", escapeHumanText(name))
	labels := map[string]string{"memory": "memory", "cpu": "CPU", "root_disk": "root disk"}
	for _, change := range changes {
		entry, ok := change.(map[string]interface{})
		if !ok {
			continue
		}
		resource, _ := entry["resource"].(string)
		after, afterOK := humanNumber(entry["after"])
		if !afterOK {
			continue
		}
		beforeText := "unknown"
		before, beforeKnown := humanNumber(entry["before"])
		if beforeKnown {
			beforeText = strconv.FormatInt(before, 10)
		} else if reason, ok := entry["before_unknown"].(string); ok && reason != "" {
			beforeText = "unknown (" + escapeHumanText(reason) + ")"
		}
		if entry["unit"] == "count" {
			fmt.Fprintf(stdout, "  %s: %s -> %d\n", labels[resource], beforeText, after)
			continue
		}
		if beforeKnown {
			fmt.Fprintf(stdout, "  %s: %d MiB -> %d MiB\n", labels[resource], before, after)
			continue
		}
		fmt.Fprintf(stdout, "  %s: %s -> %d MiB\n", labels[resource], beforeText, after)
	}
}

// humanNumber reads one response number produced by the transport's
// UseNumber decoder.
func humanNumber(value interface{}) (int64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	parsed, err := number.Int64()
	return parsed, err == nil
}

func parseHumanSandboxes(result map[string]interface{}) ([]humanSandbox, error) {
	raw, ok := result["sandboxes"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("sandbox inventory is malformed")
	}
	sandboxes := make([]humanSandbox, 0, len(raw))
	for _, candidate := range raw {
		item, ok := candidate.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("sandbox inventory is malformed")
		}
		name, nameOK := item["logical_name"].(string)
		state, stateOK := item["instance_status"].(string)
		if !nameOK || name == "" || !stateOK || (state != "Running" && state != "Frozen" && state != "Stopped") {
			return nil, fmt.Errorf("sandbox inventory is malformed")
		}
		description := ""
		if candidateDescription, present := item["description"]; present {
			var descriptionOK bool
			description, descriptionOK = candidateDescription.(string)
			if !descriptionOK || !protocol.ValidSandboxDescription(description) {
				return nil, fmt.Errorf("sandbox inventory is malformed")
			}
		}
		sandbox := humanSandbox{Name: name, State: strings.ToLower(state), Mode: "managed", Description: description}
		sandboxes = append(sandboxes, sandbox)
	}
	return sandboxes, nil
}

func parseHumanDescription(result map[string]interface{}, requireChanged bool) (string, string, bool, error) {
	name, nameOK := result["logical_name"].(string)
	description, descriptionOK := result["description"].(string)
	if !nameOK || name == "" || !descriptionOK || !protocol.ValidSandboxDescription(description) {
		return "", "", false, fmt.Errorf("sandbox description response is malformed")
	}
	changed := false
	if requireChanged {
		var changedOK bool
		changed, changedOK = result["changed"].(bool)
		if !changedOK {
			return "", "", false, fmt.Errorf("sandbox description response is malformed")
		}
	}
	return name, description, changed, nil
}

func renderSandboxList(stdout io.Writer, sandboxes []humanSandbox, style humanOutputStyle) {
	if len(sandboxes) == 0 {
		fmt.Fprintln(stdout, "No sandboxes found.")
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "0 sandboxes · 0 running")
		return
	}
	width := style.Width
	if width < 40 {
		width = 40
	}
	wide := width >= minimumWideTableWidth
	nameWidth := len("SANDBOX")
	for _, sandbox := range sandboxes {
		if width := cellWidth(sandbox.Name); width > nameWidth {
			nameWidth = width
		}
	}
	if nameWidth > 32 {
		nameWidth = 32
	}
	if wide {
		descriptionWidth := width - nameWidth - 23
		fmt.Fprintf(stdout, "%-*s  %-10s  %-7s  %-*s\n", nameWidth, "SANDBOX", "STATE", "MODE", descriptionWidth, "DESCRIPTION")
		for _, sandbox := range sandboxes {
			description := sandbox.Description
			if description == "" {
				description = "—"
			}
			fmt.Fprintf(stdout, "%-*s  %s  %-7s  %-*s\n",
				nameWidth, truncateCell(sandbox.Name, nameWidth), styledState(sandbox.State, 10, style.Color),
				sandbox.Mode, descriptionWidth, truncateDescription(description, descriptionWidth))
		}
	} else {
		availableNameWidth := width - 23
		if availableNameWidth < 12 {
			availableNameWidth = 12
		}
		if nameWidth > availableNameWidth {
			nameWidth = availableNameWidth
		}
		fmt.Fprintf(stdout, "%-*s  %-10s  %-7s\n", nameWidth, "SANDBOX", "STATE", "MODE")
		for _, sandbox := range sandboxes {
			fmt.Fprintf(stdout, "%-*s  %s  %-7s\n",
				nameWidth, truncateCell(sandbox.Name, nameWidth), styledState(sandbox.State, 10, style.Color), sandbox.Mode)
		}
	}
	running := 0
	for _, sandbox := range sandboxes {
		if sandbox.State == "running" {
			running++
		}
	}
	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "%d %s · %d running\n", len(sandboxes), pluralize("sandbox", len(sandboxes)), running)
}

func styledState(state string, width int, color bool) string {
	label := "○ stopped"
	code := "\x1b[2m"
	if state == "running" {
		label = "● running"
		code = "\x1b[32m"
	}
	if state == "frozen" {
		// An Incus FROZEN instance (after `chasm pause`) is paused, not
		// stopped: resuming it restores the running VM in place.
		label = "◐ paused"
		code = "\x1b[33m"
	}
	padded := fmt.Sprintf("%-*s", width, label)
	if !color {
		return padded
	}
	return code + padded + "\x1b[0m"
}

// escapeHumanText makes untrusted text unambiguous and safe for a terminal.
func escapeHumanText(value string) string {
	var escaped strings.Builder
	for len(value) > 0 {
		character, size := utf8.DecodeRuneInString(value)
		if character == utf8.RuneError && size == 1 {
			fmt.Fprintf(&escaped, `\x%02x`, value[0])
			value = value[1:]
			continue
		}
		value = value[size:]
		switch character {
		case '\\':
			escaped.WriteString(`\\`)
		case '\n':
			escaped.WriteString(`\n`)
		case '\t':
			escaped.WriteString(`\t`)
		case '\r':
			escaped.WriteString(`\r`)
		default:
			if character <= 0x1f || character == 0x7f {
				fmt.Fprintf(&escaped, `\x%02x`, character)
			} else if (character >= 0x80 && character <= 0x9f) || character == 0x2028 || character == 0x2029 {
				fmt.Fprintf(&escaped, `\u%04x`, character)
			} else {
				escaped.WriteRune(character)
			}
		}
	}
	return escaped.String()
}

func truncateCell(value string, width int) string {
	safe := escapeHumanText(value)
	if len([]rune(safe)) <= width {
		return safe
	}
	if width <= 0 {
		return ""
	}
	if width == 1 {
		return "…"
	}
	var cell strings.Builder
	used := 0
	for len(value) > 0 {
		_, size := utf8.DecodeRuneInString(value)
		piece := escapeHumanText(value[:size])
		value = value[size:]
		length := len([]rune(piece))
		if used+length > width-1 {
			break
		}
		cell.WriteString(piece)
		used += length
	}
	return cell.String() + "…"
}

func truncateDescription(value string, width int) string {
	characters := []rune(value)
	if len(characters) <= width {
		return value
	}
	if width <= 1 {
		return string(characters[:width])
	}
	return string(characters[:width-1]) + "…"
}

func cellWidth(value string) int { return len([]rune(escapeHumanText(value))) }

func pluralize(noun string, count int) string {
	if count == 1 {
		return noun
	}
	if strings.HasSuffix(noun, "y") {
		return strings.TrimSuffix(noun, "y") + "ies"
	}
	for _, suffix := range []string{"s", "x", "z", "ch", "sh"} {
		if strings.HasSuffix(noun, suffix) {
			return noun + "es"
		}
	}
	return noun + "s"
}

func writeHumanError(stdout io.Writer, command string, err error) {
	var remote transportError
	message := err.Error()
	if errors.As(err, &remote) {
		message = remote.Message
	}
	_ = command
	fmt.Fprintf(stdout, "Error: %s\n", escapeHumanText(message))
}
