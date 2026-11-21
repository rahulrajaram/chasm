package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestEscapeHumanText(t *testing.T) {
	tests := []struct {
		name, input, want string
	}{
		{"ordinary Unicode", "café/資料", "café/資料"},
		{"backslash", `a\b`, `a\\b`},
		{"common controls", "a\n\t\r", `a\n\t\r`},
		{"remaining C0 and DEL", "\x00\x1b\x1f\x7f", `\x00\x1b\x1f\x7f`},
		{"C1 and separators", "\u0080\u0085\u009f\u2028\u2029", `\u0080\u0085\u009f\u2028\u2029`},
		{"invalid lone byte", string([]byte{0x9b}), `\x9b`},
		{"invalid bytes and replacement rune", string([]byte{'a', 0xff, 0x9b}) + "\ufffd", `a\xff\x9b` + "\ufffd"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			if got := escapeHumanText(testCase.input); got != testCase.want {
				t.Fatalf("escapeHumanText(%q) = %q, want %q", testCase.input, got, testCase.want)
			}
		})
	}
	if got := truncateCell("a\x1b[2J", 4); got != "a…" {
		t.Fatalf("truncation split an escape: %q", got)
	}
	if got := truncateCell(string([]byte{'a', 0x9b, 'b'}), 10); got != `a\x9bb` {
		t.Fatalf("truncation lost invalid UTF-8: %q", got)
	}
}

func TestSandboxDescriptionsKeepBackslashesInHumanViews(t *testing.T) {
	const description = `C:\tmp`
	t.Run("list", func(t *testing.T) {
		var output bytes.Buffer
		result := map[string]interface{}{"sandboxes": []interface{}{map[string]interface{}{
			"logical_name": "demo", "instance_status": "Stopped", "description": description,
		}}}
		if handled, err := renderHumanResult("list-sandboxes", result, &output, humanOutputStyle{Width: 100}); err != nil || !handled {
			t.Fatalf("handled=%v err=%v", handled, err)
		}
		if !strings.Contains(output.String(), description) || strings.Contains(output.String(), `C:\\tmp`) {
			t.Fatalf("description changed: %q", output.String())
		}
	})
	t.Run("inspect", func(t *testing.T) {
		var output bytes.Buffer
		result := map[string]interface{}{
			"logical_name": "demo", "instance_status": "Stopped", "instance_pid": float64(0),
			"description": description, "workspace": "/workspace", "shared": "/shared",
		}
		if handled, err := renderHumanResult("inspect-sandbox", result, &output, humanOutputStyle{Width: 100}); err != nil || !handled {
			t.Fatalf("handled=%v err=%v", handled, err)
		}
		if !strings.Contains(output.String(), description) || strings.Contains(output.String(), `C:\\tmp`) {
			t.Fatalf("description changed: %q", output.String())
		}
	})
	t.Run("describe", func(t *testing.T) {
		var output bytes.Buffer
		result := map[string]interface{}{"logical_name": "demo", "description": description}
		if handled, err := renderHumanResult("describe-sandbox", result, &output, humanOutputStyle{Width: 100}); err != nil || !handled {
			t.Fatalf("handled=%v err=%v", handled, err)
		}
		if got := output.String(); got != "demo — "+description+"\n" {
			t.Fatalf("description changed: %q", got)
		}
	})
	if got := truncateDescription(`a\bcd`, 4); got != `a\b…` {
		t.Fatalf("description truncation changed: %q", got)
	}
}

func TestGuestPathsRenderWithoutRawControlsOrExtraLines(t *testing.T) {
	path := "/workspace/one\n\x1b[2J\u0085"
	cases := []struct {
		name      string
		render    func(*bytes.Buffer)
		wantLines int
	}{
		{"layout", func(output *bytes.Buffer) {
			renderSandboxLayout(output, humanSandboxLayout{
				Name: "demo", Root: "/workspace", Depth: 1, Freshness: "live", Complete: true,
				Entries: []humanLayoutEntry{{Type: "file", Path: path}},
			})
		}, 9},
		{"repository", func(output *bytes.Buffer) {
			renderRepositoryInventory(output, humanRepositoryInventory{
				Repositories: []humanRepository{{Name: "one\n\x1b[2J\u0085", Sandbox: "demo", Path: path, Freshness: "live"}},
				Sandboxes:    []humanSandboxObservation{{Name: "demo", Freshness: "live"}}, Complete: true,
			}, "", humanOutputStyle{Width: 500})
		}, 4},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var output bytes.Buffer
			testCase.render(&output)
			got := output.String()
			if lines := strings.Count(got, "\n"); lines != testCase.wantLines {
				t.Fatalf("line count = %d, want %d: %q", lines, testCase.wantLines, got)
			}
			for _, character := range got {
				if character != '\n' && (character <= 0x1f || character == 0x7f || character >= 0x80 && character <= 0x9f || character == 0x2028 || character == 0x2029) {
					t.Fatalf("raw control %U in output: %q", character, got)
				}
			}
			if !strings.Contains(got, `\n\x1b[2J\u0085`) {
				t.Fatalf("guest controls were not visibly escaped: %q", got)
			}
		})
	}
}

