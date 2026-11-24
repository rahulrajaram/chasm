package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	ProtocolVersion         = 1
	ExecutorMaxRequestBytes = 16 * 1024
	PublicMaxRequestBytes   = 1040 * 1024
	MaxSandboxDescription   = 120
	// Declared per-sandbox memory budget in MiB. The budget reserves host
	// headroom so resource stop conditions hold by construction; admission
	// refuses, naming the exhausted resource. The default matches the
	// sandbox template profile cap (limits.memory 3GiB), so the
	// reservation equals what the VM can actually consume.
	//
	// There is no fixed product cap: the real per-sandbox ceiling is the
	// host's memory minus the site's host_reserve_mib, computed where
	// /proc/meminfo is read (idiom 4). MinSandboxBudgetMemoryMiB is a
	// floor and SafetyBoundSandboxBudgetMemoryMiB exists only to reject
	// absurd inputs before arithmetic; the parsers below validate against
	// those two, and the executor enforces the host cap.
	MinSandboxBudgetMemoryMiB         = 512
	SafetyBoundSandboxBudgetMemoryMiB = 1048576
	DefaultSandboxBudgetMemoryMiB     = 3072
	// Declared per-sandbox CPU budget: the instance's limits.cpu count.
	// The default matches the sandbox template profile (limits.cpu 2);
	// the real per-sandbox cap is the host CPU count, enforced by the
	// executor at create time.
	MinSandboxBudgetCPU     = 1
	MaxSandboxBudgetCPU     = 1024
	DefaultSandboxBudgetCPU = 2
	// Declared per-sandbox root disk size in MiB, applied to the instance's
	// root device. The floor keeps the sealed template's guest bootable;
	// the safety bound only rejects absurd inputs before arithmetic. The
	// real caps are relational and enforced by the executor: a resize may
	// only grow, and may never exceed the storage pool's configured size
	// (idiom 4: no fixed product ceiling).
	MinSandboxRootDiskMiB = 1024
	MaxSandboxRootDiskMiB = 4194304
)

var (
	identifierPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	managedInstancePattern = regexp.MustCompile(`^sandbox-[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$`)
	phasePattern           = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)
)

// BudgetRangeMessage is the single prose rendering of the admitted memory
// budget range: every invalid_budget error formats it from the constants so
// the range is stated once and can never drift between surfaces. The upper
// bound is the arithmetic-safety bound, not a product cap: the executor
// enforces the host-derived cap (host memory minus host_reserve_mib).
func BudgetRangeMessage() string {
	return fmt.Sprintf("budget_memory_mib must be an integer MiB value between %d and %d", MinSandboxBudgetMemoryMiB, SafetyBoundSandboxBudgetMemoryMiB)
}

// RootDiskRangeMessage is the single prose rendering of the admitted root
// disk range, shared by the parser and the executor refusals.
func RootDiskRangeMessage() string {
	return fmt.Sprintf("root_disk_mib must be an integer MiB value between %d and %d", MinSandboxRootDiskMiB, MaxSandboxRootDiskMiB)
}

// ValidRootDiskMib reports whether value is a floor-and-safety-bound valid
// root disk size in MiB. Grow-only and pool-fit are relational rules the
// executor enforces against the live instance and pool.
func ValidRootDiskMib(value int64) bool {
	return value >= MinSandboxRootDiskMiB && value <= MaxSandboxRootDiskMiB
}

// CpuBudgetRangeMessage is the single prose rendering of the admitted CPU
// budget range; the executor enforces the host-derived cap (host CPU count).
func CpuBudgetRangeMessage() string {
	return fmt.Sprintf("budget_cpu must be an integer CPU count between %d and %d", MinSandboxBudgetCPU, MaxSandboxBudgetCPU)
}

// ValidMemoryBudget reports whether value is a floor-and-safety-bound valid
// memory budget in MiB. It is the only range rule the protocol parsers and
// the shell launchers apply; host caps are enforced where the host is read.
func ValidMemoryBudget(value int64) bool {
	return value >= MinSandboxBudgetMemoryMiB && value <= SafetyBoundSandboxBudgetMemoryMiB
}

// ValidCpuBudget reports whether value is a structurally valid CPU budget.
func ValidCpuBudget(value int64) bool {
	return value >= MinSandboxBudgetCPU && value <= MaxSandboxBudgetCPU
}

// ValidSandboxInstance reports whether name is a bounded, canonical managed
// sandbox name. The namespace restriction prevents lifecycle requests from
// reaching templates, builders, or unrelated Incus instances; the lowercase
// syntax is safe in both argv values and API path segments.
func ValidSandboxInstance(name string) bool {
	return managedInstancePattern.MatchString(name)
}

