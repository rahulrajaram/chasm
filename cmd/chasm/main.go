// Command chasm is the small human-facing sandbox CLI.
// Lifecycle operations use the audited supervisor/executor socket. Shell
// and foreground execution use separate sudo entries and enter validated
// managed instances as the unprivileged guest user.
package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/rahulrajaram/chasm/internal/hostidentity"
	"github.com/rahulrajaram/chasm/internal/incuslocal"
	"github.com/rahulrajaram/chasm/internal/protocol"
	"github.com/rahulrajaram/chasm/internal/siteconfig"
)

const (
	controlPath       = "/usr/local/bin/chasmctl"
	selfPath          = "/usr/local/bin/chasm"
	sudoPath          = "/usr/bin/sudo"
	incusPath         = "/usr/bin/incus"
	managedNamePrefix = "sandbox-"
	maxSandboxNameLen = 32
)

var resolvePrivilegedIncusSocket = incuslocal.ConfiguredSocketPath

var (
	errInvalidName         = errors.New("sandbox names must be 1-32 lowercase letters, digits, or hyphens, and must start and end with a letter or digit")
	errManagedPrefixName   = errors.New("NAME must be the logical sandbox name; chasm adds the sandbox- prefix itself, so a sandbox- NAME would double-prefix")
	errInvalidDescription  = errors.New("descriptions must be one trimmed printable line of 1-120 characters")
	errDefaultSandboxUnset = errors.New("the site declares no default_sandbox; an explicit sandbox NAME is required")
)

type executor func(path string, arguments, environment []string) error

type delegation struct {
	path       string
	arguments  []string
	jsonOutput bool
}

func main() {
	arguments := os.Args[1:]
	initSite := siteconfig.InitDefault
	if os.Geteuid() == 0 && len(arguments) > 0 && arguments[0] == "__exec" {
		initSite = func() error { return siteconfig.Init(siteconfig.DefaultPath) }
	}
	os.Exit(boot(arguments, initSite, os.Stdout, os.Stderr))
}

// boot dispatches help before the site descriptor is read — the same
// ordering chasmctl uses — so `chasm help`, `chasm help COMMAND`, and the
// --help forms work on a host whose descriptor is missing, unreadable, or
// invalid instead of failing at the descriptor. Doctor handles descriptor
// failures as observations. Other commands bind the site before dispatch.
func boot(arguments []string, initSite func() error, stdout, stderr *os.File) int {
	if topic, requested, err := requestedHelp(arguments); requested {
		return helpExit(topic, err, stdout, stderr)
	}
	if len(arguments) > 0 {
		switch arguments[0] {
		case "doctor":
			return runDoctor(arguments[1:], os.Geteuid(), stdout, stderr)
		case "__doctor":
			return runDoctorHelper(arguments[1:], os.Geteuid(), os.Getenv, stdout)
		}
	}
	if err := initSite(); err != nil {
		fmt.Fprintf(stderr, "chasm: site config: %v\n", err)
		return 1
	}
	// Clipboard bridging is an opt-in plugin (plugins/clipboard): the
	// standalone chasm-clipboard helper wraps this CLI explicitly
	// (`chasm-clipboard -- chasm shell NAME`). Core shell entry is the plain
	// process executor; no clipboard code is linked into this binary.
	return run(arguments, os.Geteuid(), os.Getenv, hostidentity.RequireSiteHost, execProcess, stdout, stderr)
}

type shellExit int

func (code shellExit) Error() string { return fmt.Sprintf("shell exited with status %d", code) }

func run(arguments []string, effectiveUID int, getenv func(string) string, requireHost func() error, execute executor, stdout, stderr *os.File) int {
	if topic, requested, err := requestedHelp(arguments); requested {
		return helpExit(topic, err, stdout, stderr)
	}
	if len(arguments) > 0 && arguments[0] == "__shell" {
		return runPrivilegedShell(arguments, effectiveUID, getenv, requireHost, execute, stderr)
	}
	if len(arguments) > 0 && arguments[0] == "__exec" {
		return runPrivilegedExec(arguments[1:], effectiveUID, getenv, requireHost, os.Stdin, stdout)
	}
	if effectiveUID == 0 {
		fmt.Fprintln(stderr, "chasm: commands must run as the operator, not root")
		return 1
	}
	if len(arguments) > 0 && arguments[0] == "exec" {
		return runExec(arguments[1:], getenv, os.Stdin, stdout, stderr)
	}

	invocation, err := parseOperatorCommand(stripEndOfOptions(arguments), arguments)
	if err != nil {
		reportUsageError(stderr, arguments, err)
		return 2
	}
	environment := controlEnvironment(getenv)
	if invocation.jsonOutput {
		for i, entry := range environment {
			if entry == "SANDBOXING_OUTPUT=human" {
				environment[i] = "SANDBOXING_OUTPUT=json"
			}
		}
	}
	return executeOrReport(execute, invocation.path, invocation.arguments, environment, stderr)
}

