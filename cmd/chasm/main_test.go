package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rahulrajaram/chasm/internal/siteconfig"
	"github.com/rahulrajaram/chasm/internal/testsupport"
)

// moduleRoot returns the absolute repository root. Fixture paths must stay
// absolute because the descriptor loader refuses relative paths on purpose.
func moduleRoot() string {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		panic(err)
	}
	return root
}

type invocation struct {
	path        string
	arguments   []string
	environment []string
}

func captureExecutor(target *invocation) executor {
	return func(path string, arguments, environment []string) error {
		*target = invocation{path: path, arguments: append([]string(nil), arguments...), environment: append([]string(nil), environment...)}
		return nil
	}
}

func testEnvironment(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func discardFile(t *testing.T) *os.File {
	t.Helper()
	file, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

// captureFile returns a writable scratch file; capturedText reads it back.
func captureFile(t *testing.T) *os.File {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "capture")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

func capturedText(t *testing.T, file *os.File) string {
	t.Helper()
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestMain(m *testing.M) {
	path, cleanup, err := testsupport.TempSiteConfig(filepath.Join(moduleRoot(), "testdata", "fixtures", "testsite", "site.conf"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "chasm tests: copy site: %v\n", err)
		os.Exit(1)
	}
	if err := siteconfig.Init(path); err != nil {
		fmt.Fprintf(os.Stderr, "chasm tests: bind site: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func TestClipboardShellPreservesExitStatus(t *testing.T) {
	stderr := captureFile(t)
	code := executeOrReport(func(string, []string, []string) error { return shellExit(7) }, "unused", nil, nil, stderr)
	if code != 7 || capturedText(t, stderr) != "" {
		t.Fatalf("shell exit=%d, expected 7 without wrapper error text", code)
	}
}

// bindTestSite re-binds the neutral testsite descriptor (no default_sandbox,
// operator_user chasmops, guest_uid 1001).
func bindTestSite(t *testing.T) {
	t.Helper()
	path, err := testsupport.CopySiteConfig(filepath.Join(moduleRoot(), "testdata", "fixtures", "testsite", "site.conf"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := siteconfig.Init(path); err != nil {
		t.Fatal(err)
	}
}

// bindSiteWithDefaultSandbox binds the testsite descriptor with a declared
// default_sandbox, restoring the neutral descriptor afterwards.
func bindSiteWithDefaultSandbox(t *testing.T, name string) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(moduleRoot(), "testdata", "fixtures", "testsite", "site.conf"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(content), "default_sandbox=\n", "default_sandbox="+name+"\n", 1)
	path := filepath.Join(t.TempDir(), "site.conf")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := siteconfig.Init(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bindTestSite(t) })
}

// TestHelpIsAnsweredWithoutAReadableSiteDescriptor pins the chasmctl-style
// dispatch order: every help form is answered before the site descriptor is
// read, so CHASM_SITE_CONF pointing at a nonexistent descriptor still yields
// working help instead of "chasm: site config: ...".
func TestHelpIsAnsweredWithoutAReadableSiteDescriptor(t *testing.T) {
	t.Setenv("CHASM_SITE_CONF", filepath.Join(t.TempDir(), "absent-site.conf"))
	// The binding this environment would perform really does fail, so the
	// help below is proven to come from the pre-descriptor dispatch.
	if err := siteconfig.InitDefault(); err == nil {
		t.Fatal("binding a nonexistent descriptor unexpectedly succeeded")
	}
	tests := []struct {
		arguments []string
		usage     string
	}{
		{[]string{"help"}, "chasm COMMAND [OPTIONS]"},
		{[]string{"help", "pause"}, "chasm pause NAME"},
		{[]string{"help", "resume"}, "chasm resume NAME"},
		{[]string{"--help"}, "chasm COMMAND [OPTIONS]"},
		{[]string{"-h"}, "chasm COMMAND [OPTIONS]"},
		{[]string{"pause", "--help"}, "chasm pause NAME"},
		{[]string{"create", "-h"}, "chasm create NAME"},
	}
	for _, testCase := range tests {
		t.Run(strings.Join(testCase.arguments, "-"), func(t *testing.T) {
			output := captureFile(t)
			code := boot(testCase.arguments, siteconfig.InitDefault, output, discardFile(t))
			text := capturedText(t, output)
			if code != 0 || !strings.Contains(text, "Usage:\n") || !strings.Contains(text, testCase.usage) {
				t.Fatalf("arguments=%q code=%d must print help with %q:\n%s", testCase.arguments, code, testCase.usage, text)
			}
			if strings.Contains(text, "site config") {
				t.Fatalf("help output leaked a descriptor error:\n%s", text)
			}
		})
	}
}

func TestHelpWriteFailureIsAnError(t *testing.T) {
	output := captureFile(t)
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	stderr := captureFile(t)
	if code := boot([]string{"help"}, siteconfig.InitDefault, output, stderr); code != 1 {
		t.Fatalf("closed help output exit = %d, want 1", code)
	}
	if message := capturedText(t, stderr); !strings.Contains(message, "writing help") {
		t.Fatalf("missing write failure diagnostic: %q", message)
	}
}

// TestNonHelpCommandsStillRequireTheSiteDescriptor guards the other side of
// the dispatch order: only help runs before the descriptor binding, so a
// missing descriptor still fails a real command with the site-config error.
func TestNonHelpCommandsStillRequireTheSiteDescriptor(t *testing.T) {
	t.Setenv("CHASM_SITE_CONF", filepath.Join(t.TempDir(), "absent-site.conf"))
	stderr := captureFile(t)
	code := boot([]string{"list"}, siteconfig.InitDefault, discardFile(t), stderr)
	output := capturedText(t, stderr)
	if code != 1 || !strings.Contains(output, "chasm: site config:") {
		t.Fatalf("code=%d stderr must report the unreadable descriptor:\n%s", code, output)
	}
}

func TestCommandsDelegateOnlyToFixedControlOperations(t *testing.T) {
	bindSiteWithDefaultSandbox(t, "demo")
	tests := []struct {
		arguments []string
		want      []string
	}{
		{nil, []string{"list-sandboxes"}},
		{[]string{"create", "research"}, []string{"create-sandbox", "sandbox-research"}},
		{[]string{"create", "research", "2048"}, []string{"create-sandbox", "sandbox-research", "2048"}},
		{[]string{"create", "research", "--memory", "4096"}, []string{"create-sandbox", "sandbox-research", "4096"}},
		{[]string{"create", "research", "--cpu", "4"}, []string{"create-sandbox", "sandbox-research", "--cpu", "4"}},
		{[]string{"create", "research", "2048", "--cpu", "4"}, []string{"create-sandbox", "sandbox-research", "2048", "--cpu", "4"}},
		{[]string{"list"}, []string{"list-sandboxes"}},
		{[]string{"show"}, []string{"sandbox-status"}},
		{[]string{"show", "research"}, []string{"inspect-sandbox", "sandbox-research"}},
		{[]string{"show", "research", "--layout"}, []string{"sandbox-layout", "sandbox-research", "3"}},
		{[]string{"describe", "research"}, []string{"describe-sandbox", "sandbox-research"}},
		{[]string{"describe", "research", "--set", "LLM evaluation and tooling"}, []string{"set-sandbox-description", "sandbox-research", "LLM evaluation and tooling"}},
		{[]string{"describe", "research", "--set", "--verbose note"}, []string{"set-sandbox-description", "sandbox-research", "--verbose note"}},
		{[]string{"describe", "research", "--clear"}, []string{"set-sandbox-description", "sandbox-research", ""}},
		{[]string{"inspect", "research"}, []string{"inspect-sandbox", "sandbox-research"}},
		{[]string{"layout", "research"}, []string{"sandbox-layout", "sandbox-research", "3"}},
		{[]string{"repos"}, []string{"list-repositories"}},
		{[]string{"repos", "demo"}, []string{"locate-repository", "demo"}},
		{[]string{"locate", "demo"}, []string{"locate-repository", "demo"}},
		{[]string{"start", "research"}, []string{"start-sandbox", "sandbox-research"}},
		{[]string{"pause", "research"}, []string{"pause-sandbox", "sandbox-research"}},
		{[]string{"resume", "research"}, []string{"resume-sandbox", "sandbox-research"}},
		{[]string{"status", "research"}, []string{"inspect-sandbox", "sandbox-research"}},
		{[]string{"stop", "research"}, []string{"stop-sandbox", "sandbox-research"}},
		{[]string{"delete", "research"}, []string{"delete-sandbox", "sandbox-research"}},
		{[]string{"resize", "research", "--memory", "8GiB", "--cpu", "4"}, []string{"resize-sandbox", "sandbox-research", "--memory", "8GiB", "--cpu", "4"}},
		{[]string{"resize", "research", "--root-disk", "100GiB"}, []string{"resize-sandbox", "sandbox-research", "--root-disk", "100GiB"}},
		{[]string{"resize", "research", "--memory", "2048"}, []string{"resize-sandbox", "sandbox-research", "--memory", "2048"}},
	}
	for _, testCase := range tests {
		name := strings.Join(testCase.arguments, "-")
		if name == "" {
			name = "no-arguments"
		}
		t.Run(name, func(t *testing.T) {
			var got invocation
			code := run(testCase.arguments, 1000, testEnvironment(nil), func() error {
				t.Fatal("control command performed a root host check")
				return nil
			}, captureExecutor(&got), discardFile(t), discardFile(t))
			if code != 0 || got.path != controlPath || !reflect.DeepEqual(got.arguments, testCase.want) {
				t.Fatalf("code=%d invocation=%+v", code, got)
			}
		})
	}
}

func TestControlCommandsRequestHumanOutputAndPassOnlySafeDisplayHints(t *testing.T) {
	var got invocation
	values := map[string]string{"NO_COLOR": "anything", "COLUMNS": "88", "TERM": "xterm-256color"}
	code := run([]string{"list"}, 1000, testEnvironment(values), func() error { return nil }, captureExecutor(&got), discardFile(t), discardFile(t))
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	joined := strings.Join(got.environment, "\n")
	for _, expected := range []string{"SANDBOXING_OUTPUT=human", "NO_COLOR=1", "COLUMNS=88", "TERM=xterm-256color"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("environment lacks %q: %v", expected, got.environment)
		}
	}

	got = invocation{}
	values["COLUMNS"] = "80;id"
	code = run([]string{"list"}, 1000, testEnvironment(values), func() error { return nil }, captureExecutor(&got), discardFile(t), discardFile(t))
	if code != 0 || strings.Contains(strings.Join(got.environment, "\n"), "COLUMNS=") {
		t.Fatalf("unsafe columns reached child: code=%d env=%v", code, got.environment)
	}
}

func TestDeleteRefusesMissingOrExtraOperands(t *testing.T) {
	for _, arguments := range [][]string{{"delete"}, {"delete", "research", "extra"}} {
		t.Run(strings.Join(arguments, "-"), func(t *testing.T) {
			var got invocation
			code := run(arguments, 1000, testEnvironment(nil), func() error { return nil }, captureExecutor(&got), discardFile(t), discardFile(t))
			if code != 2 {
				t.Fatalf("arguments=%q code=%d want 2", arguments, code)
			}
		})
	}
}

func TestDeleteRequiresAnExplicitNameAndNeverDefaultsToASandbox(t *testing.T) {
	// contrast start/stop/status: no-args delete must not map to a sandbox op
	var got invocation
	if code := run([]string{"delete"}, 1000, testEnvironment(nil), func() error { return nil }, captureExecutor(&got), discardFile(t), discardFile(t)); code != 2 || len(got.arguments) != 0 {
		t.Fatalf("no-args delete code=%d invocation=%+v", code, got)
	}
}

func TestDeleteRefusesFullInstanceNamesWithTheCauseNamed(t *testing.T) {
	// Contract: a full instance name would double-prefix into a
	// phantom; the refusal must name the cause instead of passing a
	// sandbox-sandbox-* operand to the executor.
	var got invocation
	stderr := discardFile(t)
	code := run([]string{"delete", "sandbox-research"}, 1000, testEnvironment(nil), func() error { return nil }, captureExecutor(&got), stderr, stderr)
	if code != 2 || len(got.arguments) != 0 {
		t.Fatalf("instance-name delete code=%d invocation=%+v", code, got)
	}
}

// commandStderr captures the stderr of one run for usage-error assertions.
func commandStderr(t *testing.T, arguments []string) (string, int) {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "chasm-stderr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	executed := false
	code := run(arguments, 1000, testEnvironment(nil), func() error { return nil }, func(string, []string, []string) error {
		executed = true
		return nil
	}, discardFile(t), file)
	if executed {
		t.Fatalf("arguments=%q executed despite invalid usage", arguments)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	output, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(output), code
}

func TestInvalidArgumentsNameTheProblemAndTheCommandUsage(t *testing.T) {
	tests := []struct {
		arguments []string
		problem   string
		usage     string
	}{
		{[]string{"create"}, "create takes one NAME and an optional BUDGET_MEMORY_MIB", "chasm create NAME [BUDGET_MEMORY_MIB]"},
		{[]string{"create", "research", "thirty"}, "create's BUDGET_MEMORY_MIB must be a whole number of MiB", "chasm create NAME [BUDGET_MEMORY_MIB]"},
		{[]string{"list", "research"}, "list takes no arguments", "chasm list"},
		{[]string{"show", "research", "--unknown"}, "show takes an optional NAME and an optional --layout", "chasm show NAME --layout"},
		{[]string{"describe", "research", "text"}, "describe takes one NAME with an optional --set TEXT or --clear", "chasm describe NAME --set TEXT"},
		{[]string{"inspect", "research", "extra"}, "inspect takes one NAME", "chasm inspect NAME"},
		{[]string{"status", "a", "b"}, "status takes one optional NAME", "chasm status [NAME]"},
		{[]string{"layout"}, "layout takes one NAME", "chasm layout NAME"},
		{[]string{"repos", "research", "extra"}, "repos takes one REPOSITORY", "chasm repos REPOSITORY"},
		{[]string{"locate"}, "locate takes one REPOSITORY", "chasm locate REPOSITORY"},
		{[]string{"start", "a", "b"}, "start takes one optional NAME", "chasm start [NAME]"},
		{[]string{"stop", "a", "b"}, "stop takes one optional NAME", "chasm stop [NAME]"},
		{[]string{"pause"}, "pause takes one NAME", "chasm pause NAME"},
		{[]string{"resume"}, "resume takes one NAME", "chasm resume NAME"},
		{[]string{"pause", "a", "b"}, "pause takes one NAME", "chasm pause NAME"},
		{[]string{"delete"}, "delete takes one NAME", "chasm delete NAME"},
		{[]string{"shell", "a", "b"}, "shell takes one optional NAME", "chasm shell [NAME]"},
		{[]string{"unknown-command"}, `unknown command "unknown-command"`, "chasm COMMAND [OPTIONS]"},
	}
	for _, testCase := range tests {
		t.Run(strings.Join(testCase.arguments, "-"), func(t *testing.T) {
			output, code := commandStderr(t, testCase.arguments)
			if code != 2 {
				t.Fatalf("arguments=%q code=%d want 2\nstderr:\n%s", testCase.arguments, code, output)
			}
			if !strings.Contains(output, "chasm: "+testCase.problem) || !strings.Contains(output, "Usage:\n") || !strings.Contains(output, testCase.usage) {
				t.Fatalf("arguments=%q stderr must name %q and the usage %q:\n%s", testCase.arguments, testCase.problem, testCase.usage, output)
			}
		})
	}
}

func TestLeadingEndOfOptionsMarkerIsAUsageErrorNotAnUnknownTopic(t *testing.T) {
	// chasm -- --help: everything after a leading marker is operand data,
	// so this must be the command parser's usage error, not "unknown help
	// topic".
	output, code := commandStderr(t, []string{"--", "--help"})
	if code != 2 {
		t.Fatalf("code=%d want 2\nstderr:\n%s", code, output)
	}
	if !strings.Contains(output, `unknown command "--help"`) || strings.Contains(output, "unknown help topic") {
		t.Fatalf("stderr must name the unknown command, not an unknown help topic:\n%s", output)
	}
}

func TestDescribeSetWithBareTrailingMarkerPointsAtTheEscapedForm(t *testing.T) {
	// chasm describe NAME --set -- : the trailing marker was consumed as
	// the end-of-options marker, leaving --set without TEXT; the error
	// must point at the form that stores literal text starting with '-'.
	output, code := commandStderr(t, []string{"describe", "research", "--set", "--"})
	if code != 2 {
		t.Fatalf("code=%d want 2\nstderr:\n%s", code, output)
	}
	if !strings.Contains(output, "describe --set requires TEXT") ||
		!strings.Contains(output, "chasm describe NAME --set -- --text") {
		t.Fatalf("stderr must point at the escaped --set form:\n%s", output)
	}

	// The escaped form itself keeps storing literal text.
	var got invocation
	code = run([]string{"describe", "research", "--set", "--", "-note"}, 1000, testEnvironment(nil), func() error { return nil }, captureExecutor(&got), discardFile(t), discardFile(t))
	if code != 0 || !reflect.DeepEqual(got.arguments, []string{"set-sandbox-description", "sandbox-research", "-note"}) {
		t.Fatalf("escaped --set code=%d invocation=%+v", code, got)
	}
}

func TestDescribeHelpDocumentsTheMarkerForLiteralText(t *testing.T) {
	output := commandOutput(t, []string{"help", "describe"})
	if !strings.Contains(output, "chasm describe NAME --set -- --text") {
		t.Fatalf("describe help must document the marker form for literal text:\n%s", output)
	}
}

func TestHelpFlagAnywhereInCommandArgumentsShowsThatCommandHelp(t *testing.T) {
	tests := []struct {
		arguments []string
		usage     string
	}{
		{[]string{"create", "research", "--help"}, "chasm create NAME [BUDGET_MEMORY_MIB]"},
		{[]string{"create", "-h", "research"}, "chasm create NAME [BUDGET_MEMORY_MIB]"},
		{[]string{"create", "research", "2048", "--help"}, "chasm create NAME [BUDGET_MEMORY_MIB]"},
		{[]string{"describe", "research", "--set", "-h"}, "chasm describe NAME --set TEXT"},
		{[]string{"show", "research", "--layout", "--help"}, "chasm show NAME --layout"},
		{[]string{"repos", "demo", "--help"}, "chasm repos REPOSITORY"},
		{[]string{"stop", "-h"}, "chasm stop [NAME]"},
	}
	for _, testCase := range tests {
		t.Run(strings.Join(testCase.arguments, "-"), func(t *testing.T) {
			output := commandOutput(t, testCase.arguments)
			if !strings.Contains(output, "Usage:\n") || !strings.Contains(output, testCase.usage) {
				t.Fatalf("arguments=%q help lacks the usage %q:\n%s", testCase.arguments, testCase.usage, output)
			}
		})
	}
}

func TestEndOfOptionsMarkerStopsHelpAndFlagScanning(t *testing.T) {
	// "--" ends option scanning: describe NAME --set -- --help must store
	// the literal text "--help" instead of showing help.
	bindSiteWithDefaultSandbox(t, "demo")
	var got invocation
	code := run([]string{"describe", "research", "--set", "--", "--help"}, 1000, testEnvironment(nil), func() error { return nil }, captureExecutor(&got), discardFile(t), discardFile(t))
	if code != 0 || got.path != controlPath || !reflect.DeepEqual(got.arguments, []string{"set-sandbox-description", "sandbox-research", "--help"}) {
		t.Fatalf("code=%d invocation=%+v", code, got)
	}

	// Help-anywhere still works before the marker.
	output := commandOutput(t, []string{"create", "research", "--help", "--"})
	if !strings.Contains(output, "Usage:\n") || !strings.Contains(output, "chasm create NAME") {
		t.Fatalf("help flag before -- did not show command help:\n%s", output)
	}
	output = commandOutput(t, []string{"show", "research", "--help", "--"})
	if !strings.Contains(output, "Usage:\n") || !strings.Contains(output, "chasm show NAME --layout") {
		t.Fatalf("help flag before -- did not show command help:\n%s", output)
	}
	// A help flag after the marker is operand data, not a help request: the
	// command still parses (and here fails its arity check) instead of being
	// intercepted as help.
	stdErr, code := commandStderr(t, []string{"show", "research", "--layout", "--", "--help"})
	if code != 2 || !strings.Contains(stdErr, "show takes an optional NAME and an optional --layout") {
		t.Fatalf("arguments after -- were not treated as operands: code=%d stderr:\n%s", code, stdErr)
	}
}

func TestManagedPrefixNamesAreRefusedOnEveryNamedCommand(t *testing.T) {
	// chasm adds the sandbox- prefix itself, so a prefixed NAME would
	// double-prefix (chasm create sandbox-foo -> sandbox-sandbox-foo); every
	// command that takes a NAME must refuse it with that explanation.
	for _, arguments := range [][]string{
		{"create", "sandbox-research"},
		{"show", "sandbox-research"},
		{"show", "sandbox-research", "--layout"},
		{"describe", "sandbox-research"},
		{"inspect", "sandbox-research"},
		{"layout", "sandbox-research"},
		{"start", "sandbox-research"},
		{"status", "sandbox-research"},
		{"stop", "sandbox-research"},
		{"pause", "sandbox-research"},
		{"resume", "sandbox-research"},
		{"delete", "sandbox-research"},
		{"shell", "sandbox-research"},
	} {
		t.Run(strings.Join(arguments, "-"), func(t *testing.T) {
			output, code := commandStderr(t, arguments)
			if code != 2 {
				t.Fatalf("arguments=%q code=%d want 2\nstderr:\n%s", arguments, code, output)
			}
			if !strings.Contains(output, "chasm adds the sandbox- prefix itself") {
				t.Fatalf("arguments=%q refusal does not explain the prefix:\n%s", arguments, output)
			}
		})
	}
}

func TestNoNameLifecycleCommandsPreserveSandboxOperations(t *testing.T) {
	tests := []struct {
		command string
		want    string
	}{
		{"start", "ensure-guest"},
		{"status", "sandbox-status"},
		{"stop", "stop-guest"},
	}
	for _, testCase := range tests {
		t.Run(testCase.command, func(t *testing.T) {
			var got invocation
			code := run([]string{testCase.command}, 1000, testEnvironment(nil), func() error { return nil }, captureExecutor(&got), discardFile(t), discardFile(t))
			if code != 0 || got.path != controlPath || !reflect.DeepEqual(got.arguments, []string{testCase.want}) {
				t.Fatalf("code=%d invocation=%+v", code, got)
			}
		})
	}
}

// TestPauseAndResumeRequireAnExplicitName pins the pause/resume fix: with
// no operand the CLI used to forward a bare pause-sandbox/resume-sandbox
// that the control plane rejected with its own usage error; chasm must
// refuse locally, naming the command, like every other per-command usage
// error.
func TestPauseAndResumeRequireAnExplicitName(t *testing.T) {
	for _, command := range []string{"pause", "resume"} {
		t.Run(command, func(t *testing.T) {
			output, code := commandStderr(t, []string{command})
			if code != 2 {
				t.Fatalf("%s without NAME code=%d want 2\nstderr:\n%s", command, code, output)
			}
			if !strings.Contains(output, "chasm: "+command+" takes one NAME") ||
				!strings.Contains(output, "Usage:\n") ||
				!strings.Contains(output, "chasm "+command+" NAME") {
				t.Fatalf("%s without NAME must name the command and its usage:\n%s", command, output)
			}
		})
	}
}

func TestShellUsesOneExactPasswordlessHelperCommand(t *testing.T) {
	bindSiteWithDefaultSandbox(t, "demo")
	tests := []struct {
		arguments []string
		name      string
	}{
		{[]string{"shell"}, "demo"},
		{[]string{"shell", "research-2"}, "research-2"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			var got invocation
			code := run(testCase.arguments, 1000, testEnvironment(map[string]string{"TERM": "xterm-256color"}), func() error {
				t.Fatal("unprivileged shell request performed a root host check")
				return nil
			}, captureExecutor(&got), discardFile(t), discardFile(t))
			want := []string{"-n", selfPath, "__shell", testCase.name}
			if code != 0 || got.path != sudoPath || !reflect.DeepEqual(got.arguments, want) {
				t.Fatalf("code=%d invocation=%+v", code, got)
			}
		})
	}
}

func TestMissingDefaultSandboxFailsClosedNamingTheKey(t *testing.T) {
	bindTestSite(t)
	for _, arguments := range [][]string{{"shell"}} {
		t.Run(strings.Join(arguments, "-"), func(t *testing.T) {
			executed := false
			stderr := discardFile(t)
			code := run(arguments, 1000, testEnvironment(nil), func() error { return nil }, func(string, []string, []string) error {
				executed = true
				return nil
			}, discardFile(t), stderr)
			if code == 0 || executed {
				t.Fatalf("code=%d executed=%v", code, executed)
			}
		})
	}
}

func TestPrivilegedShellIsIdentityAndManagedNameBoundAndNeverEntersGuestRoot(t *testing.T) {
	var got invocation
	hostChecks := 0
	values := map[string]string{"SUDO_USER": "chasmops", "TERM": "screen-256color"}
	code := run([]string{"__shell", "research"}, 0, testEnvironment(values), func() error {
		hostChecks++
		return nil
	}, captureExecutor(&got), discardFile(t), discardFile(t))
	joined := strings.Join(got.arguments, " ")
	if code != 0 || hostChecks != 1 || got.path != incusPath {
		t.Fatalf("code=%d host_checks=%d invocation=%+v", code, hostChecks, got)
	}
	for _, required := range []string{"--force-local exec sandbox-research", "--user 1001", "--group 1001", "--cwd /home/agent", "/usr/bin/zsh -l"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("shell invocation lacks %q: %s", required, joined)
		}
	}
	for _, forbidden := range []string{"--force-local exec research", "--user 0", "--group 0", "/bin/sh", "bash"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("shell invocation contains %q: %s", forbidden, joined)
		}
	}
}

func TestPrivilegedShellRefusesWrongCallerHostAndArguments(t *testing.T) {
	cases := []struct {
		name        string
		arguments   []string
		effectiveID int
		environment map[string]string
		hostError   error
	}{
		{"wrong-user", []string{"__shell", "research"}, 0, map[string]string{"SUDO_USER": "other"}, nil},
		{"not-root", []string{"__shell", "research"}, 1000, map[string]string{"SUDO_USER": "chasmops"}, nil},
		{"wrong-host", []string{"__shell", "research"}, 0, map[string]string{"SUDO_USER": "chasmops"}, errors.New("wrong host")},
		{"missing-name", []string{"__shell"}, 0, map[string]string{"SUDO_USER": "chasmops"}, nil},
		{"extra-argument", []string{"__shell", "research", "id"}, 0, map[string]string{"SUDO_USER": "chasmops"}, nil},
		{"unsafe-name", []string{"__shell", "research;id"}, 0, map[string]string{"SUDO_USER": "chasmops"}, nil},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			executed := false
			code := run(testCase.arguments, testCase.effectiveID, testEnvironment(testCase.environment), func() error {
				return testCase.hostError
			}, func(string, []string, []string) error {
				executed = true
				return nil
			}, discardFile(t), discardFile(t))
			if code == 0 || executed {
				t.Fatalf("code=%d executed=%v", code, executed)
			}
		})
	}
}