// ValidSandboxDescription accepts an empty value to represent no description,
// or one trimmed, printable line of at most MaxSandboxDescription runes.
func ValidSandboxDescription(description string) bool {
	if !utf8.ValidString(description) || utf8.RuneCountInString(description) > MaxSandboxDescription || strings.TrimSpace(description) != description {
		return false
	}
	for _, character := range description {
		if !unicode.IsPrint(character) || character == '\u2028' || character == '\u2029' {
			return false
		}
	}
	return true
}

type DecoderError struct {
	Code    string
	Message string
}

func (e DecoderError) Error() string {
	if e.Message != "" {
		return e.Code + ": " + e.Message
	}
	return e.Code
}

type Request struct {
	Operation string
	// Fields carries the validated request members (instance, depth,
	// repository, ...) so a server-side handler can dispatch without
	// re-parsing. Set for both ParseExecutor and ParsePublic.
	Fields map[string]interface{}
	// LifecycleTimings is private executor dispatch state, never parsed from wire bytes.
	LifecycleTimings map[string]int64
}

// RequestID returns the validated request_id identifier.
func (r Request) RequestID() string { return fieldString(r.Fields, "request_id") }

// Instance returns the instance member, or "" when the operation has none.
func (r Request) Instance() string { return fieldString(r.Fields, "instance") }

// StringField returns a string member, or "" when absent or not a string.
func (r Request) StringField(name string) string { return fieldString(r.Fields, name) }

// NumberField returns an integer member (json.Number, float64, int or
// int64). parseStrictObject decodes with UseNumber, so payload numbers
// arrive as json.Number.
func (r Request) NumberField(name string) (int64, bool) {
	value, ok := r.Fields[name]
	if !ok {
		return 0, false
	}
	return numberValue(value)
}

func fieldString(fields map[string]interface{}, name string) string {
	if fields == nil {
		return ""
	}
	value, ok := fields[name].(string)
	if !ok {
		return ""
	}
	return value
}

func numberValue(value interface{}) (int64, bool) {
	switch number := value.(type) {
	case json.Number:
		parsed, err := number.Int64()
		return parsed, err == nil
	case float64:
		return int64(number), true
	case int:
		return int64(number), true
	case int64:
		return number, true
	default:
		return 0, false
	}
}

func ErrorCode(err error) (string, bool) {
	var decoderError DecoderError
	if errors.As(err, &decoderError) {
		return decoderError.Code, true
	}
	return "", false
}

func ParseExecutorOperation(raw []byte) (string, error) {
	request, err := ParseExecutor(raw)
	if err != nil {
		return "", err
	}
	return request.Operation, nil
}

func ParsePublicOperation(raw []byte) (string, error) {
	request, err := ParsePublic(raw)
	if err != nil {
		return "", err
	}
	return request.Operation, nil
}

