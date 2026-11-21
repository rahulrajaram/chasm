package superserver

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Tests for the NSS-aware account lookup (account.go): the pure
// passwd-record parser, the fixed-getent execution path, and the argv
// validation. The lookupOperatorAccount seam is exercised in
// superserver_test.go; these tests cover the default implementation's
// own behavior without touching the host's account database (except
// through the injected command seam where noted).

func TestParsePasswdRecordAcceptsNSSOnlyRecord(t *testing.T) {
	// An SSSD/LDAP operator: the record exists only in NSS output, never
	// in a local passwd file. Shape is what getent prints.
	uid, gid, err := parsePasswdRecord("chasmops:x:4242:4243:Operator:/home/chasmops:/usr/bin/zsh\n", "chasmops")
	if err != nil {
		t.Fatalf("parsePasswdRecord: %v", err)
	}
	if uid != 4242 || gid != 4243 {
		t.Fatalf("uid/gid = %d/%d, want 4242/4243", uid, gid)
	}
}

func TestParsePasswdRecordIgnoresForeignAccounts(t *testing.T) {
	output := "root:x:0:0:root:/root:/bin/bash\nchasmops:x:4242:4243:Operator:/home/chasmops:/usr/bin/zsh\n"
	uid, gid, err := parsePasswdRecord(output, "chasmops")
	if err != nil {
		t.Fatalf("parsePasswdRecord: %v", err)
	}
	if uid != 4242 || gid != 4243 {
		t.Fatalf("uid/gid = %d/%d, want 4242/4243", uid, gid)
	}
}

func TestParsePasswdRecordRejectsMalformedRecords(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		account string
	}{
		{name: "too few fields", output: "chasmops:x:4242:4243\n", account: "chasmops"},
		{name: "too many fields", output: "chasmops:x:4242:4243:g:e:c:o:s\n", account: "chasmops"},
		{name: "wrong account only", output: "other:x:1:1::/home/other:/bin/sh\n", account: "chasmops"},
		{name: "account absent", output: "", account: "chasmops"},
		{name: "duplicate account lines", output: "chasmops:x:4242:4243::/home/c:/zsh\nchasmops:x:4244:4245::/home/c:/zsh\n", account: "chasmops"},
		{name: "non-numeric uid", output: "chasmops:x:abc:4243::/home/c:/zsh\n", account: "chasmops"},
		{name: "non-numeric gid", output: "chasmops:x:4242:xyz::/home/c:/zsh\n", account: "chasmops"},
		{name: "negative uid", output: "chasmops:x:-1:4243::/home/c:/zsh\n", account: "chasmops"},
		{name: "signed uid", output: "chasmops:x:+4242:4243::/home/c:/zsh\n", account: "chasmops"},
		{name: "leading-zero uid", output: "chasmops:x:04242:4243::/home/c:/zsh\n", account: "chasmops"},
		{name: "empty uid", output: "chasmops:x::4243::/home/c:/zsh\n", account: "chasmops"},
		{name: "oversized uid", output: "chasmops:x:12345678901:4243::/home/c:/zsh\n", account: "chasmops"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			if uid, gid, err := parsePasswdRecord(testCase.output, testCase.account); err == nil {
				t.Fatalf("malformed record accepted: uid=%d gid=%d", uid, gid)
			}
		})
	}
}

func TestValidateAccountNameRefusesArgvInjection(t *testing.T) {
	for _, bad := range []string{"", "-s", "--", "a b", "a:b", "a\tb", "a\nb", "a\x00b"} {
		if err := validateAccountName(bad); err == nil {
			t.Fatalf("unsafe account name %q was accepted", bad)
		}
	}
	for _, good := range []string{"chasmops", "root", "svc-agent1"} {
		if err := validateAccountName(good); err != nil {
			t.Fatalf("safe account name %q was refused: %v", good, err)
		}
	}
}

func TestGetentAccountLookupFailsOnUnknownAccount(t *testing.T) {
	// A failed NSS lookup must be a clear failure naming getent, never a silent zero.
	script := filepath.Join(t.TempDir(), "getent-failure.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	original := runGetent
	t.Cleanup(func() { runGetent = original })
	runGetent = func(ctx context.Context, name string) ([]byte, error) {
		if name != "chasm-no-such-operator-account-3x9f" {
			t.Fatalf("lookup name = %q", name)
		}
		return exec.CommandContext(ctx, script).Output()
	}
	uid, gid, err := getentAccountLookup("chasm-no-such-operator-account-3x9f")
	if err == nil {
		t.Fatalf("unknown account resolved to uid=%d gid=%d", uid, gid)
	}
	if !strings.Contains(err.Error(), "exit 2") || !strings.Contains(err.Error(), "NSS") {
		t.Fatalf("lookup failure does not name exit 2 and NSS: %v", err)
	}
}

func TestGetentAccountLookupReportsExecutionFailure(t *testing.T) {
	original := runGetent
	t.Cleanup(func() { runGetent = original })
	runGetent = func(context.Context, string) ([]byte, error) {
		return nil, errors.New("stubbed execution failure")
	}
	_, _, err := getentAccountLookup("chasmops")
	if err == nil || !strings.Contains(err.Error(), "could not be executed") {
		t.Fatalf("execution failure wording: %v", err)
	}
}

func TestGetentAccountLookupFailsOnUnsafeName(t *testing.T) {
	if _, _, err := getentAccountLookup("-s"); err == nil {
		t.Fatal("an argv-injecting account name was accepted")
	}
}

// TestDefaultLookupIsGetentBound pins the seam default: the supervisor must
// resolve through the getent path (cgo-independent), not os/user.
func TestDefaultLookupIsGetentBound(t *testing.T) {
	if reflect.ValueOf(lookupOperatorAccount).Pointer() != reflect.ValueOf(getentAccountLookup).Pointer() {
		t.Fatal("default lookup seam is not getentAccountLookup")
	}
}