// helpExit answers a parsed help request. The request's own errors (an
// unknown topic, too many arguments) are usage errors and exit 2, matching
// every other usage path. Both main's pre-descriptor dispatch and run
// itself land here, so the two cannot drift apart.
func helpExit(topic string, err error, stdout, stderr *os.File) int {
	if err != nil {
		fmt.Fprintf(stderr, "chasm: %v\n", err)
		return 2
	}
	text, ok := helpText(topic)
	if !ok {
		fmt.Fprintf(stderr, "chasm: unknown help topic %q; run 'chasm help' to list commands\n", topic)
		return 2
	}
	if _, err := stdout.WriteString(text); err != nil {
		fmt.Fprintf(stderr, "chasm: writing help: %v\n", err)
		return 1
	}
	return 0
}

// stripEndOfOptions removes the first literal "--" from the arguments:
// option (and help-flag) scanning stops there, so everything after it is
// operand data. chasm describe NAME --set -- --help stores the literal
// text "--help" instead of showing help.
func stripEndOfOptions(arguments []string) []string {
	for index, argument := range arguments {
		if argument != "--" {
			continue
		}
		stripped := append([]string(nil), arguments[:index]...)
		return append(stripped, arguments[index+1:]...)
	}
	return arguments
}

// endsWithBareEndOfOptions reports whether the raw arguments end with the
// end-of-options marker itself: the operator wrote a trailing "--" where
// an operand (here TEXT) belongs, and it was consumed by
// stripEndOfOptions.
func endsWithBareEndOfOptions(arguments []string) bool {
	return len(arguments) > 0 && arguments[len(arguments)-1] == "--"
}

// reportUsageError names the specific problem and the command's own usage
// lines instead of only pointing at the global help.
func reportUsageError(stderr *os.File, arguments []string, err error) {
	fmt.Fprintf(stderr, "chasm: %v\n", err)
	topic := ""
	if len(arguments) > 0 {
		topic = arguments[0]
	}
	if usage, ok := usageBlock(topic); ok {
		fmt.Fprint(stderr, usage)
		return
	}
	fmt.Fprintln(stderr, "Run 'chasm help' for usage.")
}

func parseOperatorCommand(arguments []string, original []string) (delegation, error) {
	if len(arguments) == 0 {
		return controlDelegation("list-sandboxes"), nil
	}

	command := arguments[0]
	operands := arguments[1:]
	switch command {
	case "create":
		return createDelegation(operands)
	case "list":
		return zeroOperandControlDelegation("list", operands)
	case "show":
		return showDelegation(operands)
	case "describe":
		return descriptionDelegation(operands, original)
	case "inspect":
		return namedManagedControlDelegation("inspect", "inspect-sandbox", operands)
	case "layout":
		name, err := requiredSafeName("layout", operands)
		if err != nil {
			return delegation{}, err
		}
		return controlDelegation("sandbox-layout", managedInstanceName(name), "3"), nil
	case "repos":
		return repositoryDelegation("repos", operands)
	case "locate":
		query, err := requiredRepositoryQuery("locate", operands)
		if err != nil {
			return delegation{}, err
		}
		return controlDelegation("locate-repository", query), nil
	case "start":
		return lifecycleDelegation("start", "ensure-guest", "start-sandbox", operands)
	case "status":
		return lifecycleDelegation("status", "sandbox-status", "inspect-sandbox", operands)
	case "stop":
		return lifecycleDelegation("stop", "stop-guest", "stop-sandbox", operands)
	case "resize":
		return resizeDelegation(operands)
	case "pause":
		return namedManagedControlDelegation("pause", "pause-sandbox", operands)
	case "resume":
		return namedManagedControlDelegation("resume", "resume-sandbox", operands)
	case "delete":
		if len(operands) != 1 {
			return delegation{}, errors.New("delete takes one NAME")
		}
		// Contract: delete takes the LOGICAL name and derives the
		// managed instance name. A full instance name here would
		// double-prefix into a harmless-but-confusing phantom; refuse
		// it with the cause named.
		name, err := safeLogicalName(operands[0])
		if err != nil {
			return delegation{}, err
		}
		return controlDelegation("delete-sandbox", managedInstanceName(name)), nil
	case "shell":
		return shellDelegation(operands)
	default:
		return delegation{}, fmt.Errorf("unknown command %q", command)
	}
}