func ParseExecutor(raw []byte) (Request, error) {
	payload, err := parseStrictObject(raw, ExecutorMaxRequestBytes)
	if err != nil {
		return Request{}, err
	}
	if err := checkVersion(payload); err != nil {
		return Request{}, err
	}
	if _, err := requireIdentifier(payload, "request_id"); err != nil {
		return Request{}, err
	}
	operation, ok := stringField(payload, "operation")
	if !ok {
		return Request{}, failMessage("unknown_operation", "operation is not permitted")
	}
	switch operation {
	case "inspect_default_sandbox", "list_sandboxes", "list_repositories":
		if err := requireExactFields(payload, "version", "request_id", "operation"); err != nil {
			return Request{}, err
		}
	case "create_sandbox":
		required := []string{"version", "request_id", "operation", "instance"}
		// budget_memory_mib and budget_cpu are the optional create_sandbox
		// fields; each is admitted only when the caller supplied it.
		for _, field := range []string{"budget_memory_mib", "budget_cpu"} {
			if _, present := payload[field]; present {
				required = append(required, field)
			}
		}
		if err := requireExactFields(payload, required...); err != nil {
			return Request{}, err
		}
		instance, ok := stringField(payload, "instance")
		if !ok || !ValidSandboxInstance(instance) {
			return Request{}, failMessage("invalid_instance", "instance is not permitted")
		}
		if _, present := payload["budget_memory_mib"]; present {
			budget, ok := numberIntField(payload, "budget_memory_mib")
			if !ok || !ValidMemoryBudget(budget) {
				return Request{}, failMessage("invalid_budget", BudgetRangeMessage())
			}
		}
		if _, present := payload["budget_cpu"]; present {
			cpu, ok := numberIntField(payload, "budget_cpu")
			if !ok || !ValidCpuBudget(cpu) {
				return Request{}, failMessage("invalid_cpu_budget", CpuBudgetRangeMessage())
			}
		}
	case "resize_sandbox":
		// At least one resize target is required; each admitted field is
		// validated exactly like its create_sandbox counterpart.
		required := []string{"version", "request_id", "operation", "instance"}
		for _, field := range []string{"budget_memory_mib", "budget_cpu", "root_disk_mib"} {
			if _, present := payload[field]; present {
				required = append(required, field)
			}
		}
		if len(required) == 4 {
			return Request{}, failMessage("invalid_fields", "resize_sandbox requires at least one of budget_memory_mib, budget_cpu, root_disk_mib")
		}
		if err := requireExactFields(payload, required...); err != nil {
			return Request{}, err
		}
		instance, ok := stringField(payload, "instance")
		if !ok || !ValidSandboxInstance(instance) {
			return Request{}, failMessage("invalid_instance", "instance is not permitted")
		}
		if _, present := payload["budget_memory_mib"]; present {
			budget, ok := numberIntField(payload, "budget_memory_mib")
			if !ok || !ValidMemoryBudget(budget) {
				return Request{}, failMessage("invalid_budget", BudgetRangeMessage())
			}
		}
		if _, present := payload["budget_cpu"]; present {
			cpu, ok := numberIntField(payload, "budget_cpu")
			if !ok || !ValidCpuBudget(cpu) {
				return Request{}, failMessage("invalid_cpu_budget", CpuBudgetRangeMessage())
			}
		}
		if _, present := payload["root_disk_mib"]; present {
			disk, ok := numberIntField(payload, "root_disk_mib")
			if !ok || !ValidRootDiskMib(disk) {
				return Request{}, failMessage("invalid_root_disk", RootDiskRangeMessage())
			}
		}
	case "describe_sandbox", "inspect_sandbox", "ensure_guest_running", "stop_guest", "pause_guest", "resume_guest", "delete_sandbox", "admit_launch":
		if err := requireExactFields(payload, "version", "request_id", "operation", "instance"); err != nil {
			return Request{}, err
		}
		instance, ok := stringField(payload, "instance")
		if !ok || !ValidSandboxInstance(instance) {
			return Request{}, failMessage("invalid_instance", "instance is not permitted")
		}
	case "set_sandbox_description":
		if err := requireExactFields(payload, "version", "request_id", "operation", "instance", "description"); err != nil {
			return Request{}, err
		}
		instance, ok := stringField(payload, "instance")
		if !ok || !ValidSandboxInstance(instance) {
			return Request{}, failMessage("invalid_instance", "instance is not permitted")
		}
		description, ok := stringField(payload, "description")
		if !ok || !ValidSandboxDescription(description) {
			return Request{}, failMessage("invalid_description", "description must be one trimmed printable line of at most 120 characters")
		}
	case "sandbox_layout":
		if err := requireExactFields(payload, "version", "request_id", "operation", "instance", "depth"); err != nil {
			return Request{}, err
		}
		instance, ok := stringField(payload, "instance")
		if !ok || !ValidSandboxInstance(instance) {
			return Request{}, failMessage("invalid_instance", "instance is not permitted")
		}
		depth, ok := numberIntField(payload, "depth")
		if !ok || depth < 1 || depth > 5 {
			return Request{}, failMessage("invalid_depth", "layout depth is not permitted")
		}
	case "locate_repository":
		if err := requireExactFields(payload, "version", "request_id", "operation", "repository"); err != nil {
			return Request{}, err
		}
		if _, err := requireIdentifier(payload, "repository"); err != nil {
			return Request{}, failMessage("invalid_repository", "repository identifier is not permitted")
		}
	case "collect_performance":
		if err := requireExactFields(payload, "version", "request_id", "operation", "phase"); err != nil {
			return Request{}, err
		}
		phase, ok := stringField(payload, "phase")
		if !ok || !phasePattern.MatchString(phase) {
			return Request{}, fail("invalid_phase")
		}
	default:
		return Request{}, failMessage("unknown_operation", "operation is not permitted")
	}
	return Request{Operation: operation, Fields: payload}, nil
}

