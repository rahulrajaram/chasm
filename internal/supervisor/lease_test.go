package supervisor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriterLeaseRejectsConcurrentControllerAndRecoversAfterRelease(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	first, err := AcquireWriterLease(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := AcquireWriterLease(root)
	if second != nil {
		t.Fatal("second writer unexpectedly acquired lease")
	}
	var codeError CodeError
	if !errors.As(err, &codeError) || codeError.Code != "writer_busy" {
		t.Fatalf("second writer error=%T %v", err, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := AcquireWriterLease(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := third.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWriterLeaseIsPrivateAndRecordsHolderEvidence(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	lease, err := AcquireWriterLease(root)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	path := filepath.Join(root, writerLeaseName)
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !ownedByEffectiveUser(info) {
		t.Fatalf("unsafe writer lease metadata: %v", info.Mode())
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "pid=") || !strings.Contains(string(content), "started_at=") {
		t.Fatalf("writer lease evidence is incomplete: %q", content)
	}
}

func TestWriterLeaseRefusesSymlink(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "state")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(parent, "target")
	if err := os.WriteFile(target, []byte("not a lease"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, writerLeaseName)); err != nil {
		t.Fatal(err)
	}
	if lease, err := AcquireWriterLease(root); err == nil || lease != nil {
		t.Fatal("symlink writer lease was admitted")
	}
}