func shellDelegation(operands []string) (delegation, error) {
	name := ""
	switch {
	case len(operands) == 0:
		defaultName, err := defaultSandboxName()
		if err != nil {
			return delegation{}, err
		}
		name = defaultName
	case len(operands) == 1:
		name = operands[0]
	default:
		return delegation{}, errors.New("shell takes one optional NAME")
	}
	name, err := safeLogicalName(name)
	if err != nil {
		return delegation{}, err
	}
	return delegation{path: sudoPath, arguments: []string{"-n", selfPath, "__shell", name}}, nil
}

// createDelegation parses `create NAME [BUDGET_MEMORY_MIB] [--memory MIB]
// [--cpu N]`. The memory budget keeps its historical positional form and
// gains a --memory spelling; the CPU budget is flag-only. Both flags may
// appear before or after the positionals; --cpu is forwarded to chasmctl,
// which validates it against the protocol range before any transport.
func createDelegation(operands []string) (delegation, error) {
	var positional []string
	cpu := ""
	for index := 0; index < len(operands); index++ {
		switch operands[index] {
		case "--memory", "--cpu":
			if index+1 >= len(operands) {
				return delegation{}, fmt.Errorf("create's %s requires a value", operands[index])
			}
			if operands[index] == "--cpu" {
				cpu = operands[index+1]
			} else {
				positional = append(positional, operands[index+1])
			}
			index++
		default:
			positional = append(positional, operands[index])
		}
	}
	if len(positional) != 1 && len(positional) != 2 {
		return delegation{}, errors.New("create takes one NAME and an optional BUDGET_MEMORY_MIB ([--memory MIB] [--cpu N])")
	}
	name, err := requiredSafeName("create", positional[:1])
	if err != nil {
		return delegation{}, err
	}
	arguments := []string{"create-sandbox", managedInstanceName(name)}
	if len(positional) == 2 {
		if !isDecimalDigits(positional[1]) {
			return delegation{}, errors.New("create's BUDGET_MEMORY_MIB must be a whole number of MiB")
		}
		arguments = append(arguments, positional[1])
	}
	if cpu != "" {
		arguments = append(arguments, "--cpu", cpu)
	}
	return controlDelegation(arguments[0], arguments[1:]...), nil
}

// resizeDelegation parses `resize NAME [--memory SIZE] [--cpu N]
// [--root-disk SIZE]`. At least one target flag is required. SIZE keeps the
// operator's spelling (plain MiB integers like create, or explicit
// GiB/MiB suffixes) and is converted to whole MiB by chasmctl; NAME is
// validated locally because every delegation derives the managed instance
// name from the logical name.
func resizeDelegation(operands []string) (delegation, error) {
	if len(operands) == 0 {
		return delegation{}, errors.New("resize takes one NAME and at least one of --memory SIZE, --cpu N, --root-disk SIZE")
	}
	name, err := requiredSafeName("resize", operands[:1])
	if err != nil {
		return delegation{}, err
	}
	flags := 0
	for index := 1; index < len(operands); index++ {
		switch operands[index] {
		case "--memory", "--cpu", "--root-disk":
			if index+1 >= len(operands) {
				return delegation{}, fmt.Errorf("resize's %s requires a value", operands[index])
			}
			flags++
			index++
		default:
			return delegation{}, fmt.Errorf("resize accepts only --memory SIZE, --cpu N, and --root-disk SIZE")
		}
	}
	if flags == 0 {
		return delegation{}, errors.New("resize requires at least one of --memory SIZE, --cpu N, --root-disk SIZE")
	}
	return controlDelegation("resize-sandbox", append([]string{managedInstanceName(name)}, operands[1:]...)...), nil
}

func showDelegation(operands []string) (delegation, error) {
	if len(operands) > 0 && operands[len(operands)-1] == "--json" {
		invocation, err := showDelegationWithoutJSON(operands[:len(operands)-1])
		invocation.jsonOutput = true
		return invocation, err
	}
	return showDelegationWithoutJSON(operands)
}

func showDelegationWithoutJSON(operands []string) (delegation, error) {
	if len(operands) == 0 {
		return controlDelegation("sandbox-status"), nil
	}
	if len(operands) == 1 {
		return namedManagedControlDelegation("show", "inspect-sandbox", operands)
	}
	if len(operands) == 2 && operands[1] == "--layout" {
		name, err := requiredSafeName("show", operands[:1])
		if err != nil {
			return delegation{}, err
		}
		return controlDelegation("sandbox-layout", managedInstanceName(name), "3"), nil
	}
	return delegation{}, errors.New("show takes an optional NAME and an optional --layout")
}