func ParsePublic(raw []byte) (Request, error) {
	payload, err := parseStrictObject(raw, PublicMaxRequestBytes)
	if err != nil {
		return Request{}, err
	}
	if err := checkVersion(payload); err != nil {
		return Request{}, err
	}
	if _, err := requireIdentifier(payload, "request_id"); err != nil {
		return Request{}, err
	}
	operation, ok := stringField(payload, "operation")
	if !ok {
		return Request{}, failMessage("unknown_operation", "operation is not permitted")
	}
	fields := map[string][]string{
		"sandbox_status":          {"version", "request_id", "operation"},
		"collect_performance":     {"version", "request_id", "operation", "phase"},
		"ensure_guest":            {"version", "request_id", "operation"},
		"stop_guest":              {"version", "request_id", "operation"},
		"start_sandbox":           {"version", "request_id", "operation", "instance"},
		"stop_sandbox":            {"version", "request_id", "operation", "instance"},
		"pause_sandbox":           {"version", "request_id", "operation", "instance"},
		"resume_sandbox":          {"version", "request_id", "operation", "instance"},
		"delete_sandbox":          {"version", "request_id", "operation", "instance"},
		"admit_launch":            {"version", "request_id", "operation", "instance"},
		"create_sandbox":          {"version", "request_id", "operation", "instance"},
		"resize_sandbox":          {"version", "request_id", "operation", "instance"},
		"describe_sandbox":        {"version", "request_id", "operation", "instance"},
		"set_sandbox_description": {"version", "request_id", "operation", "instance", "description"},
		"list_sandboxes":          {"version", "request_id", "operation"},
		"inspect_sandbox":         {"version", "request_id", "operation", "instance"},
		"sandbox_layout":          {"version", "request_id", "operation", "instance", "depth"},
		"list_repositories":       {"version", "request_id", "operation"},
		"locate_repository":       {"version", "request_id", "operation", "repository"},
	}
	required, ok := fields[operation]
	if !ok {
		return Request{}, fail("unknown_operation")
	}
	// budget_memory_mib and budget_cpu are the optional create_sandbox
	// fields; the exact-shape check admits each only when the caller
	// supplied it.
	if operation == "create_sandbox" {
		for _, field := range []string{"budget_memory_mib", "budget_cpu"} {
			if _, present := payload[field]; present {
				required = append(append([]string(nil), required...), field)
			}
		}
	}
	if operation == "resize_sandbox" {
		for _, field := range []string{"budget_memory_mib", "budget_cpu", "root_disk_mib"} {
			if _, present := payload[field]; present {
				required = append(append([]string(nil), required...), field)
			}
		}
		if len(required) == 4 {
			return Request{}, failMessage("invalid_fields", "resize_sandbox requires at least one of budget_memory_mib, budget_cpu, root_disk_mib")
		}
	}
	if err := requireExactFields(payload, required...); err != nil {
		return Request{}, err
	}
	if contains(required, "instance") {
		instance, ok := stringField(payload, "instance")
		if !ok || !ValidSandboxInstance(instance) {
			return Request{}, failMessage("invalid_instance", "instance is not permitted")
		}
	}
	if operation == "sandbox_layout" {
		depth, ok := numberIntField(payload, "depth")
		if !ok || depth < 1 || depth > 5 {
			return Request{}, failMessage("invalid_depth", "layout depth is not permitted")
		}
	}
	if operation == "create_sandbox" {
		if _, present := payload["budget_memory_mib"]; present {
			budget, ok := numberIntField(payload, "budget_memory_mib")
			if !ok || !ValidMemoryBudget(budget) {
				return Request{}, failMessage("invalid_budget", BudgetRangeMessage())
			}
		}
		if _, present := payload["budget_cpu"]; present {
			cpu, ok := numberIntField(payload, "budget_cpu")
			if !ok || !ValidCpuBudget(cpu) {
				return Request{}, failMessage("invalid_cpu_budget", CpuBudgetRangeMessage())
			}
		}
	}
	if operation == "resize_sandbox" {
		if _, present := payload["budget_memory_mib"]; present {
			budget, ok := numberIntField(payload, "budget_memory_mib")
			if !ok || !ValidMemoryBudget(budget) {
				return Request{}, failMessage("invalid_budget", BudgetRangeMessage())
			}
		}
		if _, present := payload["budget_cpu"]; present {
			cpu, ok := numberIntField(payload, "budget_cpu")
			if !ok || !ValidCpuBudget(cpu) {
				return Request{}, failMessage("invalid_cpu_budget", CpuBudgetRangeMessage())
			}
		}
		if _, present := payload["root_disk_mib"]; present {
			disk, ok := numberIntField(payload, "root_disk_mib")
			if !ok || !ValidRootDiskMib(disk) {
				return Request{}, failMessage("invalid_root_disk", RootDiskRangeMessage())
			}
		}
	}
	if operation == "locate_repository" {
		if _, err := requireIdentifier(payload, "repository"); err != nil {
			return Request{}, failMessage("invalid_repository", "repository identifier is not permitted")
		}
	}
	if operation == "set_sandbox_description" {
		description, ok := stringField(payload, "description")
		if !ok || !ValidSandboxDescription(description) {
			return Request{}, failMessage("invalid_description", "description must be one trimmed printable line of at most 120 characters")
		}
	}
	if operation == "collect_performance" {
		phase, ok := stringField(payload, "phase")
		if !ok || !phasePattern.MatchString(phase) {
			return Request{}, fail("invalid_phase")
		}
	}
	return Request{Operation: operation, Fields: payload}, nil
}

