package protocol

import "testing"

func TestDeleteSandboxExecutorParsing(t *testing.T) {
	accepted := `{"version":1,"request_id":"req-1","operation":"delete_sandbox","instance":"sandbox-perf-cycle-1"}`
	request, err := ParseExecutor([]byte(accepted))
	if err != nil {
		t.Fatalf("valid delete_sandbox refused: %v", err)
	}
	if request.Instance() != "sandbox-perf-cycle-1" {
		t.Fatalf("instance = %q", request.Instance())
	}
	rejections := []struct {
		name string
		raw  string
		code string
	}{
		{"extra-field", `{"version":1,"request_id":"req-1","operation":"delete_sandbox","instance":"sandbox-research","force":true}`, "invalid_fields"},
		{"missing-instance", `{"version":1,"request_id":"req-1","operation":"delete_sandbox"}`, "invalid_fields"},
		{"bad-instance", `{"version":1,"request_id":"req-1","operation":"delete_sandbox","instance":"Template-Build"}`, "invalid_instance"},
	}
	for _, testCase := range rejections {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := ParseExecutor([]byte(testCase.raw))
			code, ok := ErrorCode(err)
			if !ok || code != testCase.code {
				t.Fatalf("error = %v, want code %s", err, testCase.code)
			}
		})
	}
}

func TestDeleteSandboxPublicParsing(t *testing.T) {
	accepted := `{"version":1,"request_id":"req-2","operation":"delete_sandbox","instance":"sandbox-perf-cycle-1"}`
	request, err := ParsePublic([]byte(accepted))
	if err != nil {
		t.Fatalf("valid public delete_sandbox refused: %v", err)
	}
	if request.Instance() != "sandbox-perf-cycle-1" {
		t.Fatalf("instance = %q", request.Instance())
	}
	if _, err := ParsePublic([]byte(`{"version":1,"request_id":"req-2","operation":"delete_sandbox"}`)); err == nil {
		t.Fatal("public delete_sandbox without instance was admitted")
	}
}
