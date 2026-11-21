package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// chasm resize: the resize_sandbox operation requires an instance and at
// least one target, and every admitted target is validated exactly like its
// create_sandbox counterpart. The same shape is enforced at both surfaces
// (public and executor), so each case runs through both parsers.
func TestResizeSandboxParsesEveryTargetShape(t *testing.T) {
	tests := []struct {
		name    string
		fields  string
		fails   bool
		message string
	}{
		{name: "memory only", fields: `,"budget_memory_mib":4096`},
		{name: "cpu only", fields: `,"budget_cpu":4`},
		{name: "root disk only", fields: `,"root_disk_mib":65536`},
		{name: "all three", fields: `,"budget_memory_mib":4096,"budget_cpu":4,"root_disk_mib":65536`},
		{name: "no target", fields: ``, fails: true, message: "at least one"},
		{name: "memory floor", fields: `,"budget_memory_mib":511`, fails: true, message: "budget_memory_mib must be"},
		{name: "cpu floor", fields: `,"budget_cpu":0`, fails: true, message: "budget_cpu must be"},
		{name: "root disk floor", fields: `,"root_disk_mib":1023`, fails: true, message: "root_disk_mib must be"},
		{name: "root disk safety bound", fields: `,"root_disk_mib":4194305`, fails: true, message: "root_disk_mib must be"},
		{name: "root disk fractional", fields: `,"root_disk_mib":64.5`, fails: true, message: "root_disk_mib must be"},
		{name: "extra field", fields: `,"budget_cpu":4,"force":true`, fails: true, message: "extra=force"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			raw := []byte(`{"version":` + protocolVersionJSON() + `,"request_id":"req-1","operation":"resize_sandbox","instance":"sandbox-research"` + testCase.fields + `}`)
			if _, err := ParsePublic(raw); err != nil {
				if !testCase.fails {
					t.Fatalf("public parse must admit the request: %v", err)
				}
				assertResizeRefusal(t, err, testCase.message)
			} else if testCase.fails {
				t.Fatal("public parse must refuse the request")
			}
			if _, err := ParseExecutor(raw); err != nil {
				if !testCase.fails {
					t.Fatalf("executor parse must admit the request: %v", err)
				}
				assertResizeRefusal(t, err, testCase.message)
				return
			} else if testCase.fails {
				t.Fatal("executor parse must refuse the request")
			}
		})
	}
}

func TestResizeSandboxRefusesAnUnmanagedInstanceName(t *testing.T) {
	raw := []byte(`{"version":` + protocolVersionJSON() + `,"request_id":"req-1","operation":"resize_sandbox","instance":"Research","budget_cpu":4}`)
	if _, err := ParsePublic(raw); err == nil {
		t.Fatal("a non-managed instance name must be refused")
	}
	if _, err := ParseExecutor(raw); err == nil {
		t.Fatal("a non-managed instance name must be refused by the executor parser")
	}
}

func protocolVersionJSON() string {
	raw, _ := json.Marshal(ProtocolVersion)
	return string(raw)
}

func assertResizeRefusal(t *testing.T, err error, message string) {
	t.Helper()
	if message == "" {
		return
	}
	rendered := err.Error()
	var decoderError DecoderError
	if asDecoderError(err, &decoderError) {
		rendered = decoderError.Message
	}
	if !strings.Contains(rendered, message) {
		t.Fatalf("refusal %q lacks %q", rendered, message)
	}
}

func asDecoderError(err error, target *DecoderError) bool {
	if decoderError, ok := err.(DecoderError); ok {
		*target = decoderError
		return true
	}
	return false
}