func TestSandboxNameValidationIsStrict(t *testing.T) {
	valid := []string{"a", "0", "research", "research-2", strings.Repeat("a", maxSandboxNameLen)}
	invalid := []string{"", "-research", "research-", "Research", "research_2", "research.2", "research/id", "research id", "research;id", "r\u00e9search", strings.Repeat("a", maxSandboxNameLen+1)}
	for _, name := range valid {
		if !isSafeName(name) {
			t.Errorf("safe name refused: %q", name)
		}
	}
	for _, name := range invalid {
		if isSafeName(name) {
			t.Errorf("unsafe name admitted: %q", name)
		}
	}
}

func TestRepositoryQueriesAreSafeBasenames(t *testing.T) {
	valid := []string{"demo", "my_repo", "repo.go", "Repo-2"}
	invalid := []string{"", ".", "..", ".hidden", "-repo", "owner/repo", "repo;id", strings.Repeat("a", 129)}
	for _, query := range valid {
		if !isSafeRepositoryQuery(query) {
			t.Errorf("safe repository query refused: %q", query)
		}
	}
	for _, query := range invalid {
		if isSafeRepositoryQuery(query) {
			t.Errorf("unsafe repository query admitted: %q", query)
		}
	}
}

func TestDescriptionCommandsRejectAmbiguousOrUnsafeText(t *testing.T) {
	cases := [][]string{
		{"describe"},
		{"describe", "research", "--set"},
		{"describe", "research", "--set", ""},
		{"describe", "research", "--set", " leading"},
		{"describe", "research", "--set", "line one\nline two"},
		{"describe", "research", "--clear", "extra"},
		{"describe", "research", "text without --set"},
	}
	for _, arguments := range cases {
		executed := false
		code := run(arguments, 1000, testEnvironment(nil), func() error { return nil }, func(string, []string, []string) error {
			executed = true
			return nil
		}, discardFile(t), discardFile(t))
		if code == 0 || executed {
			t.Fatalf("arguments=%q code=%d executed=%v", arguments, code, executed)
		}
	}
}