func parseStrictObject(raw []byte, maxBytes int) (map[string]interface{}, error) {
	if len(raw) == 0 || len(raw) > maxBytes {
		return nil, failMessage("invalid_size", "request size is invalid")
	}
	// Match the Python reference: invalid UTF-8 sequences (including
	// inside otherwise-valid JSON strings, where Go would silently
	// substitute U+FFFD) are a decode refusal.
	if !utf8.Valid(raw) {
		return nil, failMessage("invalid_json", "request must be one UTF-8 JSON object")
	}
	if !json.Valid(raw) {
		return nil, failMessage("invalid_json", "request must be one UTF-8 JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	tok, err := decoder.Token()
	if err != nil {
		return nil, failMessage("invalid_json", "request must be one UTF-8 JSON object")
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return nil, failMessage("invalid_json", "request must be one UTF-8 JSON object")
	}
	payload := map[string]interface{}{}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, failMessage("invalid_json", "request must be one UTF-8 JSON object")
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, failMessage("invalid_json", "request must be one UTF-8 JSON object")
		}
		if _, exists := payload[key]; exists {
			return nil, failMessage("duplicate_field", fmt.Sprintf("duplicate field '%s'", key))
		}
		var value interface{}
		if err := decoder.Decode(&value); err != nil {
			return nil, failMessage("invalid_json", "request must be one UTF-8 JSON object")
		}
		payload[key] = value
	}
	end, err := decoder.Token()
	if err != nil {
		return nil, failMessage("invalid_json", "request must be one UTF-8 JSON object")
	}
	if endDelim, ok := end.(json.Delim); !ok || endDelim != '}' {
		return nil, failMessage("invalid_json", "request must be one UTF-8 JSON object")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, failMessage("invalid_json", "request must be one UTF-8 JSON object")
	}
	return payload, nil
}

func fail(code string) error { return DecoderError{Code: code} }

func failMessage(code, message string) error { return DecoderError{Code: code, Message: message} }

func stringField(payload map[string]interface{}, name string) (string, bool) {
	value, ok := payload[name].(string)
	return value, ok
}

func numberIntField(payload map[string]interface{}, name string) (int64, bool) {
	value, ok := payload[name].(json.Number)
	if !ok {
		return 0, false
	}
	if strings.ContainsAny(value.String(), ".eE") {
		return 0, false
	}
	parsed, err := strconv.ParseInt(value.String(), 10, 64)
	return parsed, err == nil
}

func requireIdentifier(payload map[string]interface{}, name string) (string, error) {
	value, ok := stringField(payload, name)
	if !ok || !identifierPattern.MatchString(value) {
		return "", failMessage("invalid_identifier", name+" has an invalid format")
	}
	return value, nil
}

func requireExactFields(payload map[string]interface{}, fields ...string) error {
	if len(payload) != len(fields) {
		required := map[string]bool{}
		for _, field := range fields {
			required[field] = true
		}
		extra := []string{}
		for key := range payload {
			if !required[key] {
				extra = append(extra, key)
			}
		}
		sort.Strings(extra)
		parts := make([]string, 0, len(extra))
		for _, key := range extra {
			parts = append(parts, "extra="+key)
		}
		if len(parts) == 0 {
			parts = append(parts, "extra=<none>")
		}
		return failMessage("invalid_fields", "request fields are invalid: "+strings.Join(parts, ", "))
	}
	for _, field := range fields {
		if _, ok := payload[field]; !ok {
			return failMessage("invalid_fields", "request fields are invalid")
		}
	}
	return nil
}

func checkVersion(payload map[string]interface{}) error {
	version, ok := numberIntField(payload, "version")
	if !ok || version != ProtocolVersion {
		return failMessage("unsupported_version", "protocol version is not supported")
	}
	return nil
}

func contains(items []string, needle string) bool {
	for _, item := range items {
		if item == needle {
			return true
		}
	}
	return false
}
