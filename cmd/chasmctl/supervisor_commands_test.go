package main

import (
	"bytes"
	"io"
	"reflect"
	"strings"
	"testing"
)

type supervisorTransportFunc func(string, map[string]interface{}) (map[string]interface{}, error)

func (function supervisorTransportFunc) call(operation string, fields map[string]interface{}) (map[string]interface{}, error) {
	return function(operation, fields)
}

func TestClosedOperationalCommandsDefaultToProductionSocket(t *testing.T) {
	cases := []struct {
		args      []string
		operation string
	}{
		{[]string{"sandbox-status"}, "sandbox_status"},
		{[]string{"ensure-guest"}, "ensure_guest"},
		{[]string{"stop-guest"}, "stop_guest"},
		{[]string{"collect-performance", "startup"}, "collect_performance"},
	}
	for _, testCase := range cases {
		t.Run(testCase.operation, func(t *testing.T) {
			selected := ""
			observed := ""
			transport := supervisorTransportFunc(func(operation string, fields map[string]interface{}) (map[string]interface{}, error) {
				observed = operation
				if operation == "collect_performance" && fields["phase"] != "startup" {
					t.Fatalf("phase was not preserved: %v", fields)
				}
				return map[string]interface{}{"instance": "sandbox-demo"}, nil
			})
			var stdout bytes.Buffer
			code := runWithTransportFactory(testCase.args, &stdout, io.Discard, func(path string) supervisorTransport {
				selected = path
				return transport
			})
			if code != 0 || selected != "/run/chasm-supervisor.sock" || observed != testCase.operation {
				t.Fatalf("command mismatch: code=%d socket=%q operation=%q output=%s", code, selected, observed, stdout.String())
			}
		})
	}
}

func TestNamedSandboxCommandsUseOnlyTheSupervisorSocket(t *testing.T) {
	tests := []struct {
		arguments []string
		operation string
		fields    map[string]interface{}
	}{
		{[]string{"create-sandbox", "sandbox-research"}, "create_sandbox", map[string]interface{}{"instance": "sandbox-research"}},
		{[]string{"create-sandbox", "sandbox-research", "2048"}, "create_sandbox", map[string]interface{}{"instance": "sandbox-research", "budget_memory_mib": int64(2048)}},
		{[]string{"resize-sandbox", "sandbox-research", "--memory", "8GiB"}, "resize_sandbox", map[string]interface{}{"instance": "sandbox-research", "budget_memory_mib": int64(8192)}},
		{[]string{"resize-sandbox", "sandbox-research", "--memory", "2048MiB", "--cpu", "4", "--root-disk", "100GiB"}, "resize_sandbox", map[string]interface{}{"instance": "sandbox-research", "budget_memory_mib": int64(2048), "budget_cpu": int64(4), "root_disk_mib": int64(102400)}},
		{[]string{"resize-sandbox", "sandbox-research", "--memory", "4096"}, "resize_sandbox", map[string]interface{}{"instance": "sandbox-research", "budget_memory_mib": int64(4096)}},
		{[]string{"describe-sandbox", "sandbox-research"}, "describe_sandbox", map[string]interface{}{"instance": "sandbox-research"}},
		{[]string{"set-sandbox-description", "sandbox-research", "LLM evaluation"}, "set_sandbox_description", map[string]interface{}{"instance": "sandbox-research", "description": "LLM evaluation"}},
		{[]string{"set-sandbox-description", "sandbox-research", ""}, "set_sandbox_description", map[string]interface{}{"instance": "sandbox-research", "description": ""}},
		{[]string{"list-sandboxes"}, "list_sandboxes", map[string]interface{}{}},
		{[]string{"inspect-sandbox", "sandbox-research"}, "inspect_sandbox", map[string]interface{}{"instance": "sandbox-research"}},
		{[]string{"start-sandbox", "sandbox-research"}, "start_sandbox", map[string]interface{}{"instance": "sandbox-research"}},
		{[]string{"admit-launch", "sandbox-research"}, "admit_launch", map[string]interface{}{"instance": "sandbox-research"}},
		{[]string{"stop-sandbox", "sandbox-research"}, "stop_sandbox", map[string]interface{}{"instance": "sandbox-research"}},
		{[]string{"pause-sandbox", "sandbox-research"}, "pause_sandbox", map[string]interface{}{"instance": "sandbox-research"}},
		{[]string{"resume-sandbox", "sandbox-research"}, "resume_sandbox", map[string]interface{}{"instance": "sandbox-research"}},
		{[]string{"delete-sandbox", "sandbox-perf-cycle-1"}, "delete_sandbox", map[string]interface{}{"instance": "sandbox-perf-cycle-1"}},
		{[]string{"sandbox-layout", "sandbox-research", "3"}, "sandbox_layout", map[string]interface{}{"instance": "sandbox-research", "depth": 3}},
		{[]string{"list-repositories"}, "list_repositories", map[string]interface{}{}},
		{[]string{"locate-repository", "demo"}, "locate_repository", map[string]interface{}{"repository": "demo"}},
	}
	for _, testCase := range tests {
		t.Run(testCase.operation, func(t *testing.T) {
			selected := ""
			observedOperation := ""
			observedFields := map[string]interface{}{}
			transport := supervisorTransportFunc(func(operation string, fields map[string]interface{}) (map[string]interface{}, error) {
				observedOperation = operation
				observedFields = cloneFields(fields)
				return map[string]interface{}{"ok": true}, nil
			})
			var stdout bytes.Buffer
			code := runWithTransportFactory(testCase.arguments, &stdout, io.Discard, func(path string) supervisorTransport {
				selected = path
				return transport
			})
			if code != 0 || selected != "/run/chasm-supervisor.sock" || observedOperation != testCase.operation || !reflect.DeepEqual(observedFields, testCase.fields) {
				t.Fatalf("code=%d socket=%q operation=%q fields=%v output=%s", code, selected, observedOperation, observedFields, stdout.String())
			}
		})
	}
}