func TestInvalidCommandsNeverExecute(t *testing.T) {
	cases := [][]string{
		{"create"},
		{"create", "research", "abc"},
		{"create", "research", "2048", "extra"},
		{"list", "research"},
		{"show", "research", "--unknown"},
		{"show", "research", "--layout", "extra"},
		{"inspect"},
		{"describe", "research", "--set", "trailing "},
		{"repos", "research", "extra"},
		{"start", "Research"},
		{"resize", "research"},
		{"resize", "research", "--memory"},
		{"resize", "research", "--force", "4"},
		{"resize", "research", "extra", "--cpu", "4"},
		{"shell", "research;id"},
		{"unknown"},
	}
	for _, arguments := range cases {
		t.Run(strings.Join(arguments, "-"), func(t *testing.T) {
			executed := false
			code := run(arguments, 1000, testEnvironment(nil), func() error { return nil }, func(string, []string, []string) error {
				executed = true
				return nil
			}, discardFile(t), discardFile(t))
			if code == 0 || executed {
				t.Fatalf("code=%d executed=%v", code, executed)
			}
		})
	}
}

func TestPrimaryHelpShowsOnlyCanonicalCommands(t *testing.T) {
	output := commandOutput(t, []string{"help"})
	for _, expected := range []string{"Chasm manages persistent development sandboxes", "Usage:", "create      Create", "show        Show", "repos       List", "shell       Open"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("primary help lacks %q:\n%s", expected, output)
		}
	}
	for _, hidden := range []string{"  chasm list", "  chasm status", "  chasm inspect", "  chasm layout", "  chasm locate", "  sandboxing ..."} {
		if strings.Contains(output, hidden) {
			t.Fatalf("primary help exposes compatibility command %q:\n%s", hidden, output)
		}
	}
}

