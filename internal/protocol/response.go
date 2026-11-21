package protocol

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"unicode/utf8"
)

// surrogateEscape matches any \u escape encoding a UTF-16 surrogate (lone
// or paired). The symmetric strict contract refuses the escape form on
// the raw frame: the Python reference cannot preserve lone surrogates
// through JSON while Go cannot distinguish them after decoding.
var surrogateEscape = regexp.MustCompile(`\\u[dD][89a-fA-F][0-9a-fA-F]{2}`)

const MaxResponseBytes = 1024 * 1024

// ResponseError is an operation error carried by a well-formed executor
// response frame. It is a delivered disposition, not a decode refusal:
// the frame itself satisfied the wire contract.
type ResponseError struct {
	Code    string
	Message string
}

func (e *ResponseError) Error() string { return e.Code }

// ParseResponse decodes one executor response frame against the expected
// request ID. A nil requestID requires the frame to carry a null or absent
// request_id. Decode refusals return a DecoderError with code
// "response_too_large" or "invalid_response". A well-formed error frame is
// returned as a *ResponseError and never as a refusal.
func ParseResponse(raw []byte, requestID *string) (map[string]interface{}, error) {
	if len(raw) > MaxResponseBytes {
		return nil, fail("response_too_large")
	}
	payload, err := parseResponseObject(raw)
	if err != nil {
		return nil, err
	}
	version, ok := numberIntField(payload, "version")
	if !ok || version != ProtocolVersion {
		return nil, fail("invalid_response")
	}
	actualID, present := payload["request_id"]
	if requestID == nil {
		if present && actualID != nil {
			return nil, fail("invalid_response")
		}
	} else {
		actual, isString := actualID.(string)
		if !isString || actual != *requestID {
			return nil, fail("invalid_response")
		}
	}
	okFlag, present := payload["ok"]
	if !present || okFlag != true {
		return nil, responseErrorField(payload)
	}
	result, isObject := payload["result"].(map[string]interface{})
	if !isObject {
		return nil, fail("invalid_response")
	}
	return result, nil
}

func responseErrorField(payload map[string]interface{}) error {
	errorValue, present := payload["error"]
	if !present {
		return fail("invalid_response")
	}
	errorObject, isObject := errorValue.(map[string]interface{})
	if !isObject {
		return fail("invalid_response")
	}
	code, codeIsString := errorObject["code"].(string)
	message, messageIsString := errorObject["message"].(string)
	if !codeIsString || !messageIsString {
		return fail("invalid_response")
	}
	return &ResponseError{Code: code, Message: message}
}

// parseResponseObject applies the same strict JSON subset as the request
// decoder (one object, no duplicate fields, no non-standard constants, no
// trailing data) while mapping every violation to the response-surface
// refusal code "invalid_response".
func parseResponseObject(raw []byte) (map[string]interface{}, error) {
	if len(raw) == 0 {
		return nil, fail("invalid_response")
	}
	// The Python reference decodes bytes as UTF-8 and refuses invalid
	// sequences; Go's encoding/json would silently replace them with
	// U+FFFD. Match the reference: invalid UTF-8 is a refusal.
	if !utf8.Valid(raw) {
		return nil, fail("invalid_response")
	}
	if surrogateEscape.Match(raw) {
		return nil, fail("invalid_response")
	}
	if !json.Valid(raw) {
		return nil, fail("invalid_response")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	tok, err := decoder.Token()
	if err != nil {
		return nil, fail("invalid_response")
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return nil, fail("invalid_response")
	}
	payload := map[string]interface{}{}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, fail("invalid_response")
		}
		key, isString := keyToken.(string)
		if !isString {
			return nil, fail("invalid_response")
		}
		if _, exists := payload[key]; exists {
			return nil, fail("invalid_response")
		}
		var value interface{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fail("invalid_response")
		}
		payload[key] = value
	}
	end, err := decoder.Token()
	if err != nil {
		return nil, fail("invalid_response")
	}
	if endDelim, isDelim := end.(json.Delim); !isDelim || endDelim != '}' {
		return nil, fail("invalid_response")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, fail("invalid_response")
	}
	return payload, nil
}
