package execserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/rahulrajaram/chasm/internal/protocol"
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

type auditGoldenCorpus struct {
	SchemaVersion int               `json:"schema_version"`
	Cases         []auditGoldenCase `json:"cases"`
}

type auditGoldenCase struct {
	Name      string          `json:"name"`
	At        string          `json:"at"`
	PeerUID   int             `json:"peer_uid"`
	OK        bool            `json:"ok"`
	ErrorCode *string         `json:"error_code"`
	Request   json.RawMessage `json:"request"`
	Expected  string          `json:"expected"`
}

func TestAuditJSONLMatchesPythonGoldenBytes(t *testing.T) {
	corpus := loadAuditGoldenCorpus(t)
	operations := make([]string, 0, len(corpus.Cases))
	for _, testCase := range corpus.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			request, err := protocol.ParseExecutor(testCase.Request)
			if err != nil {
				t.Fatalf("parse request: %v", err)
			}
			at, err := time.Parse(time.RFC3339Nano, testCase.At)
			if err != nil {
				t.Fatalf("parse time: %v", err)
			}
			errorCode := ""
			if testCase.ErrorCode != nil {
				errorCode = *testCase.ErrorCode
			}
			encoded, err := encodeAuditRecord(&request, testCase.PeerUID, testCase.OK, errorCode, at)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if string(encoded) != testCase.Expected {
				t.Fatalf("audit bytes differ from Python:\n got  %q\n want %q", encoded, testCase.Expected)
			}
		})
		request, _ := protocol.ParseExecutor(testCase.Request)
		operations = append(operations, request.Operation)
	}
	wantOperations := []string{
		"inspect_default_sandbox", "ensure_guest_running", "collect_performance",
	}
	if !reflect.DeepEqual(operations, wantOperations) {
		t.Fatalf("golden corpus operation coverage differs: got %v want %v", operations, wantOperations)
	}
}

func TestJSONLAuditAppendsPrivateOwnedRecords(t *testing.T) {
	corpus := loadAuditGoldenCorpus(t)
	testCase := corpus.Cases[0]
	request, err := protocol.ParseExecutor(testCase.Request)
	if err != nil {
		t.Fatal(err)
	}
	at, err := time.Parse(time.RFC3339Nano, testCase.At)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "audit")
	audit := jsonlAuditWithClock(dir, "audit.jsonl", uint32(os.Geteuid()), func() time.Time { return at })
	if err := audit(&request, testCase.PeerUID, true, ""); err != nil {
		t.Fatal(err)
	}
	if err := audit(&request, testCase.PeerUID, false, "invalid_state"); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	failure, err := encodeAuditRecord(&request, testCase.PeerUID, false, "invalid_state", at)
	if err != nil {
		t.Fatal(err)
	}
	want := testCase.Expected + string(failure)
	if string(content) != want {
		t.Fatalf("append mismatch:\n got  %q\n want %q", content, want)
	}
	assertOwnedMode(t, dir, uint32(os.Geteuid()), 0o700)
	assertOwnedMode(t, filepath.Join(dir, "audit.jsonl"), uint32(os.Geteuid()), 0o600)
}

func TestJSONLAuditRefusesUnexpectedOwnerAndMode(t *testing.T) {
	corpus := loadAuditGoldenCorpus(t)
	request, err := protocol.ParseExecutor(corpus.Cases[0].Request)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	wrongOwnerDir := filepath.Join(root, "wrong-owner")
	wrongOwnerAudit := jsonlAuditWithClock(wrongOwnerDir, "audit.jsonl", uint32(os.Geteuid()+1), time.Now)
	if err := wrongOwnerAudit(&request, 984, true, ""); err == nil {
		t.Fatal("unexpected owner must fail closed")
	}
	if _, err := os.Stat(filepath.Join(wrongOwnerDir, "audit.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("unexpected-owner audit file must not exist: %v", err)
	}

	wrongModeDir := filepath.Join(root, "wrong-mode")
	if err := os.Mkdir(wrongModeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(wrongModeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	wrongModeAudit := jsonlAuditWithClock(wrongModeDir, "audit.jsonl", uint32(os.Geteuid()), time.Now)
	if err := wrongModeAudit(&request, 984, true, ""); err == nil {
		t.Fatal("unexpected mode must fail closed")
	}
	if _, err := os.Stat(filepath.Join(wrongModeDir, "audit.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("wrong-mode audit file must not exist: %v", err)
	}
}

func TestJSONLAuditSurfacesOpenFailure(t *testing.T) {
	corpus := loadAuditGoldenCorpus(t)
	request, err := protocol.ParseExecutor(corpus.Cases[0].Request)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "audit")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "audit.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	audit := jsonlAuditWithClock(dir, "audit.jsonl", uint32(os.Geteuid()), time.Now)
	if err := audit(&request, 984, true, ""); err == nil {
		t.Fatal("audit open failure was discarded")
	}
}

func loadAuditGoldenCorpus(t *testing.T) auditGoldenCorpus {
	t.Helper()
	path := filepath.Join(moduleRoot(), "testdata", "language-evaluation", "audit-jsonl-golden.json")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var corpus auditGoldenCorpus
	if err := json.Unmarshal(content, &corpus); err != nil {
		t.Fatal(err)
	}
	if corpus.SchemaVersion != 1 || len(corpus.Cases) == 0 {
		t.Fatalf("invalid audit golden corpus: %+v", corpus)
	}
	return corpus
}

func assertOwnedMode(t *testing.T, path string, expectedUID uint32, expectedMode os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != expectedMode || !ownedBy(info, expectedUID) {
		t.Fatalf("metadata mismatch for %s: mode=%#o", path, info.Mode().Perm())
	}
}
