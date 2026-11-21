package hostidentity

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFingerprintUsesCanonicalMaterial(t *testing.T) {
	material := Material{Hostname: "chasm-test", MachineID: "machine", RootUUID: "root", ProductName: "HP"}
	first := Fingerprint(material)
	second := Fingerprint(material)
	if len(first) != 64 || first != second {
		t.Fatalf("fingerprint is not a stable SHA-256 digest: %q %q", first, second)
	}
	material.Hostname = "other"
	if Fingerprint(material) == first {
		t.Fatal("hostname change did not affect identity")
	}
}

func TestValidateMarkerRejectsModeContentAndSymlink(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "marker")
	if err := os.WriteFile(path, []byte("expected\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := validateMarker(path, "expected", uint32(os.Geteuid())); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateMarker(path, "expected", uint32(os.Geteuid())); err == nil {
		t.Fatal("publicly writable marker was admitted")
	}
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := validateMarker(path, "different", uint32(os.Geteuid())); err == nil {
		t.Fatal("wrong marker content was admitted")
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := validateMarker(link, "expected", uint32(os.Geteuid())); err == nil {
		t.Fatal("marker symlink was admitted")
	}
}