func TestSandboxLayoutRefusesUnboundedDepthBeforeTransport(t *testing.T) {
	called := false
	var stdout bytes.Buffer
	code := runWithTransportFactory([]string{"sandbox-layout", "sandbox-research", "6"}, &stdout, io.Discard, func(string) supervisorTransport {
		return supervisorTransportFunc(func(string, map[string]interface{}) (map[string]interface{}, error) {
			called = true
			return nil, nil
		})
	})
	if code != 2 || called {
		t.Fatalf("code=%d called=%v output=%s", code, called, stdout.String())
	}
}

func TestDeleteSandboxRefusesANonManagedInstanceBeforeTransport(t *testing.T) {
	// Deletion is destructive, so an operand that is not a canonical managed
	// instance name is refused locally and never reaches the executor.
	for _, operand := range []string{"chatty", "sandbox-", "sandbox-Chatty", "Template-Build"} {
		t.Run(operand, func(t *testing.T) {
			called := false
			var stdout bytes.Buffer
			code := runWithTransportFactory([]string{"delete-sandbox", operand}, &stdout, io.Discard, func(string) supervisorTransport {
				return supervisorTransportFunc(func(string, map[string]interface{}) (map[string]interface{}, error) {
					called = true
					return nil, nil
				})
			})
			if code != 2 || called {
				t.Fatalf("operand %q: code=%d called=%v output=%s", operand, code, called, stdout.String())
			}
			if !strings.Contains(stdout.String(), "invalid_instance") {
				t.Fatalf("operand %q: refusal does not name invalid_instance: %s", operand, stdout.String())
			}
		})
	}

	called := false
	var stdout bytes.Buffer
	code := runWithTransportFactory([]string{"delete-sandbox", "sandbox-research"}, &stdout, io.Discard, func(string) supervisorTransport {
		return supervisorTransportFunc(func(string, map[string]interface{}) (map[string]interface{}, error) {
			called = true
			return map[string]interface{}{"instance": "sandbox-research", "already_deleted": false}, nil
		})
	})
	if code != 0 || !called {
		t.Fatalf("a managed instance must reach the executor: code=%d called=%v output=%s", code, called, stdout.String())
	}
}

func cloneFields(fields map[string]interface{}) map[string]interface{} {
	cloned := make(map[string]interface{}, len(fields))
	for key, value := range fields {
		cloned[key] = value
	}
	return cloned
}

func TestCreateSandboxForwardsTheCpuBudget(t *testing.T) {
	tests := []struct {
		arguments []string
		fields    map[string]interface{}
	}{
		{[]string{"create-sandbox", "sandbox-research", "--cpu", "4"}, map[string]interface{}{"instance": "sandbox-research", "budget_cpu": int64(4)}},
		{[]string{"create-sandbox", "sandbox-research", "2048", "--cpu", "2"}, map[string]interface{}{"instance": "sandbox-research", "budget_memory_mib": int64(2048), "budget_cpu": int64(2)}},
	}
	for _, testCase := range tests {
		var stdout bytes.Buffer
		code := runWithTransportFactory(testCase.arguments, &stdout, io.Discard, func(string) supervisorTransport {
			return supervisorTransportFunc(func(operation string, fields map[string]interface{}) (map[string]interface{}, error) {
				if operation != "create_sandbox" || !reflect.DeepEqual(fields, testCase.fields) {
					t.Fatalf("operation=%s fields=%v want=%v", operation, fields, testCase.fields)
				}
				return map[string]interface{}{"ok": true}, nil
			})
		})
		if code != 0 {
			t.Fatalf("arguments=%v code=%d output=%s", testCase.arguments, code, stdout.String())
		}
	}
	for _, argument := range []string{"0", "1025", "abc"} {
		called := false
		var stdout bytes.Buffer
		code := runWithTransportFactory([]string{"create-sandbox", "sandbox-research", "--cpu", argument}, &stdout, io.Discard, func(string) supervisorTransport {
			return supervisorTransportFunc(func(string, map[string]interface{}) (map[string]interface{}, error) {
				called = true
				return map[string]interface{}{"ok": true}, nil
			})
		})
		if code != 2 || called {
			t.Fatalf("--cpu %q must be refused before transport: code=%d called=%v", argument, code, called)
		}
	}
}