func repositoryDelegation(command string, operands []string) (delegation, error) {
	if len(operands) == 0 {
		return controlDelegation("list-repositories"), nil
	}
	query, err := requiredRepositoryQuery(command, operands)
	if err != nil {
		return delegation{}, err
	}
	return controlDelegation("locate-repository", query), nil
}

func descriptionDelegation(operands []string, original []string) (delegation, error) {
	if len(operands) == 1 {
		name, err := requiredSafeName("describe", operands)
		if err != nil {
			return delegation{}, err
		}
		return controlDelegation("describe-sandbox", managedInstanceName(name)), nil
	}
	if len(operands) == 2 && operands[1] == "--set" && endsWithBareEndOfOptions(original) {
		// The trailing "--" was consumed by stripEndOfOptions as the
		// end-of-options marker, so --set stands without its TEXT: point
		// at the form that stores literal text starting with "-".
		return delegation{}, errors.New("describe --set requires TEXT; to store literal text that starts with '-', give it after the marker: chasm describe NAME --set -- --text")
	}
	if len(operands) == 2 && operands[1] == "--clear" {
		name, err := requiredSafeName("describe", operands[:1])
		if err != nil {
			return delegation{}, err
		}
		return controlDelegation("set-sandbox-description", managedInstanceName(name), ""), nil
	}
	if len(operands) == 3 && operands[1] == "--set" {
		name, err := requiredSafeName("describe", operands[:1])
		if err != nil {
			return delegation{}, err
		}
		description := operands[2]
		if description == "" || !protocol.ValidSandboxDescription(description) {
			return delegation{}, errInvalidDescription
		}
		return controlDelegation("set-sandbox-description", managedInstanceName(name), description), nil
	}
	return delegation{}, errors.New("describe takes one NAME with an optional --set TEXT or --clear")
}

func zeroOperandControlDelegation(command string, operands []string) (delegation, error) {
	if len(operands) != 0 {
		return delegation{}, fmt.Errorf("%s takes no arguments", command)
	}
	return controlDelegation("list-sandboxes"), nil
}

func namedManagedControlDelegation(command string, operation string, operands []string) (delegation, error) {
	name, err := requiredSafeName(command, operands)
	if err != nil {
		return delegation{}, err
	}
	return controlDelegation(operation, managedInstanceName(name)), nil
}

func lifecycleDelegation(command, legacyOperation, namedOperation string, operands []string) (delegation, error) {
	if len(operands) == 0 {
		return controlDelegation(legacyOperation), nil
	}
	if len(operands) != 1 {
		return delegation{}, fmt.Errorf("%s takes one optional NAME", command)
	}
	return namedManagedControlDelegation(command, namedOperation, operands)
}

func controlDelegation(operation string, operands ...string) delegation {
	return delegation{path: controlPath, arguments: append([]string{operation}, operands...)}
}

func defaultSandboxName() (string, error) {
	if name := siteconfig.Active().DefaultSandbox; name != "" {
		return name, nil
	}
	return "", errDefaultSandboxUnset
}

// requiredSafeName validates the one NAME operand of command; the usage
// error names the command so the operator sees the specific problem.
func requiredSafeName(command string, operands []string) (string, error) {
	if len(operands) != 1 {
		return "", fmt.Errorf("%s takes one NAME", command)
	}
	return safeLogicalName(operands[0])
}

// safeLogicalName refuses the managed instance prefix — chasm adds the
// sandbox- prefix itself — and validates the logical name grammar.
func safeLogicalName(name string) (string, error) {
	if strings.HasPrefix(name, managedNamePrefix) {
		return "", errManagedPrefixName
	}
	if !isSafeName(name) {
		return "", errInvalidName
	}
	return name, nil
}

func requiredRepositoryQuery(command string, operands []string) (string, error) {
	if len(operands) != 1 {
		return "", fmt.Errorf("%s takes one REPOSITORY", command)
	}
	if !isSafeRepositoryQuery(operands[0]) {
		return "", fmt.Errorf("%s takes one safe repository basename", command)
	}
	return operands[0], nil
}