func TestEveryCanonicalCommandHasFocusedHelp(t *testing.T) {
	tests := []struct {
		topic string
		usage string
	}{
		{"create", "chasm create NAME"},
		{"show", "chasm show NAME --layout"},
		{"describe", "chasm describe NAME --set TEXT"},
		{"repos", "chasm repos REPOSITORY"},
		{"start", "chasm start [NAME]"},
		{"pause", "chasm pause NAME"},
		{"resume", "chasm resume NAME"},
		{"stop", "chasm stop [NAME]"},
		{"delete", "chasm delete NAME"},
		{"shell", "chasm shell [NAME]"},
		{"resize", "chasm resize NAME [--memory SIZE] [--cpu N] [--root-disk SIZE]"},
	}
	for _, testCase := range tests {
		t.Run(testCase.topic, func(t *testing.T) {
			for _, arguments := range [][]string{{testCase.topic, "--help"}, {testCase.topic, "-h"}, {"help", testCase.topic}} {
				output := commandOutput(t, arguments)
				if !strings.Contains(output, "Usage:\n") || !strings.Contains(output, testCase.usage) || !strings.Contains(output, "-h, --help") {
					t.Fatalf("arguments=%q produced incomplete focused help:\n%s", arguments, output)
				}
			}
		})
	}
}