func TestCreateSandboxRefusesInvalidBudgetBeforeTransport(t *testing.T) {
	for _, argument := range []string{"511", "1048577", "abc", "-2048", ""} {
		called := false
		var stdout bytes.Buffer
		code := runWithTransportFactory([]string{"create-sandbox", "sandbox-research", argument}, &stdout, io.Discard, func(string) supervisorTransport {
			return supervisorTransportFunc(func(string, map[string]interface{}) (map[string]interface{}, error) {
				called = true
				return map[string]interface{}{"ok": true}, nil
			})
		})
		if code != 2 || called {
			t.Fatalf("argument=%q code=%d called=%v output=%s", argument, code, called, stdout.String())
		}
	}
}

func TestResizeSandboxRefusesBadTargetsBeforeTransport(t *testing.T) {
	tests := [][]string{
		{"resize-sandbox", "sandbox-research"},
		{"resize-sandbox", "Research", "--cpu", "2"},
		{"resize-sandbox", "sandbox-research", "--memory", "2GiX"},
		{"resize-sandbox", "sandbox-research", "--memory", "511"},
		{"resize-sandbox", "sandbox-research", "--memory", "1048577"},
		{"resize-sandbox", "sandbox-research", "--root-disk", "1023"},
		{"resize-sandbox", "sandbox-research", "--root-disk", "50%"},
		{"resize-sandbox", "sandbox-research", "--cpu", "0"},
		{"resize-sandbox", "sandbox-research", "--cpu"},
		{"resize-sandbox", "sandbox-research", "--memory", "8GiB", "--memory", "4GiB"},
		{"resize-sandbox", "sandbox-research", "--force"},
	}
	for _, arguments := range tests {
		t.Run(strings.Join(arguments, "-"), func(t *testing.T) {
			called := false
			var stdout bytes.Buffer
			code := runWithTransportFactory(arguments, &stdout, io.Discard, func(string) supervisorTransport {
				return supervisorTransportFunc(func(string, map[string]interface{}) (map[string]interface{}, error) {
					called = true
					return map[string]interface{}{"ok": true}, nil
				})
			})
			if code != 2 || called {
				t.Fatalf("arguments=%q code=%d called=%v output=%s", arguments, code, called, stdout.String())
			}
		})
	}
}

func TestParseSizeToMibAcceptsTheCreateFormsPlusSuffixes(t *testing.T) {
	tests := []struct {
		value string
		want  int64
		fails bool
	}{
		{value: "2048", want: 2048},
		{value: "2048MiB", want: 2048},
		{value: "2GiB", want: 2048},
		{value: "100GiB", want: 102400},
		{value: "", fails: true},
		{value: "2TiB", fails: true},
		{value: "2 GiB", fails: true},
		{value: "2gib", fails: true},
		{value: "GiB", fails: true},
		{value: "-2048", fails: true},
	}
	for _, testCase := range tests {
		got, ok := parseSizeToMib(testCase.value)
		if testCase.fails {
			if ok {
				t.Fatalf("size %q must be refused, got %d", testCase.value, got)
			}
			continue
		}
		if !ok || got != testCase.want {
			t.Fatalf("size %q parsed to %d (ok=%v), want %d", testCase.value, got, ok, testCase.want)
		}
	}
}

type closedOutput struct{}

func (closedOutput) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestResultWriteFailureIsAnError(t *testing.T) {
	transport := supervisorTransportFunc(func(string, map[string]interface{}) (map[string]interface{}, error) {
		return sandboxListFixture(), nil
	})
	for _, mode := range []string{"", "human"} {
		t.Setenv("SANDBOXING_OUTPUT", mode)
		if code := writeRemoteResult(transport, "list-sandboxes", "list_sandboxes", nil, closedOutput{}); code != 1 {
			t.Fatalf("mode %q closed output exit = %d, want 1", mode, code)
		}
	}
}