func sandboxListFixture() map[string]interface{} {
	return map[string]interface{}{
		"sandboxes": []interface{}{
			map[string]interface{}{
				"instance": "sandbox-demo", "logical_name": "demo", "instance_status": "Running", "description": "General agent work",
			},
			map[string]interface{}{
				"instance": "sandbox-research", "logical_name": "research", "instance_status": "Stopped",
				"workspace": "/workspace", "shared": "/shared", "description": "LLM evaluation and tooling",
			},
			map[string]interface{}{
				"instance": "sandbox-hold", "logical_name": "hold", "instance_status": "Frozen",
				"description": "Paused mid-session",
			},
		},
	}
}

func TestHumanSandboxListRendersCalmWideTable(t *testing.T) {
	var output bytes.Buffer
	handled, err := renderHumanResult("list-sandboxes", sandboxListFixture(), &output, humanOutputStyle{Width: 100})
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	text := output.String()
	for _, expected := range []string{
		"SANDBOX", "STATE", "MODE", "DESCRIPTION",
		"demo", "● running", "General agent work",
		"research", "○ stopped", "managed", "LLM evaluation and tooling",
		"hold", "◐ paused", "Paused mid-session",
		"3 sandboxes · 1 running",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("output lacks %q:\n%s", expected, text)
		}
	}
	if strings.Contains(text, "\x1b[") {
		t.Fatalf("non-color rendering contains ANSI escapes: %q", text)
	}
	if strings.Contains(text, "WORKSPACE") || strings.Contains(text, "SHARED") || strings.Contains(text, "/workspace") || strings.Contains(text, "/shared") {
		t.Fatalf("invariant mount paths leaked into list output:\n%s", text)
	}
}

func TestHumanSandboxListUsesLowDensityNarrowFallback(t *testing.T) {
	var output bytes.Buffer
	renderSandboxList(&output, []humanSandbox{{Name: "research", State: "running", Mode: "managed", Description: "A deliberately long description"}}, humanOutputStyle{Width: 60})
	text := output.String()
	if !strings.Contains(text, "SANDBOX") || !strings.Contains(text, "STATE") || !strings.Contains(text, "MODE") || strings.Contains(text, "DESCRIPTION") {
		t.Fatalf("unexpected narrow output:\n%s", text)
	}
	if !strings.Contains(text, "1 sandbox · 1 running") {
		t.Fatalf("narrow summary is missing:\n%s", text)
	}
}

func TestHumanDescriptionCommandsRenderConciseOutcomes(t *testing.T) {
	tests := []struct {
		command string
		result  map[string]interface{}
		want    string
	}{
		{"describe-sandbox", map[string]interface{}{"logical_name": "research", "description": "LLM evaluation"}, "research — LLM evaluation\n"},
		{"describe-sandbox", map[string]interface{}{"logical_name": "research", "description": ""}, "research — No description\n"},
		{"set-sandbox-description", map[string]interface{}{"logical_name": "research", "description": "LLM evaluation", "changed": true}, "Updated research description.\n"},
		{"set-sandbox-description", map[string]interface{}{"logical_name": "research", "description": "", "changed": true}, "Cleared research description.\n"},
		{"set-sandbox-description", map[string]interface{}{"logical_name": "research", "description": "LLM evaluation", "changed": false}, "research description is unchanged.\n"},
	}
	for _, testCase := range tests {
		var output bytes.Buffer
		handled, err := renderHumanResult(testCase.command, testCase.result, &output, humanOutputStyle{Width: 100})
		if err != nil || !handled || output.String() != testCase.want {
			t.Fatalf("command=%s handled=%v output=%q err=%v", testCase.command, handled, output.String(), err)
		}
	}
}

func TestHumanDescriptionRejectsMalformedControlPlaneResponse(t *testing.T) {
	var output bytes.Buffer
	handled, err := renderHumanResult("describe-sandbox", map[string]interface{}{
		"logical_name": "research", "description": "line one\nline two",
	}, &output, humanOutputStyle{Width: 100})
	if !handled || err == nil || output.Len() != 0 {
		t.Fatalf("handled=%v output=%q err=%v", handled, output.String(), err)
	}
}

func TestHumanSandboxListHasExplicitEmptyState(t *testing.T) {
	var output bytes.Buffer
	renderSandboxList(&output, nil, humanOutputStyle{Width: 100})
	if got := output.String(); got != "No sandboxes found.\n\n0 sandboxes · 0 running\n" {
		t.Fatalf("empty output=%q", got)
	}
}

func TestHumanSandboxListColorNeverCarriesStateByItself(t *testing.T) {
	for _, state := range []struct {
		token string
		label string
	}{{"running", "running"}, {"stopped", "stopped"}, {"frozen", "paused"}} {
		colored := styledState(state.token, 10, true)
		if !strings.Contains(colored, "\x1b[") || !strings.Contains(colored, state.label) {
			t.Fatalf("colored state lacks escape or text: %q", colored)
		}
	}
}

