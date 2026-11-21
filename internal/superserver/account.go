package superserver

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// The supervisor resolves the operator account through the host's NSS, the
// same contract the installer's preflight uses (`getent passwd`). os/user
// is NOT used for this: with CGO_ENABLED=0 (or when cgo is unavailable) its
// Lookup degrades to reading the local account files only, which would
// silently drop NSS-only (SSSD/LDAP) operators from the allowed-UID set in
// an otherwise valid static build. Instead the supervisor shells out to the
// fixed getent executable the core preflight already relies on, with an
// explicit argv vector and a wall-clock bound, and parses the passwd record
// with a pure, strictly validating parser — no cgo and no GCC dependency.

const (
	// getentExecutable is the fixed lookup command. The name is pinned to
	// this one binary; operator-controlled data is passed as an argument,
	// never through a shell.
	getentExecutable = "/usr/bin/getent"
	// accountLookupTimeout bounds each getent invocation: an NSS backend
	// that hangs (a dead LDAP/SSSD upstream) must fail startup within this
	// budget, not wedge the supervisor.
	accountLookupTimeout = 5 * time.Second
)

var runGetent = func(ctx context.Context, name string) ([]byte, error) {
	return exec.CommandContext(ctx, getentExecutable, "passwd", name).Output()
}

// errAccountParse marks a passwd record that getent returned but the parser
// rejected (malformed shape, wrong account, or non-numeric ids).
type errAccountParse struct{ reason string }

func (e *errAccountParse) Error() string { return e.reason }

// validateAccountName refuses operator names that could be misread as a
// getent option or corrupt its argv: the name must be a non-empty run of
// printable, non-whitespace, non-colon characters not starting with '-'.
func validateAccountName(name string) error {
	if name == "" {
		return fmt.Errorf("operator account name is empty")
	}
	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("operator account name %q must not start with '-'", name)
	}
	if strings.ContainsAny(name, ":\n\r\t ") || strings.ContainsRune(name, 0) {
		return fmt.Errorf("operator account name %q contains a forbidden character", name)
	}
	return nil
}

// parsePasswdRecord extracts the numeric uid and gid from `getent passwd`
// output for exactly one account. getent prints one passwd line per match
// (name:passwd:uid:gid:gecos:home:shell); the parser requires exactly one
// line naming the requested account, seven colon-separated fields, and
// canonical non-negative decimal uid/gid fields. Any other shape fails
// closed.
func parsePasswdRecord(output, name string) (int, int, error) {
	var uid, gid int
	found := false
	for _, line := range strings.Split(output, "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) != 7 {
			return 0, 0, &errAccountParse{fmt.Sprintf("passwd record has %d fields, want 7", len(fields))}
		}
		if fields[0] != name {
			continue
		}
		if found {
			return 0, 0, &errAccountParse{fmt.Sprintf("passwd output names the account %q more than once", name)}
		}
		found = true
		parsedUID, err := parsePasswdNumber(fields[2], "uid")
		if err != nil {
			return 0, 0, err
		}
		parsedGID, err := parsePasswdNumber(fields[3], "gid")
		if err != nil {
			return 0, 0, err
		}
		uid, gid = parsedUID, parsedGID
	}
	if !found {
		return 0, 0, &errAccountParse{fmt.Sprintf("passwd output does not name the account %q", name)}
	}
	return uid, gid, nil
}

// parsePasswdNumber accepts only a canonical non-negative decimal integer:
// digits only, at most ten of them, no sign, no leading zeros.
func parsePasswdNumber(field, what string) (int, error) {
	if field == "" || len(field) > 10 {
		return 0, &errAccountParse{fmt.Sprintf("passwd record has an invalid %s field", what)}
	}
	if len(field) > 1 && field[0] == '0' {
		return 0, &errAccountParse{fmt.Sprintf("passwd record has a non-canonical %s field %q", what, field)}
	}
	for i := 0; i < len(field); i++ {
		if field[i] < '0' || field[i] > '9' {
			return 0, &errAccountParse{fmt.Sprintf("passwd record has a non-numeric %s field %q", what, field)}
		}
	}
	value, err := strconv.Atoi(field)
	if err != nil {
		return 0, &errAccountParse{fmt.Sprintf("passwd record has an out-of-range %s field %q", what, field)}
	}
	return value, nil
}

// getentAccountLookup resolves one account through the host's NSS with a
// fixed getent executable, an explicit argv vector, and a wall-clock
// bound. A missing binary, a nonzero exit (unknown account, NSS backend
// failure), a timeout, or an unparsable record is a clear error.
func getentAccountLookup(name string) (int, int, error) {
	if err := validateAccountName(name); err != nil {
		return 0, 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), accountLookupTimeout)
	defer cancel()
	output, err := runGetent(ctx, name)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return 0, 0, fmt.Errorf("getent passwd %s did not complete within %s", name, accountLookupTimeout)
		}
		var missing *exec.Error
		if errors.As(err, &missing) {
			return 0, 0, fmt.Errorf("the account lookup executable %s is not available: %w", getentExecutable, err)
		}
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			return 0, 0, fmt.Errorf("getent passwd %s failed (exit %d): the account could not be resolved through NSS", name, exitError.ExitCode())
		}
		return 0, 0, fmt.Errorf("getent passwd %s could not be executed: %w", name, err)
	}
	uid, gid, err := parsePasswdRecord(string(output), name)
	if err != nil {
		return 0, 0, fmt.Errorf("the passwd record for %s is invalid: %w", name, err)
	}
	return uid, gid, nil
}
