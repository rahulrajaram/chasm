package execserver

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLayoutEntryBoundAppliesAfterRequestedDepthScan(t *testing.T) {
	original := runIncus
	t.Cleanup(func() { runIncus = original })
	var observedDepth int
	runIncus = func(args []string, _ io.Reader, stdout io.Writer, _ []*os.File, _ time.Duration) (int, error) {
		for i, arg := range args {
			if arg == "-maxdepth" {
				observedDepth, _ = strconv.Atoi(args[i+1])
			}
		}
		count := maxCatalogEntries - 1
		if observedDepth > 3 {
			count = maxCatalogEntries + 1
		}
		var output strings.Builder
		for i := 0; i < count; i++ {
			fmt.Fprintf(&output, "f\tproject/file-%04d\x00", i)
		}
		_, _ = io.WriteString(stdout, output.String())
		return 0, nil
	}
	if _, err := scanSandboxLayout("sandbox-demo", 3); err != nil || observedDepth != 3 {
		t.Fatalf("requested depth-three scan failed: depth=%d err=%v", observedDepth, err)
	}
	if _, err := scanSandboxLayout("sandbox-demo", 5); err == nil || observedDepth != 5 {
		t.Fatalf("over-bound depth-five scan was accepted: depth=%d err=%v", observedDepth, err)
	}
}

func TestRepositoryScanIgnoresUnrelatedLayoutEntryBound(t *testing.T) {
	original := runIncus
	t.Cleanup(func() { runIncus = original })
	runIncus = func(args []string, _ io.Reader, stdout io.Writer, _ []*os.File, _ time.Duration) (int, error) {
		if !strings.Contains(strings.Join(args, " "), "-name .git") {
			t.Fatalf("repository scan did not use the Git-only find expression: %v", args)
		}
		if !strings.Contains(strings.Join(args, " "), "-maxdepth 12") {
			t.Fatalf("repository scan depth changed: %v", args)
		}
		_, _ = io.WriteString(stdout, "./example-project\x00")
		return 0, nil
	}
	repositories, err := scanRepositories("sandbox-demo")
	if err != nil || len(repositories) != 1 || repositories[0] != "example-project" {
		t.Fatalf("repositories=%v err=%v", repositories, err)
	}
}

func TestRepositoryDiscoveryNormalizesFindPathsAndExcludesTaskWorktrees(t *testing.T) {
	repositories, err := parseRepositoryOutput([]byte("./example-project\x00./.worktrees/example-project/fix-a\x00nested/other\x00example-project\x00"))
	if err != nil {
		t.Fatal(err)
	}
	if len(repositories) != 2 || repositories[0] != "example-project" || repositories[1] != "nested/other" {
		t.Fatalf("unexpected primary repositories: %v", repositories)
	}
	for _, path := range []string{"./../escape", "./.worktrees/../escape", "././project"} {
		if _, err := parseRepositoryOutput([]byte(path + "\x00")); err == nil {
			t.Fatalf("unsafe path accepted: %q", path)
		}
	}
}

