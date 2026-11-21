package superserver

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rahulrajaram/chasm/internal/protocol"
	"github.com/rahulrajaram/chasm/internal/siteconfig"
	"github.com/rahulrajaram/chasm/internal/testsupport"
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

// bindTestSite binds the neutral testsite descriptor (no default sandbox).
func bindTestSite(t *testing.T) {
	t.Helper()
	path, err := testsupport.CopySiteConfig(filepath.Join(moduleRoot(), "testdata", "fixtures", "testsite", "site.conf"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := siteconfig.Init(path); err != nil {
		t.Fatal(err)
	}
}

func TestWriteFrameTimesOutWhenPeerDoesNotRead(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	writerFile := os.NewFile(uintptr(fds[0]), "writer")
	peerFile := os.NewFile(uintptr(fds[1]), "peer")
	defer writerFile.Close()
	defer peerFile.Close()
	writer, err := net.FileConn(writerFile)
	if errors.Is(err, syscall.EPERM) {
		// Some sandboxes prohibit Go's getsockopt on Unix sockets.
		t.Log("write deadline transport: net.Pipe (Unix FileConn denied)")
		writer, peer := net.Pipe()
		defer writer.Close()
		defer peer.Close()
		checkBlockedResponseWrite(t, writer, maxResponseBytes)
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	peer, err := net.FileConn(peerFile)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	t.Log("write deadline transport: Unix socket pair")
	checkBlockedResponseWrite(t, writer, maxResponseBytes)
}

func checkBlockedResponseWrite(t *testing.T, writer net.Conn, size int) {
	t.Helper()
	original := responseWriteTimeout
	responseWriteTimeout = 40 * time.Millisecond
	t.Cleanup(func() { responseWriteTimeout = original })
	start := time.Now()
	result := make(chan error, 1)
	go func() { result <- writeFrame(writer, make([]byte, size)) }()
	var err error
	select {
	case err = <-result:
	case <-time.After(3 * time.Second):
		t.Fatal("write to unread peer did not finish within 3 seconds; write deadline may be missing")
	}
	if !os.IsTimeout(err) || time.Since(start) > time.Second {
		t.Fatalf("write to unread peer: err=%v elapsed=%v", err, time.Since(start))
	}
}

func TestExecutorOperationForBindingsFailClosed(t *testing.T) {
	if operation, ok := executorOperationFor("stop_sandbox"); !ok || operation != "stop_guest" {
		t.Fatalf("stop_sandbox binding = %q, %v", operation, ok)
	}
	if operation, ok := executorOperationFor("pause_sandbox"); !ok || operation != "pause_guest" {
		t.Fatalf("pause_sandbox binding = %q, %v", operation, ok)
	}
	if operation, ok := executorOperationFor("resume_sandbox"); !ok || operation != "resume_guest" {
		t.Fatalf("resume_sandbox binding = %q, %v", operation, ok)
	}
	if operation, ok := executorOperationFor("delete_sandbox"); !ok || operation != "delete_sandbox" {
		t.Fatalf("delete_sandbox binding = %q, %v", operation, ok)
	}
	if operation, ok := executorOperationFor("admit_launch"); !ok || operation != "admit_launch" {
		t.Fatalf("admit_launch binding = %q, %v", operation, ok)
	}
	if operation, ok := executorOperationFor("resize_sandbox"); !ok || operation != "resize_sandbox" {
		t.Fatalf("resize_sandbox binding = %q, %v", operation, ok)
	}
	if _, ok := executorOperationFor("drop_instance"); ok {
		t.Fatal("unknown operation was admitted")
	}
}

func TestDefaultGuestInstanceComesFromTheSite(t *testing.T) {
	bindTestSite(t)
	if _, err := defaultGuestInstance(); err == nil {
		t.Fatal("a site without default_sandbox must refuse to name a guest")
	} else {
		var delivered *Error
		if !errors.As(err, &delivered) || delivered.Code != "missing_default_sandbox" || !strings.Contains(delivered.Message, "default_sandbox") {
			t.Fatalf("unset default_sandbox must fail closed naming the key, got %v", err)
		}
	}
	bindTestSiteWithDefaultSandbox(t, "demo")
	instance, err := defaultGuestInstance()
	if err != nil || instance != "sandbox-demo" {
		t.Fatalf("default guest must be sandbox-<default_sandbox>, got %q err=%v", instance, err)
	}
}

func bindTestSiteWithDefaultSandbox(t *testing.T, name string) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(moduleRoot(), "testdata", "fixtures", "testsite", "site.conf"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(content), "default_sandbox=\n", "default_sandbox="+name+"\n", 1)
	path := filepath.Join(t.TempDir(), "site.conf")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := siteconfig.Init(path); err != nil {
		t.Fatal(err)
	}
}

// TestDispatchForwardsEveryOptionalField pins the supervisor→executor
// boundary: one per-operation optional-field table drives create and
// resize, and only the fields the request carried are forwarded. Mutation
// checked: removing a field from optionalExecutorFields (as the dropped
// budget_cpu on create once was) fails the corresponding case with a
// missing-key error.
func TestDispatchForwardsEveryOptionalField(t *testing.T) {
	bindTestSite(t)
	base := func(operation string) map[string]interface{} {
		return map[string]interface{}{"operation": operation, "request_id": "req-1", "instance": "sandbox-research"}
	}
	tests := []struct {
		name      string
		operation string
		fields    map[string]interface{}
		want      map[string]interface{}
	}{
		{
			name: "create with no budget", operation: "create_sandbox",
			want: map[string]interface{}{"version": protocol.ProtocolVersion, "request_id": executorRequestID("req-1", "create_sandbox"), "operation": "create_sandbox", "instance": "sandbox-research"},
		},
		{
			name: "create with memory only", operation: "create_sandbox",
			fields: map[string]interface{}{"budget_memory_mib": 2048},
			want:   map[string]interface{}{"version": protocol.ProtocolVersion, "request_id": executorRequestID("req-1", "create_sandbox"), "operation": "create_sandbox", "instance": "sandbox-research", "budget_memory_mib": 2048},
		},
		{
			name: "create with cpu only", operation: "create_sandbox",
			fields: map[string]interface{}{"budget_cpu": 8},
			want:   map[string]interface{}{"version": protocol.ProtocolVersion, "request_id": executorRequestID("req-1", "create_sandbox"), "operation": "create_sandbox", "instance": "sandbox-research", "budget_cpu": 8},
		},
		{
			name: "create with both budgets", operation: "create_sandbox",
			fields: map[string]interface{}{"budget_memory_mib": 2048, "budget_cpu": 8},
			want:   map[string]interface{}{"version": protocol.ProtocolVersion, "request_id": executorRequestID("req-1", "create_sandbox"), "operation": "create_sandbox", "instance": "sandbox-research", "budget_memory_mib": 2048, "budget_cpu": 8},
		},
		{
			name: "resize memory alone", operation: "resize_sandbox",
			fields: map[string]interface{}{"budget_memory_mib": 4096},
			want:   map[string]interface{}{"version": protocol.ProtocolVersion, "request_id": executorRequestID("req-1", "resize_sandbox"), "operation": "resize_sandbox", "instance": "sandbox-research", "budget_memory_mib": 4096},
		},
		{
			name: "resize cpu alone", operation: "resize_sandbox",
			fields: map[string]interface{}{"budget_cpu": 4},
			want:   map[string]interface{}{"version": protocol.ProtocolVersion, "request_id": executorRequestID("req-1", "resize_sandbox"), "operation": "resize_sandbox", "instance": "sandbox-research", "budget_cpu": 4},
		},
		{
			name: "resize root disk alone", operation: "resize_sandbox",
			fields: map[string]interface{}{"root_disk_mib": 65536},
			want:   map[string]interface{}{"version": protocol.ProtocolVersion, "request_id": executorRequestID("req-1", "resize_sandbox"), "operation": "resize_sandbox", "instance": "sandbox-research", "root_disk_mib": 65536},
		},
		{
			name: "resize all three", operation: "resize_sandbox",
			fields: map[string]interface{}{"budget_memory_mib": 4096, "budget_cpu": 4, "root_disk_mib": 65536},
			want:   map[string]interface{}{"version": protocol.ProtocolVersion, "request_id": executorRequestID("req-1", "resize_sandbox"), "operation": "resize_sandbox", "instance": "sandbox-research", "budget_memory_mib": 4096, "budget_cpu": 4, "root_disk_mib": 65536},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			payload := base(testCase.operation)
			for key, value := range testCase.fields {
				payload[key] = value
			}
			supervisor := New(t.TempDir(), "")
			var captured map[string]interface{}
			supervisor.executorCallFn = func(received map[string]interface{}) (map[string]interface{}, error) {
				captured = received
				return map[string]interface{}{}, nil
			}
			if _, err := supervisor.Dispatch(payload); err != nil {
				t.Fatalf("dispatch %s: %v", testCase.operation, err)
			}
			if len(captured) != len(testCase.want) {
				t.Fatalf("forwarded payload = %v, want exactly %v", captured, testCase.want)
			}
			for key, want := range testCase.want {
				if captured[key] != want {
					t.Fatalf("forwarded payload field %s = %v, want %v (full payload %v)", key, captured[key], want, captured)
				}
			}
		})
	}
}

// restoreLookup restores the injected account-lookup seam after a test.
func restoreLookup(t *testing.T) {
	t.Helper()
	previous := lookupOperatorAccount
	t.Cleanup(func() { lookupOperatorAccount = previous })
}

// TestAllowedOperatorUIDsResolvesNSSOnlyOperator covers the accepted
// installer boundary: an operator account the host's NSS resolves (for
// example an SSSD/LDAP user) that has no entry in any local passwd file.
// The lookup seam stands in for the NSS-aware getpwnam path; the assertion
// is that the resolved UID is admitted beside root, not silently dropped.
func TestAllowedOperatorUIDsResolvesNSSOnlyOperator(t *testing.T) {
	bindTestSite(t)
	restoreLookup(t)
	const nssOnlyUID = 4242
	const nssOnlyGID = 4243
	lookupOperatorAccount = func(name string) (int, int, error) {
		if name != "chasmops" {
			t.Fatalf("lookup called with %q, want the configured operator", name)
		}
		return nssOnlyUID, nssOnlyGID, nil
	}
	uids, err := AllowedOperatorUIDs()
	if err != nil {
		t.Fatalf("AllowedOperatorUIDs: %v", err)
	}
	if !uids[nssOnlyUID] {
		t.Fatalf("NSS-only operator uid %d is not admitted: %v", nssOnlyUID, uids)
	}
	if !uids[0] {
		t.Fatalf("root uid 0 is not admitted: %v", uids)
	}
	if len(uids) != 2 {
		t.Fatalf("allowed set = %v, want exactly root and the operator", uids)
	}
	gid, err := OperatorGID()
	if err != nil {
		t.Fatalf("OperatorGID: %v", err)
	}
	if gid != nssOnlyGID {
		t.Fatalf("OperatorGID = %d, want %d", gid, nssOnlyGID)
	}
}

// TestAllowedOperatorUIDsFailsClosedOnLookupFailure: an unresolvable
// operator is a clear startup error, never a silent root-only allowlist,
// and root admission never masks the failure.
func TestAllowedOperatorUIDsFailsClosedOnLookupFailure(t *testing.T) {
	bindTestSite(t)
	restoreLookup(t)
	lookupOperatorAccount = func(name string) (int, int, error) {
		return 0, 0, errors.New("user: unknown user chasmops")
	}
	if _, err := AllowedOperatorUIDs(); err == nil {
		t.Fatal("AllowedOperatorUIDs succeeded for an unresolvable operator")
	} else if !strings.Contains(err.Error(), "chasmops") {
		t.Fatalf("lookup failure does not name the operator: %v", err)
	}
	if _, err := OperatorGID(); err == nil {
		t.Fatal("OperatorGID succeeded for an unresolvable operator")
	}
}

// TestAllowedOperatorUIDsPreservesPeerAuthorizationSemantics: the local
// account path keeps working through the same seam (the map remains the
// peer-authorization set: root plus the operator's resolved UID).
func TestAllowedOperatorUIDsPreservesPeerAuthorizationSemantics(t *testing.T) {
	bindTestSite(t)
	restoreLookup(t)
	lookupOperatorAccount = func(name string) (int, int, error) {
		return 1001, 1001, nil
	}
	uids, err := AllowedOperatorUIDs()
	if err != nil {
		t.Fatalf("AllowedOperatorUIDs: %v", err)
	}
	if !uids[0] || !uids[1001] || len(uids) != 2 {
		t.Fatalf("allowed set = %v, want {0, 1001}", uids)
	}
}
