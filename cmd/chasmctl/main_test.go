package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

func TestMain(m *testing.M) {
	path, cleanup, err := testsupport.TempSiteConfig(filepath.Join(moduleRoot(), "testdata", "fixtures", "testsite", "site.conf"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "copy test site: %v\n", err)
		os.Exit(1)
	}
	if err := siteconfig.Init(path); err != nil {
		fmt.Fprintf(os.Stderr, "bind test site: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func TestLocalLaunchAdmissionRequiresRootAndOneValidInstance(t *testing.T) {
	for _, testCase := range []struct {
		name string
		args []string
		euid int
	}{
		{name: "unprivileged", args: []string{"sandbox-demo"}, euid: 1000},
		{name: "missing instance", euid: 0},
		{name: "extra argument", args: []string{"sandbox-demo", "sandbox-other"}, euid: 0},
		{name: "unmanaged name", args: []string{"template-builder"}, euid: 0},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			called := false
			var stderr bytes.Buffer
			code := runLocalLaunchAdmission(testCase.args, testCase.euid, func(string) error {
				called = true
				return nil
			}, &stderr)
			if code != 2 || called || !strings.Contains(stderr.String(), "requires root") {
				t.Fatalf("code=%d called=%v stderr=%q", code, called, stderr.String())
			}
		})
	}
}

func TestLocalLaunchAdmissionPropagatesBudgetRefusal(t *testing.T) {
	var stderr bytes.Buffer
	code := runLocalLaunchAdmission([]string{"sandbox-demo"}, 0, func(instance string) error {
		if instance != "sandbox-demo" {
			t.Fatalf("wrong instance %q", instance)
		}
		return fmt.Errorf("host memory headroom exhausted")
	}, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "host memory headroom exhausted") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	stderr.Reset()
	if code := runLocalLaunchAdmission([]string{"sandbox-demo"}, 0, func(string) error { return nil }, &stderr); code != 0 {
		t.Fatalf("admitted instance failed: code=%d stderr=%q", code, stderr.String())
	}
}

func TestCLIRejectsUnsupportedCommand(t *testing.T) {
	out, code := invoke("reconcile", "--run-id", "run-1")
	if code != 2 {
		t.Fatalf("unsupported command code=%d out=%s", code, out)
	}
	var unsupported commandError
	decode(t, out, &unsupported)
	if unsupported.Code != "usage" || unsupported.OK {
		t.Fatalf("unexpected unsupported output: %s", out)
	}
}

func TestSessionCommandsAreRefusedAsUnknown(t *testing.T) {
	for _, command := range []string{
		"start-session", "resume-session", "register-receipt", "stage-artifact",
		"run-status", "fetch-artifact", "run",
	} {
		t.Run(command, func(t *testing.T) {
			out, code := invoke(command)
			if code != 2 {
				t.Fatalf("removed command %s must be a usage error, got %d: %s", command, code, out)
			}
			var failure commandError
			decode(t, out, &failure)
			if failure.Code != "usage" {
				t.Fatalf("removed command %s must fail as usage, got %s", command, out)
			}
		})
	}
}

func TestCommandWithoutDefaultSocketRequiresSupervisorSocket(t *testing.T) {
	out, code := invoke("definitely-not-a-command")
	if code != 2 {
		t.Fatalf("unknown command code=%d out=%s", code, out)
	}
	var failure commandError
	decode(t, out, &failure)
	if failure.Code != "usage" {
		t.Fatalf("unknown command must fail as usage: %s", out)
	}
}

func TestHelpWorksBeforeTheSiteDescriptorIsRead(t *testing.T) {
	// A descriptor with an unknown key must not break help: runCLI
	// dispatches help invocations before it reads the site descriptor.
	dir := t.TempDir()
	path := filepath.Join(dir, "site.conf")
	if err := os.WriteFile(path, []byte("unknown_key=yes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CHASM_SITE_CONF", path)
	for _, argument := range []string{"help", "--help", "-h"} {
		var stdout bytes.Buffer
		code := runCLI([]string{argument}, &stdout, io.Discard)
		if code != 0 || !strings.Contains(stdout.String(), "chasmctl") {
			t.Fatalf("argument=%q code=%d stdout=%q", argument, code, stdout.String())
		}
	}
	for _, args := range [][]string{{"help", "list", "extra"}, {"--help", "extra"}, {"-h", "extra"}} {
		var stdout bytes.Buffer
		if code := runCLI(args, &stdout, io.Discard); code != 2 {
			t.Fatalf("args=%q exit=%d stdout=%q, want usage status 2", args, code, stdout.String())
		}
		var failure commandError
		decode(t, stdout.String(), &failure)
		if failure.Code != "usage" {
			t.Fatalf("args=%q error code=%q, want usage", args, failure.Code)
		}
	}

	// Without help, the unreadable descriptor is still the hard failure it
	// must be.
	var stderr bytes.Buffer
	if code := runCLI([]string{"sandbox-status"}, io.Discard, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "unknown_key") {
		t.Fatalf("non-help run must fail on the invalid descriptor: code=%d stderr=%q", code, stderr.String())
	}
}

func TestHelpWriteFailureIsAnError(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"help", "sandbox-status"}} {
		var stderr bytes.Buffer
		if code := runCLI(args, closedOutput{}, &stderr); code != 1 {
			t.Fatalf("args=%q closed output exit=%d, want 1", args, code)
		}
		if !strings.Contains(stderr.String(), "writing help") {
			t.Fatalf("missing write failure diagnostic: %q", stderr.String())
		}
	}
}

func invoke(args ...string) (string, int) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return stdout.String(), code
}

func decode(t *testing.T, raw string, target interface{}) {
	t.Helper()
	if err := json.Unmarshal([]byte(raw), target); err != nil {
		t.Fatalf("invalid JSON %q: %v", raw, err)
	}
}

// TestCLIHelpListsEveryCommand pins the help surface: bare help and --help
// exit 0 with human-readable text listing every dispatch case name, and an
// unknown help topic stays a usage error.
func TestCLIHelpListsEveryCommand(t *testing.T) {
	var stdout bytes.Buffer
	if code := runWithTransportFactory([]string{"help"}, &stdout, io.Discard, nil); code != 0 {
		t.Fatalf("help exit code = %d", code)
	}
	helpText := stdout.String()
	if strings.Contains(helpText, `"ok"`) {
		t.Fatalf("help must not emit a JSON error object: %q", helpText)
	}
	for _, item := range supervisorCommandSynopses {
		if !strings.Contains(helpText, item.name) {
			t.Errorf("help does not list command %q", item.name)
		}
	}

	stdout.Reset()
	if code := runWithTransportFactory([]string{"--help"}, &stdout, io.Discard, nil); code != 0 {
		t.Fatalf("--help exit code = %d", code)
	}

	stdout.Reset()
	if code := runWithTransportFactory([]string{"help", "sandbox-status"}, &stdout, io.Discard, nil); code != 0 {
		t.Fatalf("help sandbox-status exit code = %d", code)
	}
	if !strings.Contains(stdout.String(), "sandbox-status") {
		t.Fatalf("help sandbox-status missing synopsis: %q", stdout.String())
	}

	stdout.Reset()
	if code := runWithTransportFactory([]string{"help", "definitely-not-a-command"}, &stdout, io.Discard, nil); code != 2 {
		t.Fatalf("unknown help topic exit code = %d", code)
	}
}