func TestAliasHelpDocumentsCompatibilityWithoutExecuting(t *testing.T) {
	output := commandOutput(t, []string{"help", "aliases"})
	for _, expected := range []string{"Compatibility commands retained for migration", "chasm list", "chasm status [NAME]", "chasm inspect NAME", "chasm layout NAME", "chasm locate REPOSITORY"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("alias help lacks %q:\n%s", expected, output)
		}
	}
	if strings.Contains(output, "sandboxing ...") {
		t.Fatalf("alias help still documents the sandboxing alias:\n%s", output)
	}
}

func commandOutput(t *testing.T, arguments []string) string {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "chasm-output-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	executed := false
	code := run(arguments, 1000, testEnvironment(nil), func() error { return nil }, func(string, []string, []string) error {
		executed = true
		return nil
	}, file, discardFile(t))
	if code != 0 || executed {
		t.Fatalf("code=%d executed=%v", code, executed)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	output, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(output)
}

func TestRootCannotBypassControlPlane(t *testing.T) {
	executed := false
	code := run([]string{"list"}, 0, testEnvironment(nil), func() error { return nil }, func(string, []string, []string) error {
		executed = true
		return nil
	}, discardFile(t), discardFile(t))
	if code == 0 || executed {
		t.Fatalf("code=%d executed=%v", code, executed)
	}
}

func TestUnsafeTerminalValueFallsBackInsideGuest(t *testing.T) {
	if got := safeTerm("xterm;id"); got != "" {
		t.Fatalf("unsafe terminal admitted: %q", got)
	}
	joined := strings.Join(shellArguments("sandbox-research", safeTerm("xterm;id"), 1001), " ")
	if !strings.Contains(joined, "TERM=xterm-256color") || strings.Contains(joined, ";id") {
		t.Fatalf("unsafe terminal reached invocation: %s", joined)
	}
}
