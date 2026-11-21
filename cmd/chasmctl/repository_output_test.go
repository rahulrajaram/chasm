package main

import (
	"bytes"
	"strings"
	"testing"
)

func incompleteRepositoryFixture() map[string]interface{} {
	return map[string]interface{}{
		"complete":     false,
		"repositories": []interface{}{},
		"sandboxes": []interface{}{
			map[string]interface{}{"instance": "sandbox-archive", "logical_name": "sandbox-archive", "freshness": "unobserved", "observed_at": ""},
		},
	}
}

func completeRepositoryFixture() map[string]interface{} {
	return map[string]interface{}{
		"complete": true,
		"repositories": []interface{}{
			map[string]interface{}{
				"name": "example-app", "instance": "sandbox-research", "logical_name": "research", "path": "/workspace/example-app",
				"freshness": "live", "observed_at": "2026-09-02T23:00:00Z",
			},
			map[string]interface{}{
				"name": "demo", "instance": "sandbox-tools", "logical_name": "tools", "path": "/workspace/projects/demo",
				"freshness": "last_observed", "observed_at": "2026-09-02T22:00:00Z",
			},
		},
		"sandboxes": []interface{}{
			map[string]interface{}{"instance": "sandbox-research", "logical_name": "research", "freshness": "live", "observed_at": "2026-09-02T23:00:00Z"},
			map[string]interface{}{"instance": "sandbox-tools", "logical_name": "tools", "freshness": "last_observed", "observed_at": "2026-09-02T22:00:00Z"},
		},
	}
}

func TestHumanRepositoryInventoryExplainsUnobservedSandboxes(t *testing.T) {
	var output bytes.Buffer
	handled, err := renderHumanResult("list-repositories", incompleteRepositoryFixture(), &output, humanOutputStyle{Width: 100})
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	text := output.String()
	for _, expected := range []string{
		"No repositories observed.", "Inventory incomplete", "sandbox-archive — repository contents unknown",
		"0 repositories · 1 sandbox · inventory incomplete",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("output lacks %q:\n%s", expected, text)
		}
	}
	if strings.HasPrefix(text, "{") || strings.Contains(text, `"complete"`) {
		t.Fatalf("human output leaked JSON:\n%s", text)
	}
}

func TestHumanRepositoryInventoryRendersWideTable(t *testing.T) {
	var output bytes.Buffer
	handled, err := renderHumanResult("list-repositories", completeRepositoryFixture(), &output, humanOutputStyle{Width: 100})
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	text := output.String()
	for _, expected := range []string{
		"REPOSITORY", "SANDBOX", "FRESHNESS", "PATH", "example-app", "research", "live", "/workspace/example-app",
		"demo", "tools", "last observed", "/workspace/projects/demo", "2 repositories · 2 sandboxes · inventory complete",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("output lacks %q:\n%s", expected, text)
		}
	}
}

func TestHumanRepositoryInventoryKeepsPathsInNarrowLayout(t *testing.T) {
	var output bytes.Buffer
	renderRepositoryInventory(&output, mustParseRepositoryFixture(t, completeRepositoryFixture()), "", humanOutputStyle{Width: 60})
	text := output.String()
	if strings.Contains(strings.SplitN(text, "\n", 2)[0], "PATH") || !strings.Contains(text, "  /workspace/example-app\n") {
		t.Fatalf("unexpected narrow output:\n%s", text)
	}
}

func TestHumanRepositoryLocationDistinguishesNoMatchFromCompleteKnowledge(t *testing.T) {
	fixture := completeRepositoryFixture()
	fixture["repository"] = "missing"
	fixture["matches"] = []interface{}{}
	delete(fixture, "repositories")
	var output bytes.Buffer
	handled, err := renderHumanResult("locate-repository", fixture, &output, humanOutputStyle{Width: 100})
	if err != nil || !handled || !strings.Contains(output.String(), `No observed repository named "missing".`) ||
		!strings.Contains(output.String(), "inventory complete") {
		t.Fatalf("handled=%v err=%v output=%q", handled, err, output.String())
	}
}

func TestHumanRepositoryInventoryRejectsMalformedOrInconsistentResponses(t *testing.T) {
	tests := []map[string]interface{}{
		{"complete": true, "repositories": []interface{}{}, "sandboxes": []interface{}{map[string]interface{}{"logical_name": "sandbox-archive", "freshness": "unobserved", "observed_at": ""}}},
		{"complete": true, "repositories": []interface{}{map[string]interface{}{"name": "demo", "logical_name": "tools", "path": "/tmp/demo", "freshness": "live", "observed_at": "2026-09-02T23:00:00Z"}}, "sandboxes": []interface{}{}},
		{"complete": true, "repositories": "wrong", "sandboxes": []interface{}{}},
	}
	for _, result := range tests {
		var output bytes.Buffer
		handled, err := renderHumanResult("list-repositories", result, &output, humanOutputStyle{Width: 100})
		if !handled || err == nil || output.Len() != 0 {
			t.Fatalf("handled=%v err=%v output=%q", handled, err, output.String())
		}
	}
}

func TestRepositoryHumanModeIsOptInAndLowLevelDefaultStaysJSON(t *testing.T) {
	transport := supervisorTransportFunc(func(string, map[string]interface{}) (map[string]interface{}, error) {
		return incompleteRepositoryFixture(), nil
	})
	t.Setenv("SANDBOXING_OUTPUT", "")
	var raw bytes.Buffer
	if code := writeRemoteResult(transport, "list-repositories", "list_repositories", nil, &raw); code != 0 || !strings.HasPrefix(raw.String(), "{") {
		t.Fatalf("code=%d raw=%q", code, raw.String())
	}
	t.Setenv("SANDBOXING_OUTPUT", "human")
	var human bytes.Buffer
	if code := writeRemoteResult(transport, "list-repositories", "list_repositories", nil, &human); code != 0 || strings.HasPrefix(human.String(), "{") {
		t.Fatalf("code=%d human=%q", code, human.String())
	}
}

func mustParseRepositoryFixture(t *testing.T, result map[string]interface{}) humanRepositoryInventory {
	t.Helper()
	inventory, err := parseHumanRepositoryInventory(result, "repositories")
	if err != nil {
		t.Fatal(err)
	}
	return inventory
}