func isSafeRepositoryQuery(query string) bool {
	if query == "" || len(query) > 128 || query == "." || query == ".." || !utf8.ValidString(query) {
		return false
	}
	for index, character := range query {
		isAlphaNumeric := character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9'
		if !isAlphaNumeric && !strings.ContainsRune("._-", character) {
			return false
		}
		if index == 0 && (character == '-' || character == '.') {
			return false
		}
	}
	return true
}

func isSafeName(name string) bool {
	if name == "" || len(name) > maxSandboxNameLen || !utf8.ValidString(name) {
		return false
	}
	for index, character := range name {
		isAlphaNumeric := character >= 'a' && character <= 'z' || character >= '0' && character <= '9'
		if !isAlphaNumeric && character != '-' {
			return false
		}
		if character == '-' && (index == 0 || index == len(name)-1) {
			return false
		}
	}
	return true
}

func isDecimalDigits(value string) bool {
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

func managedInstanceName(logicalName string) string {
	return managedNamePrefix + logicalName
}

func runPrivilegedShell(arguments []string, effectiveUID int, getenv func(string) string, requireHost func() error, execute executor, stderr *os.File) int {
	if effectiveUID != 0 || getenv("SUDO_USER") != siteconfig.Active().OperatorUser {
		fmt.Fprintln(stderr, "chasm: privileged shell entry was refused")
		return 1
	}
	name, err := requiredSafeName("shell", arguments[1:])
	if err != nil {
		fmt.Fprintln(stderr, "chasm: privileged shell entry was refused")
		return 1
	}
	if err := requireHost(); err != nil {
		fmt.Fprintf(stderr, "chasm: host identity check failed: %v\n", err)
		return 1
	}
	socketPath, err := resolvePrivilegedIncusSocket()
	if err != nil {
		fmt.Fprintf(stderr, "chasm: local Incus socket unavailable: %v\n", err)
		return 1
	}
	if code := shellTargetError(socketPath, managedInstanceName(name)); code != "" {
		fmt.Fprintf(stderr, "chasm: %s\n", execResultMessage(code, name))
		return 125
	}
	environment := append(baseEnvironment(getenv), "INCUS_SOCKET="+socketPath)
	return executeOrReport(execute, incusPath, shellArguments(managedInstanceName(name), safeTerm(getenv("TERM")), siteconfig.Active().GuestUID), environment, stderr)
}

func executeOrReport(execute executor, path string, arguments, environment []string, stderr *os.File) int {
	if err := execute(path, arguments, environment); err != nil {
		var code shellExit
		if errors.As(err, &code) {
			return int(code)
		}
		fmt.Fprintf(stderr, "chasm: %v\n", err)
		return 1
	}
	return 0
}

func execProcess(path string, arguments, environment []string) error {
	return syscall.Exec(path, append([]string{path}, arguments...), environment)
}

func baseEnvironment(getenv func(string) string) []string {
	environment := []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	if term := safeTerm(getenv("TERM")); term != "" {
		environment = append(environment, "TERM="+term)
	}
	for _, name := range []string{"SUDO_UID", "SUDO_USER"} {
		if value := getenv(name); value != "" {
			environment = append(environment, name+"="+value)
		}
	}
	return environment
}

func controlEnvironment(getenv func(string) string) []string {
	environment := append(baseEnvironment(getenv), "SANDBOXING_OUTPUT=human")
	if getenv("NO_COLOR") != "" {
		environment = append(environment, "NO_COLOR=1")
	}
	if columns := getenv("COLUMNS"); safeColumns(columns) {
		environment = append(environment, "COLUMNS="+columns)
	}
	return environment
}

func safeColumns(value string) bool {
	if value == "" || len(value) > 3 {
		return false
	}
	parsed := 0
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
		parsed = parsed*10 + int(character-'0')
	}
	return parsed >= 40 && parsed <= 500
}

func shellArguments(instanceName, term string, guestUID int) []string {
	if term == "" {
		term = "xterm-256color"
	}
	guest := strconv.Itoa(guestUID)
	return []string{
		"--force-local", "exec", instanceName,
		"--user", guest, "--group", guest, "--cwd", "/home/agent",
		"--env", "HOME=/home/agent", "--env", "USER=agent", "--env", "LOGNAME=agent",
		"--env", "SHELL=/usr/bin/zsh", "--env", "TERM=" + term,
		"--mode", "interactive", "--", "/usr/bin/zsh", "-l",
	}
}

func safeTerm(value string) string {
	if len(value) == 0 || len(value) > 64 {
		return ""
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z') &&
			!(character >= 'A' && character <= 'Z') &&
			!(character >= '0' && character <= '9') &&
			!strings.ContainsRune("._+-", character) {
			return ""
		}
	}
	return value
}
