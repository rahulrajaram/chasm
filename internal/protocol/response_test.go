package protocol

import (
	"strings"
	"testing"
)

func mustParseResponse(t *testing.T, raw string, requestID *string) (map[string]interface{}, error) {
	t.Helper()
	return ParseResponse([]byte(raw), requestID)
}

func strPtr(value string) *string { return &value }

func TestParseResponseAcceptsWellFormedSuccessFrame(t *testing.T) {
	result, err := mustParseResponse(t,
		`{"ok":true,"request_id":"r1","result":{"marker":"v"},"version":1}`+"\n",
		strPtr("r1"),
	)
	if err != nil {
		t.Fatalf("expected acceptance, got %v", err)
	}
	if result["marker"] != "v" {
		t.Fatalf("unexpected result payload: %v", result)
	}
}

func TestParseResponseRejectsMalformedFrames(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		requestID *string
		wantCode  string
	}{
		{"empty", "", strPtr("r1"), "invalid_response"},
		{"not json", "not-json{", strPtr("r1"), "invalid_response"},
		{"not an object", "[1,2,3]\n", strPtr("r1"), "invalid_response"},
		{"wrong version", `{"version":2,"request_id":"r1","ok":true,"result":{}}` + "\n", strPtr("r1"), "invalid_response"},
		{"boolean version", `{"version":true,"request_id":"r1","ok":true,"result":{}}` + "\n", strPtr("r1"), "invalid_response"},
		{"request id mismatch", `{"version":1,"request_id":"other","ok":true,"result":{}}` + "\n", strPtr("r1"), "invalid_response"},
		{"missing request id", `{"version":1,"ok":true,"result":{}}` + "\n", strPtr("r1"), "invalid_response"},
		{"error not object", `{"version":1,"request_id":"r1","ok":false,"error":"invalid"}` + "\n", strPtr("r1"), "invalid_response"},
		{"error code not string", `{"version":1,"request_id":"r1","ok":false,"error":{"code":1,"message":"m"}}` + "\n", strPtr("r1"), "invalid_response"},
		{"error message not string", `{"version":1,"request_id":"r1","ok":false,"error":{"code":"c","message":2}}` + "\n", strPtr("r1"), "invalid_response"},
		{"missing error on not-ok", `{"version":1,"request_id":"r1","ok":false}` + "\n", strPtr("r1"), "invalid_response"},
		{"result not object", `{"version":1,"request_id":"r1","ok":true,"result":[1]}` + "\n", strPtr("r1"), "invalid_response"},
		{"missing result", `{"version":1,"request_id":"r1","ok":true}` + "\n", strPtr("r1"), "invalid_response"},
		{"duplicate fields", `{"version":1,"request_id":"r1","request_id":"r1","ok":true,"result":{}}` + "\n", strPtr("r1"), "invalid_response"},
		{"nan constant", `{"version":1,"request_id":"r1","ok":true,"result":{"value":NaN}}` + "\n", strPtr("r1"), "invalid_response"},
		{"trailing data", `{"version":1,"request_id":"r1","ok":true,"result":{}} extra` + "\n", strPtr("r1"), "invalid_response"},
		{"oversized", strings.Repeat("a", MaxResponseBytes+1), strPtr("r1"), "response_too_large"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := mustParseResponse(t, testCase.raw, testCase.requestID)
			if err == nil {
				t.Fatalf("expected refusal %s", testCase.wantCode)
			}
			code, ok := ErrorCode(err)
			if !ok || code != testCase.wantCode {
				t.Fatalf("expected %s, got %v", testCase.wantCode, err)
			}
		})
	}
}

func TestParseResponseAcceptsLenientWhitespaceAndNullRequestID(t *testing.T) {
	if _, err := mustParseResponse(t,
		`{"version":1,"request_id":"r1","ok":true,"result":{}}`+"\n  \n",
		strPtr("r1"),
	); err != nil {
		t.Fatalf("expected acceptance with trailing whitespace, got %v", err)
	}
	if _, err := mustParseResponse(t,
		`{"version":1,"request_id":null,"ok":true,"result":{}}`+"\n",
		nil,
	); err != nil {
		t.Fatalf("expected acceptance of null request id, got %v", err)
	}
	if _, err := mustParseResponse(t,
		`{"version":1,"ok":true,"result":{}}`+"\n",
		nil,
	); err != nil {
		t.Fatalf("expected acceptance of absent request id, got %v", err)
	}
}

func TestParseResponseDeliversOperationErrors(t *testing.T) {
	_, err := mustParseResponse(t,
		`{"version":1,"request_id":"r1","ok":false,"error":{"code":"unknown_operation","message":"operation is not permitted"}}`+"\n",
		strPtr("r1"),
	)
	var responseError *ResponseError
	if err == nil {
		t.Fatal("expected an operation error")
	}
	if !asResponseError(err, &responseError) {
		t.Fatalf("expected ResponseError, got %T", err)
	}
	if responseError.Code != "unknown_operation" || responseError.Message != "operation is not permitted" {
		t.Fatalf("unexpected error frame: %+v", responseError)
	}
}

func TestParseResponseAtExactSizeBound(t *testing.T) {
	// A frame of exactly MaxResponseBytes is accepted if well-formed; one
	// byte more is refused with response_too_large.
	prefix := `{"pad":"`
	suffix := `","version":1,"request_id":"r1","ok":true,"result":{}}` + "\n"
	padding := MaxResponseBytes - len(prefix) - len(suffix)
	if padding < 0 {
		t.Fatal("test frame exceeds the response bound")
	}
	raw := prefix + strings.Repeat("p", padding) + suffix
	if len(raw) != MaxResponseBytes {
		t.Fatalf("constructed frame is %d bytes, want %d", len(raw), MaxResponseBytes)
	}
	if _, err := mustParseResponse(t, raw, strPtr("r1")); err != nil {
		t.Fatalf("expected acceptance at exact bound, got %v", err)
	}
	oversized := raw + " "
	if _, err := mustParseResponse(t, oversized, strPtr("r1")); err == nil {
		t.Fatal("expected refusal one byte over the bound")
	} else if code, _ := ErrorCode(err); code != "response_too_large" {
		t.Fatalf("expected response_too_large, got %s", code)
	}
}

func asResponseError(err error, target **ResponseError) bool {
	if responseError, ok := err.(*ResponseError); ok {
		*target = responseError
		return true
	}
	return false
}

func TestParseResponseRejectsInvalidUTF8(t *testing.T) {
	// The Python reference refuses invalid UTF-8 bytes inside strings;
	// Go's encoding/json would silently replace them with U+FFFD. The
	// decoder must refuse the frame instead (pinned as
	// reject-invalid-utf8 in the response corpus).
	raw := []byte{'{', '"', 'v', 'e', 'r', 's', 'i', 'o', 'n', '"', ':', '1', ',', '"', 'r', 0xFF, '"', '}', '\n'}
	_, err := ParseResponse(raw, nil)
	if err == nil {
		t.Fatal("expected refusal of invalid UTF-8")
	}
	if code, _ := ErrorCode(err); code != "invalid_response" {
		t.Fatalf("expected invalid_response, got %s", code)
	}
}
