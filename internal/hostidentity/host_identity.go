// Package hostidentity computes and verifies the machine identity the site
// descriptor pins. Provisioning writes the fingerprint of this machine into
// the identity marker at siteconfig.MarkerPath; every later privileged entry
// recomputes the fingerprint from the live machine and requires it to match
// the pin before touching host state. There is no pre-baked fingerprint.
package hostidentity

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/rahulrajaram/chasm/internal/siteconfig"
)

// Material is the machine material that the identity fingerprint is derived
// from. Anything that changes host identity (reinstall, disk replacement,
// VM cloning) changes the fingerprint and forces re-provisioning.
type Material struct {
	Hostname    string
	MachineID   string
	RootUUID    string
	ProductName string
}

// Fingerprint derives the site identity fingerprint from the canonicalized
// machine material.
func Fingerprint(material Material) string {
	payload := fmt.Sprintf("hostname=%s\nmachine_id=%s\nroot_uuid=%s\nproduct_name=%s\n", material.Hostname, material.MachineID, material.RootUUID, material.ProductName)
	digest := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(digest[:])
}

// machineFingerprint recomputes the fingerprint from the live machine.
func machineFingerprint() (string, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return "", errors.New("cannot read site hostname")
	}
	machineID, err := readBoundedText("/etc/machine-id", 4096)
	if err != nil {
		return "", errors.New("cannot read machine identity")
	}
	productName := ""
	if value, err := readBoundedText("/sys/class/dmi/id/product_name", 4096); err == nil {
		productName = value
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("cannot read machine product identity")
	}
	command := exec.Command("/usr/bin/findmnt", "-n", "-o", "UUID", "/")
	rootUUIDRaw, err := command.Output()
	if err != nil || len(rootUUIDRaw) > 4096 {
		return "", errors.New("cannot read machine root identity")
	}
	return Fingerprint(Material{
		Hostname:    strings.TrimSpace(hostname),
		MachineID:   machineID,
		RootUUID:    strings.TrimSpace(string(rootUUIDRaw)),
		ProductName: productName,
	}), nil
}

// RequireSiteHost verifies that this machine is the host the site was
// provisioned for: the invoking user is root, the recomputed machine
// fingerprint equals the pin recorded in the marker at siteconfig.MarkerPath,
// and the marker file itself is well-formed. Privileged entry points run this
// before mutating host state.
func RequireSiteHost() error {
	if os.Geteuid() != 0 {
		return errors.New("run through sudo as root")
	}
	actual, err := machineFingerprint()
	if err != nil {
		return err
	}
	pinned, err := readBoundedText(siteconfig.MarkerPath, 4096)
	if err != nil {
		return errors.New("site identity marker is unavailable")
	}
	if actual != pinned {
		return errors.New("this host does not match the site identity marker")
	}
	return ValidateMarker(siteconfig.MarkerPath, pinned)
}

// SiteFingerprint returns the identity this host is pinned to: the
// fingerprint recorded in the marker at siteconfig.MarkerPath, re-verified
// against the live machine material. Callers that need the pinned identity
// but cannot resolve it (marker missing, machine drifted) must omit the
// identity field rather than fabricate one.
func SiteFingerprint() (string, error) {
	pinned, err := readBoundedText(siteconfig.MarkerPath, 4096)
	if err != nil {
		return "", err
	}
	actual, err := machineFingerprint()
	if err != nil {
		return "", err
	}
	if actual != pinned {
		return "", errors.New("this host does not match the site identity marker")
	}
	return actual, nil
}

// ValidateMarker checks the marker file at path: a regular, non-symlink,
// root-owned 0444 file whose content is exactly expectedContent.
func ValidateMarker(path, expectedContent string) error {
	return validateMarker(path, expectedContent, 0)
}

func validateMarker(path, expectedContent string, expectedUID uint32) error {
	info, err := os.Lstat(path)
	if err != nil {
		return errors.New("site identity marker is unavailable")
	}
	metadata, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || metadata.Uid != expectedUID || info.Mode().Perm() != 0o444 {
		return errors.New("site identity marker ownership or mode is invalid")
	}
	content, err := readBoundedText(path, 4096)
	if err != nil || content != expectedContent {
		return errors.New("site identity marker content is invalid")
	}
	return nil
}

func readBoundedText(path string, maximum int64) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > maximum {
		return "", errors.New("identity file metadata is invalid")
	}
	raw, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return "", err
	}
	if int64(len(raw)) > maximum {
		return "", errors.New("identity file is oversized")
	}
	return strings.TrimSpace(string(raw)), nil
}
