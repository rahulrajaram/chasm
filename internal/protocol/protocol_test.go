package protocol

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type goldenFixture struct {
	Summary struct {
		CaseCount int `json:"case_count"`
		Passed    int `json:"passed"`
		Failed    int `json:"failed"`
	} `json:"summary"`
	Cases []goldenCase `json:"cases"`
}

type goldenCase struct {
	Name       string `json:"name"`
	Parser     string `json:"parser"`
	Expected   string `json:"expected"`
	Observed   string `json:"observed"`
	Status     string `json:"status"`
	WireSHA256 string `json:"wire_sha256"`
}

type protocolCorpus struct {
	Cases          []map[string]interface{} `json:"cases"`
	GeneratedCases []map[string]interface{} `json:"generated_cases"`
}

func TestStrictDecodersMatchGoldenFixture(t *testing.T) {
	root := repositoryRoot(t)
	golden := readGolden(t, root)
	corpus := readCorpus(t, root)
	inputs := corpusInputs(t, corpus)

	if golden.Summary.CaseCount != 40 || golden.Summary.Passed != 40 || golden.Summary.Failed != 0 {
		t.Fatalf("unexpected golden summary: %+v", golden.Summary)
	}
	for _, testCase := range golden.Cases {
		raw, ok := inputs[testCase.Name]
		if !ok {
			t.Fatalf("golden case %s has no corpus input", testCase.Name)
		}
		if got := observed(testCase.Parser, raw); got != testCase.Observed {
			t.Fatalf("%s: observed %s, want %s", testCase.Name, got, testCase.Observed)
		}
	}
}

func observed(parser string, raw []byte) string {
	var operation string
	var err error
	if parser == "executor" {
		operation, err = ParseExecutorOperation(raw)
	} else {
		operation, err = ParsePublicOperation(raw)
	}
	if err != nil {
		if code, ok := ErrorCode(err); ok {
			return "rejected:" + code
		}
		return "unexpected-exception"
	}
	return "accepted:" + operation
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	current, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(current, "go.mod")); err == nil {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			t.Fatal("repository root not found")
		}
		current = parent
	}
}

func readGolden(t *testing.T, root string) goldenFixture {
	t.Helper()
	var fixture goldenFixture
	readJSON(t, filepath.Join(root, "testdata/language-evaluation/protocol-golden-fixtures.json"), &fixture)
	return fixture
}

func readCorpus(t *testing.T, root string) protocolCorpus {
	t.Helper()
	var corpus protocolCorpus
	readJSON(t, filepath.Join(root, "testdata/protocol/request-corpus.json"), &corpus)
	return corpus
}

func readJSON(t *testing.T, path string, target interface{}) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		t.Fatal(err)
	}
}

func corpusInputs(t *testing.T, corpus protocolCorpus) map[string][]byte {
	t.Helper()
	result := map[string][]byte{}
	for _, testCase := range append(corpus.Cases, corpus.GeneratedCases...) {
		name, ok := testCase["name"].(string)
		if !ok || name == "" {
			t.Fatal("case missing name")
		}
		if wire, ok := testCase["wire"].(string); ok {
			result[name] = []byte(wire)
			continue
		}
		generator, ok := testCase["generator"].(map[string]interface{})
		if !ok {
			t.Fatalf("case %s has no wire or generator", name)
		}
		switch generator["kind"] {
		case "repeat_byte":
			byteNumber := generator["byte"].(json.Number)
			countNumber := generator["count"].(json.Number)
			byteValue, err := byteNumber.Int64()
			if err != nil {
				t.Fatal(err)
			}
			count, err := countNumber.Int64()
			if err != nil {
				t.Fatal(err)
			}
			result[name] = bytes.Repeat([]byte{byte(byteValue)}, int(count))
		case "literal_hex":
			encoded := generator["hex"].(string)
			raw, err := hex.DecodeString(encoded)
			if err != nil {
				t.Fatal(err)
			}
			result[name] = raw
		default:
			t.Fatalf("unknown generator for %s", name)
		}
	}
	return result
}