func TestStyledStateRendersRunningStoppedAndFrozen(t *testing.T) {
	tests := []struct {
		state string
		plain string
		code  string
	}{
		{"running", "● running", "\x1b[32m"},
		{"stopped", "○ stopped", "\x1b[2m"},
		// An Incus FROZEN instance is paused (resumable in place), never
		// rendered as stopped.
		{"frozen", "◐ paused", "\x1b[33m"},
	}
	for _, testCase := range tests {
		if got := styledState(testCase.state, 0, false); got != testCase.plain {
			t.Fatalf("styledState(%q) plain = %q, want %q", testCase.state, got, testCase.plain)
		}
		colored := styledState(testCase.state, 0, true)
		if want := testCase.code + testCase.plain + "\x1b[0m"; colored != want {
			t.Fatalf("styledState(%q) colored = %q, want %q", testCase.state, colored, want)
		}
	}
}

func TestHumanSandboxListRejectsMalformedControlPlaneResponse(t *testing.T) {
	malformed := map[string]interface{}{"sandboxes": []interface{}{map[string]interface{}{"logical_name": "research", "instance_status": "Unknown"}}}
	var output bytes.Buffer
	handled, err := renderHumanResult("list-sandboxes", malformed, &output, humanOutputStyle{Width: 100})
	if !handled || err == nil || output.Len() != 0 {
		t.Fatalf("handled=%v err=%v output=%q", handled, err, output.String())
	}
}

func TestHumanListModeIsOptInAndLowLevelDefaultStaysJSON(t *testing.T) {
	t.Setenv("SANDBOXING_OUTPUT", "")
	var raw bytes.Buffer
	transport := supervisorTransportFunc(func(string, map[string]interface{}) (map[string]interface{}, error) {
		return sandboxListFixture(), nil
	})
	if code := writeRemoteResult(transport, "list-sandboxes", "list_sandboxes", nil, &raw); code != 0 || !strings.HasPrefix(raw.String(), "{") {
		t.Fatalf("code=%d raw=%q", code, raw.String())
	}

	t.Setenv("SANDBOXING_OUTPUT", "human")
	var human bytes.Buffer
	if code := writeRemoteResult(transport, "list-sandboxes", "list_sandboxes", nil, &human); code != 0 || !strings.Contains(human.String(), "3 sandboxes · 1 running") || strings.HasPrefix(human.String(), "{") {
		t.Fatalf("code=%d human=%q", code, human.String())
	}
}

func TestHumanResizeResultRendersBeforeAndAfterPerResource(t *testing.T) {
	result := map[string]interface{}{
		"instance": "sandbox-research",
		"resized": []interface{}{
			map[string]interface{}{"resource": "memory", "before": jsonNumber("3072"), "after": jsonNumber("8192"), "unit": "MiB"},
			map[string]interface{}{"resource": "cpu", "before": jsonNumber("2"), "after": jsonNumber("4"), "unit": "count"},
			map[string]interface{}{"resource": "root_disk", "before": jsonNumber("61440"), "after": jsonNumber("102400"), "unit": "MiB"},
		},
	}
	var output bytes.Buffer
	handled, err := renderHumanResult("resize-sandbox", result, &output, humanOutputStyle{Width: 100})
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	text := output.String()
	for _, expected := range []string{"Resized sandbox-research:", "memory: 3072 MiB -> 8192 MiB", "CPU: 2 -> 4", "root disk: 61440 MiB -> 102400 MiB"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("resize output lacks %q:\n%s", expected, text)
		}
	}
	// A CPU count is never labeled MiB. Mutation checked: rendering the
	// count unit with the MiB format fails this negative assertion.
	if strings.Contains(text, "CPU: 2 MiB") {
		t.Fatalf("a CPU count must not be labeled MiB:\n%s", text)
	}
}

// TestHumanResizeResultRendersUnknownBefore pins the explicit unknown: an
// unobservable before value renders as "unknown" with its reason instead of
// being dropped from the line.
func TestHumanResizeResultRendersUnknownBefore(t *testing.T) {
	result := map[string]interface{}{
		"instance": "sandbox-research",
		"resized": []interface{}{
			map[string]interface{}{"resource": "memory", "before_unknown": "limits.memory '50%' is not a supported size", "after": jsonNumber("4096"), "unit": "MiB"},
		},
	}
	var output bytes.Buffer
	handled, err := renderHumanResult("resize-sandbox", result, &output, humanOutputStyle{Width: 100})
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if !strings.Contains(output.String(), "memory: unknown (limits.memory '50%' is not a supported size) -> 4096 MiB") {
		t.Fatalf("the unknown before value must render with its reason:\n%s", output.String())
	}
}

func jsonNumber(value string) json.Number {
	return json.Number(value)
}