func TestParseLayoutOutputAcceptsBoundedRelativeEntries(t *testing.T) {
	entries, err := parseLayoutOutput([]byte("d\trepo\x00f\trepo/README.md\x00l\trepo/link\x00"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 || entries[0].Path != "repo" || entries[1].Type != "file" || entries[2].Type != "symlink" {
		t.Fatalf("entries=%v", entries)
	}
}

func TestParseLayoutOutputRejectsTraversalDuplicatesAndUnsupportedTypes(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte("d\t../host\x00"),
		[]byte("d\trepo\x00f\trepo\x00"),
		[]byte("p\tpipe\x00"),
		[]byte("d\trepo"),
	} {
		if _, err := parseLayoutOutput(raw); err == nil {
			t.Fatalf("unsafe layout was accepted: %q", raw)
		}
	}
}

func TestParseRepositoryOutputDeduplicatesAndRejectsTraversal(t *testing.T) {
	repositories, err := parseRepositoryOutput([]byte("alpha\x00nested/beta\x00alpha\x00"))
	if err != nil {
		t.Fatal(err)
	}
	if len(repositories) != 2 || repositories[0] != "alpha" || repositories[1] != "nested/beta" {
		t.Fatalf("repositories=%v", repositories)
	}
	if _, err := parseRepositoryOutput([]byte("../../escape\x00")); err == nil {
		t.Fatal("repository traversal was accepted")
	}
}

func TestLogicalSandboxNameRemovesManagedPrefix(t *testing.T) {
	if got := logicalSandboxName("sandbox-research"); got != "research" {
		t.Fatalf("named=%q", got)
	}
	if got := logicalSandboxName("sandbox-demo"); got != "demo" {
		t.Fatalf("named=%q", got)
	}
}

func TestManagedDiskContractRequiresExactGlobalAndWorkspaceMounts(t *testing.T) {
	valid := map[string]string{"type": "disk", "pool": "chasm-test", "source": "chasm-shared", "path": "/shared", "readonly": "false"}
	if !managedDiskConfigValid(valid, "chasm-test", "chasm-shared", "/shared") {
		t.Fatal("exact shared disk contract was refused")
	}
	for key, value := range map[string]string{"source": "private", "path": "/elsewhere", "readonly": "true", "pool": "other"} {
		drifted := make(map[string]string, len(valid))
		for name, original := range valid {
			drifted[name] = original
		}
		drifted[key] = value
		if managedDiskConfigValid(drifted, "chasm-test", "chasm-shared", "/shared") {
			t.Fatalf("disk drift was accepted: %s=%s", key, value)
		}
	}
}

func TestSetSandboxDescriptionIsIdempotentAndPostconditionChecked(t *testing.T) {
	current := ""
	mutations := 0
	loader := func(string) (map[string]interface{}, error) {
		config := map[string]interface{}{}
		if current != "" {
			config[descriptionKey()] = current
		}
		return map[string]interface{}{"config": config}, nil
	}
	mutator := func(instance, description string) error {
		if instance != "sandbox-research" {
			t.Fatalf("unexpected instance: %s", instance)
		}
		mutations++
		current = description
		return nil
	}

	result, err := setSandboxDescriptionWith("sandbox-research", "LLM evaluation", loader, mutator)
	if err != nil || result["description"] != "LLM evaluation" || result["changed"] != true || mutations != 1 {
		t.Fatalf("set result=%v mutations=%d err=%v", result, mutations, err)
	}
	result, err = setSandboxDescriptionWith("sandbox-research", "LLM evaluation", loader, mutator)
	if err != nil || result["changed"] != false || mutations != 1 {
		t.Fatalf("idempotent result=%v mutations=%d err=%v", result, mutations, err)
	}
	result, err = setSandboxDescriptionWith("sandbox-research", "", loader, mutator)
	if err != nil || result["description"] != "" || result["changed"] != true || mutations != 2 {
		t.Fatalf("clear result=%v mutations=%d err=%v", result, mutations, err)
	}
}

func TestSetSandboxDescriptionRefusesUnprovenMutation(t *testing.T) {
	loader := func(string) (map[string]interface{}, error) {
		return map[string]interface{}{"config": map[string]interface{}{}}, nil
	}
	result, err := setSandboxDescriptionWith("sandbox-research", "Research", loader, func(string, string) error { return nil })
	if err == nil || result != nil {
		t.Fatalf("unproven mutation was accepted: result=%v err=%v", result, err)
	}
}

func TestSandboxDescriptionMetadataRejectsControlCharacters(t *testing.T) {
	metadata := map[string]interface{}{"config": map[string]interface{}{descriptionKey(): "line one\nline two"}}
	if _, err := sandboxDescriptionFromMetadata(metadata); err == nil {
		t.Fatal("unsafe description metadata was accepted")
	}
}
