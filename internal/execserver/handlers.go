package execserver

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rahulrajaram/chasm/internal/incuslocal"
	"github.com/rahulrajaram/chasm/internal/protocol"
	"github.com/rahulrajaram/chasm/internal/siteconfig"
)

// Constants mirroring host_control/executor_server.py and
// host_control/performance_probe.py. Host-specific values (the supervisor
// user, guest uid/gid, the memfd syscall number) resolve through the site
// descriptor instead of being baked here.
const (
	auditRoot           = "/var/lib/chasm/executor"
	auditFileName       = "audit.jsonl"
	supervisorStateRoot = "/var/lib/chasm/supervisor"
	maxIncusResponse    = 2 * 1024 * 1024

	lifecycleLockPath = "/run/lock/chasm-lifecycle.lock"
)

var incusClientRoot = "/run/chasm/incus-client"
var resolveIncusSocket = incuslocal.SocketPath

// descriptionKey is the instance-config key carrying a sandbox's
// description; the namespace derives from the site's resource prefix.
func descriptionKey() string { return siteconfig.Active().MetadataKey("description") }

// serviceNamesFor lists the systemd units the executor supervises.
func serviceNamesFor(site siteconfig.Site) []string {
	return []string{"incus.service", "chasm-firewall.service"}
}

func protoError(code, message string) error { return protocolError(code, message) }

func debugExcerpt(out []byte) string {
	if os.Getenv("EXECSERVER_DEBUG") == "" {
		return ""
	}
	text := string(out)
	if len(text) > 200 {
		text = text[len(text)-200:]
	}
	return strings.TrimSpace(text)
}

func debugWriter() io.Writer {
	if os.Getenv("EXECSERVER_DEBUG") != "" {
		return os.Stderr
	}
	return io.Discard
}

func syncDir(path string) error {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	return syscall.Fsync(fd)
}

// --- supervisor uid (staged store owner) ---

var cachedUID int = -1

func SupervisorUID() int {
	if cachedUID >= 0 {
		return cachedUID
	}
	if value := os.Getenv("_AGENT_SUPERVISOR_UID"); value != "" {
		if uid, err := strconv.Atoi(value); err == nil {
			cachedUID = uid
			return uid
		}
	}
	content, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(content), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) >= 3 && fields[0] == siteconfig.Active().SupervisorUser {
			uid, err := strconv.Atoi(fields[2])
			if err == nil {
				cachedUID = uid
				return uid
			}
		}
	}
	return 0
}

// --- incus helpers ---

func incusEnvironment() (map[string]string, error) {
	socketPath, err := resolveIncusSocket()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(incusClientRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create private Incus client directory: %w", err)
	}
	var info syscall.Stat_t
	if err := syscall.Lstat(incusClientRoot, &info); err != nil {
		return nil, fmt.Errorf("inspect private Incus client directory: %w", err)
	}
	if info.Uid != uint32(os.Geteuid()) || info.Mode&0o777 != 0o700 {
		return nil, errors.New("private Incus client directory has unsafe ownership or mode")
	}
	return map[string]string{
		"HOME":         incusClientRoot,
		"INCUS_CONF":   incusClientRoot,
		"INCUS_SOCKET": socketPath,
		"LANG":         "C.UTF-8",
		"PATH":         "/usr/sbin:/usr/bin:/sbin:/bin",
	}, nil
}

func incusExec(arguments []string, stdin io.Reader, stdout io.Writer, passFDs []*os.File, timeout time.Duration) (int, error) {
	argv := append([]string{"--force-local"}, arguments...)
	ctx := context.Background()
	cancel := func() {}
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	values, err := incusEnvironment()
	if err != nil {
		return -1, err
	}
	cmd := exec.CommandContext(ctx, "incus", argv...)
	cmd.Env = make([]string, 0, len(values))
	for key, value := range values {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = io.Discard
	cmd.ExtraFiles = passFDs
	err = cmd.Run()
	if ctx.Err() != nil {
		return -1, ctx.Err()
	}
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return -1, err
}

func runScript(arguments []string, stdin io.Reader, timeout time.Duration) ([]byte, []byte, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, arguments[0], arguments[1:]...)
	cmd.Dir = "/"
	ownProcessGroup(cmd)
	cmd.Env = []string{"LANG=C.UTF-8", "PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.MultiWriter(&stderr, debugWriter())
	err := cmd.Run()
	if ctx.Err() != nil {
		return stdout.Bytes(), stderr.Bytes(), -1, ctx.Err()
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return stdout.Bytes(), stderr.Bytes(), exitErr.ExitCode(), nil
		}
		return stdout.Bytes(), stderr.Bytes(), -1, err
	}
	return stdout.Bytes(), stderr.Bytes(), 0, nil
}

func incusQueryGET(path string) (map[string]interface{}, error) {
	if !strings.HasPrefix(path, "/1.0/") || strings.ContainsAny(path, "?#") {
		return nil, protoError("unsafe_state", "Incus API path is not permitted")
	}
	var metadata map[string]interface{}
	if err := incusQueryMetadata(path, &metadata); err != nil {
		return nil, err
	}
	if metadata == nil {
		return nil, protoError("unsafe_state", "Incus API response envelope is invalid")
	}
	return metadata, nil
}

func incusQueryMetadata(path string, metadata interface{}) error {
	if path != "/1.0/instances?recursion=1" && (!strings.HasPrefix(path, "/1.0/") || strings.ContainsAny(path, "?#")) {
		return protoError("unsafe_state", "Incus API path is not permitted")
	}
	socketPath, err := resolveIncusSocket()
	if err != nil {
		return protoError("unsafe_state", "Incus local socket is unavailable")
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			conn, err := (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, "unix", socketPath)
			if err != nil {
				return nil, err
			}
			if err := incuslocal.RootPeer(conn); err == nil {
				return conn, nil
			}
			_ = conn.Close()
			return nil, errors.New("Incus socket peer is not authenticated as root")
		},
	}
	client := &http.Client{Transport: transport, Timeout: 20 * time.Second}
	response, err := client.Get("http://incus" + path)
	if err != nil {
		return protoError("unsafe_state", "Incus API is not reachable")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return protoError("unsafe_state", "Incus API returned an error status")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxIncusResponse+1))
	if err != nil {
		return protoError("unsafe_state", "Incus API response is invalid")
	}
	if len(raw) > maxIncusResponse {
		return protoError("unsafe_state", "Incus API response is oversized")
	}
	var envelope struct {
		Type       string          `json:"type"`
		StatusCode int             `json:"status_code"`
		Metadata   json.RawMessage `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Type != "sync" ||
		envelope.StatusCode != 200 || len(envelope.Metadata) == 0 || string(envelope.Metadata) == "null" {
		return protoError("unsafe_state", "Incus API response envelope is invalid")
	}
	if err := json.Unmarshal(envelope.Metadata, metadata); err != nil {
		return protoError("unsafe_state", "Incus API response metadata is invalid")
	}
	return nil
}

func instanceState() (map[string]interface{}, error) {
	instance, err := guestInstance()
	if err != nil {
		return nil, err
	}
	return instanceStateFor(instance)
}

func instanceStateFor(instance string) (map[string]interface{}, error) {
	if !protocol.ValidSandboxInstance(instance) {
		return nil, protoError("invalid_instance", "instance is not permitted")
	}
	return incusQueryGET("/1.0/instances/" + instance + "/state")
}

type sandboxInstance struct {
	Name        string
	Status      string
	PID         int64
	Description string
}

type incusInstanceSummary struct {
	Name            string                       `json:"name"`
	Status          string                       `json:"status"`
	Config          map[string]string            `json:"config"`
	ExpandedConfig  map[string]string            `json:"expanded_config"`
	Devices         map[string]map[string]string `json:"devices"`
	ExpandedDevices map[string]map[string]string `json:"expanded_devices"`
}

// listInstanceStatuses is the seam for the instance inventory read; tests
// inject a fake so inventory-dependent wiring runs without a live socket.
var listInstanceStatuses = func() ([]incusInstanceSummary, error) {
	var summaries []incusInstanceSummary
	if err := incusQueryMetadata("/1.0/instances?recursion=1", &summaries); err != nil {
		return nil, err
	}
	if len(summaries) > 1024 {
		return nil, protoError("unsafe_state", "Incus instance inventory exceeds its bound")
	}
	seen := make(map[string]struct{}, len(summaries))
	for _, summary := range summaries {
		if summary.Name == "" || (summary.Status != "Running" && summary.Status != "Frozen" && summary.Status != "Stopped") {
			return nil, protoError("unsafe_state", "Incus instance inventory is invalid")
		}
		if _, duplicate := seen[summary.Name]; duplicate {
			return nil, protoError("unsafe_state", "Incus instance inventory is ambiguous")
		}
		seen[summary.Name] = struct{}{}
	}
	return summaries, nil
}

func listSandboxInstances() ([]sandboxInstance, error) {
	summaries, err := listInstanceStatuses()
	if err != nil {
		return nil, err
	}
	return sandboxInstancesFromSummaries(summaries, instanceStateFor)
}

// sandboxInstancesFromSummaries is the inventory core of listSandboxInstances
// with the instance-state lookup injected so the managed/foreign filtering
// is unit-testable without a live Incus.
func sandboxInstancesFromSummaries(summaries []incusInstanceSummary, stateFor func(string) (map[string]interface{}, error)) ([]sandboxInstance, error) {
	site := siteconfig.Active()
	instances := make([]sandboxInstance, 0, len(summaries))
	for _, summary := range summaries {
		if !protocol.ValidSandboxInstance(summary.Name) {
			continue
		}
		if summary.Config[site.MetadataKey("managed")] != "true" {
			continue
		}
		if summary.Config[site.MetadataKey("logical-name")] != logicalSandboxName(summary.Name) ||
			summary.Config[site.MetadataKey("workspace-volume")] != site.WorkspaceVolumeName(summary.Name) ||
			summary.Config[site.MetadataKey("shared-volume")] != site.SharedVolumeName() ||
			!managedDiskConfigValid(summary.Devices["shared"], site.StoragePool, site.SharedVolumeName(), sharedPath) ||
			!managedDiskConfigValid(summary.Devices["workspace"], site.StoragePool, site.WorkspaceVolumeName(summary.Name), workspacePath) {
			return nil, protoError("unsafe_state", "managed sandbox inventory metadata is invalid")
		}
		state, err := stateFor(summary.Name)
		if err != nil {
			return nil, err
		}
		status, statusOK := state["status"].(string)
		pid, pidOK := numericInt64(state["pid"])
		if !statusOK || !pidOK || pid < 0 || (status != "Running" && status != "Frozen" && status != "Stopped") {
			return nil, protoError("unsafe_state", "Incus instance state is invalid")
		}
		if status != summary.Status {
			return nil, protoError("unsafe_state", "Incus instance inventory changed during inspection")
		}
		description := summary.Config[descriptionKey()]
		if !protocol.ValidSandboxDescription(description) {
			return nil, protoError("unsafe_state", "sandbox description metadata is invalid")
		}
		instances = append(instances, sandboxInstance{Name: summary.Name, Status: status, PID: pid, Description: description})
	}
	return instances, nil
}

// --- systemd ---

func systemdProperties(name string) (map[string]interface{}, error) {
	cmd := exec.Command("systemctl", "show", name, "--property=ActiveState,SubState,Result,MainPID,NRestarts,ExecMainStatus")
	cmd.Env = nil
	out, _, _, err := runScript(cmd.Args, nil, 15*time.Second)
	if err != nil {
		return nil, protoError("unsafe_state", "systemd properties are unavailable")
	}
	values := map[string]interface{}{"name": name}
	for _, line := range strings.Split(string(out), "\n") {
		if key, value, found := strings.Cut(line, "="); found {
			values[key] = value
		}
	}
	for _, numeric := range []string{"MainPID", "NRestarts", "ExecMainStatus"} {
		if parsed, err := strconv.ParseInt(fmt.Sprint(values[numeric]), 10, 64); err == nil {
			values[numeric] = parsed
		}
	}
	return values, nil
}

func normalizedServiceSummary(sample map[string]interface{}) (map[string]interface{}, error) {
	name, nameOK := sample["name"].(string)
	activeState, activeOK := sample["ActiveState"].(string)
	result, resultOK := sample["Result"].(string)
	restarts, restartsOK := sample["NRestarts"].(int64)
	if !nameOK || name == "" || !activeOK || !resultOK || !restartsOK || restarts < 0 {
		return nil, fmt.Errorf("systemd service evidence is invalid")
	}
	return map[string]interface{}{
		"name":         name,
		"active_state": activeState,
		"result":       result,
		"n_restarts":   restarts,
	}, nil
}

func normalizedPerformanceService(sample map[string]interface{}) (map[string]interface{}, error) {
	summary, err := normalizedServiceSummary(sample)
	if err != nil {
		return nil, err
	}
	subState, subStateOK := sample["SubState"].(string)
	mainPID, mainPIDOK := sample["MainPID"].(int64)
	execMainStatus, execMainStatusOK := sample["ExecMainStatus"].(int64)
	if !subStateOK || !mainPIDOK || mainPID < 0 || !execMainStatusOK {
		return nil, fmt.Errorf("systemd performance evidence is invalid")
	}
	return map[string]interface{}{
		"name":             summary["name"],
		"active_state":     summary["active_state"],
		"sub_state":        subState,
		"result":           summary["result"],
		"main_pid":         mainPID,
		"n_restarts":       summary["n_restarts"],
		"exec_main_status": execMainStatus,
	}, nil
}

func serviceIsActive(name string) bool {
	cmd := exec.Command("systemctl", "is-active", "--quiet", name)
	_, _, code, _ := runScript(cmd.Args, nil, 15*time.Second)
	return code == 0
}

func statDir(path string, mode os.FileMode) error {
	var info syscall.Stat_t
	if err := syscall.Lstat(path, &info); err != nil {
		return protoError("unsafe_state", "staged artifact directory is unsafe")
	}
	if info.Mode&syscall.S_IFMT != syscall.S_IFDIR || info.Uid != uint32(SupervisorUID()) || info.Mode&0o777 != uint32(mode) {
		return protoError("unsafe_state", "staged artifact directory is unsafe")
	}
	return nil
}

// --- guest readiness ---

func ensureGuestRunning(requestID string) (map[string]interface{}, error) {
	instance, err := guestInstance()
	if err != nil {
		return nil, err
	}
	return ensureSandboxRunning(instance, requestID, nil)
}

// sandboxInitialized reports whether the managed sandbox completed guest
// provisioning. The launcher sets the marker last.
// sandboxInitialized is the seam for the guest-initialized marker read.
var sandboxInitialized = func(instance string) bool {
	buffer := &boundedBuffer{limit: 4096}
	code, err := incusExec([]string{"config", "get", instance, siteconfig.Active().MetadataKey("initialized")}, nil, buffer, nil, 30*time.Second)
	return err == nil && code == 0 && strings.TrimSpace(string(buffer.Bytes())) == "true"
}

func ensureSandboxRunning(instance, requestID string, timings lifecycleTimings) (map[string]interface{}, error) {
	// The launcher takes the same lifecycle flock as this function, so
	// provisioning must happen outside the lock. Leave an uninitialized target
	// stopped; the governed admission check below must run before any start.
	if !sandboxInitialized(instance) {
		// Never adopt or start a managed sandbox whose guest was
		// never initialized; run the launcher so provisioning completes first.
		endProvision := timings.phase("provisioning")
		var provisioning lifecycleTimings
		if timings != nil {
			provisioning = make(lifecycleTimings)
		}
		err := runLauncherWithBudget(instance, true, 0, provisioning)
		endProvision()
		for phase, duration := range provisioning {
			timings["provisioning_"+phase] = duration
		}
		if err != nil {
			return nil, err
		}
	}
	return withLifecycleLock(func() (map[string]interface{}, error) {
		return ensureSandboxRunningUnlocked(instance, requestID, timings)
	})
}

func ensureSandboxRunningUnlocked(instance, requestID string, timings lifecycleTimings) (map[string]interface{}, error) {
	if err := requireManagedSandbox(instance); err != nil {
		return nil, err
	}
	summaries, err := listInstanceStatuses()
	if err != nil {
		return nil, protoError("invalid_state", "sandbox instance inventory is unavailable")
	}
	status, err := admitSandboxStart(instance, summaries)
	if err != nil {
		return nil, err
	}
	if !sandboxInitialized(instance) {
		// Provisioning already ran outside the lock in
		// ensureSandboxRunning; refuse to proceed if it did not complete.
		return nil, protoError("invalid_state", "sandbox guest initialization did not complete")
	}
	started := false
	switch status {
	case "Stopped":
		endBoot := timings.phase("boot")
		code, err := incusExec([]string{"start", instance}, nil, nil, nil, 180*time.Second)
		endBoot()
		if err != nil || code != 0 {
			return nil, protoError("invalid_state", "sandbox instance could not be started")
		}
		started = true
	case "Running":
	case "Frozen":
		if code, err := incusExec([]string{"resume", instance}, nil, nil, nil, 180*time.Second); err != nil || code != 0 {
			return nil, protoError("invalid_state", "sandbox instance could not be resumed")
		}
		started = true
	}

	endReady := timings.phase("guest_agent_ready")
	deadline := time.Now().Add(180 * time.Second)
	for time.Now().Before(deadline) {
		var ready bytes.Buffer
		code, err := incusExec([]string{"exec", instance, "--", "/usr/bin/true"}, nil, &ready, nil, 15*time.Second)
		if err == nil && code == 0 {
			endReady()
			confirmed, err := instanceStateFor(instance)
			if err != nil {
				return nil, protoError("postcondition_failed", "sandbox instance state is inconsistent")
			}
			if confirmed["status"] != "Running" {
				return nil, protoError("postcondition_failed", "sandbox instance state is inconsistent")
			}
			pid, pidOK := numericInt64(confirmed["pid"])
			if !pidOK || pid <= 0 {
				return nil, protoError("postcondition_failed", "sandbox instance state is inconsistent")
			}
			return sandboxRunningResult(instance, pid, started, requestGuestTimeResync), nil
		}
		time.Sleep(2 * time.Second)
	}
	endReady()
	return nil, protoError("guest_not_ready", "sandbox instance agent did not become ready")
}

// sandboxRunningResult assembles the shared start success payload. A
// lifecycle transition that booted or unfroze the guest (started) also
// requests one best-effort guest time resync and reports its outcome; an
// already-running guest reports "skipped" and is not touched.
func sandboxRunningResult(instance string, pid int64, started bool, timeResync guestTimeResyncer) map[string]interface{} {
	outcome := "skipped"
	if started {
		outcome = timeResync(instance)
	}
	return map[string]interface{}{
		"instance":        instance,
		"instance_status": "Running",
		"instance_pid":    pid,
		"agent_ready":     true,
		"started":         started,
		"time_resync":     outcome,
	}
}

// The host reserves a fixed floor for host services; named
// sandboxes may co-run only when their declared budgets fit the remaining
// headroom.
func budgetMetadataKey() string {
	return siteconfig.Active().MetadataKey("budget-memory-mib")
}

// cpuBudgetMetadataKey is the instance-config key carrying a sandbox's
// declared CPU budget; it is written together with limits.cpu and must
// equal it (idiom 1).
func cpuBudgetMetadataKey() string {
	return siteconfig.Active().MetadataKey("budget-cpu")
}

func validManagedSandboxSummary(summary incusInstanceSummary) bool {
	site := siteconfig.Active()
	return protocol.ValidSandboxInstance(summary.Name) &&
		summary.Config[site.MetadataKey("managed")] == "true" &&
		summary.Config[site.MetadataKey("logical-name")] == logicalSandboxName(summary.Name) &&
		summary.Config[site.MetadataKey("workspace-volume")] == site.WorkspaceVolumeName(summary.Name) &&
		summary.Config[site.MetadataKey("shared-volume")] == site.SharedVolumeName() &&
		managedDiskConfigValid(summary.Devices["shared"], site.StoragePool, site.SharedVolumeName(), sharedPath) &&
		managedDiskConfigValid(summary.Devices["workspace"], site.StoragePool, site.WorkspaceVolumeName(summary.Name), workspacePath)
}

func admitSandboxStart(instance string, summaries []incusInstanceSummary) (string, error) {
	meminfo, err := numericKibRecords("/proc/meminfo", meminfoFields)
	if err != nil {
		return "", protoError("unsafe_state", "host memory inventory is unavailable")
	}
	totalKiB, ok := numericInt64(meminfo["MemTotal"])
	if !ok || totalKiB <= 0 {
		return "", protoError("unsafe_state", "host memory inventory is invalid")
	}
	return admitSandboxStartWith(instance, summaries, totalKiB/1024)
}

func admitSandboxStartWith(instance string, summaries []incusInstanceSummary, memTotalMiB int64) (string, error) {
	if !protocol.ValidSandboxInstance(instance) {
		return "", protoError("invalid_instance", "instance is not permitted")
	}
	reserve := int64(siteconfig.Active().HostReserveMiB)
	if memTotalMiB <= 0 || reserve < 0 || reserve > memTotalMiB {
		return "", protoError("unsafe_state", "host memory inventory is invalid")
	}
	status := ""
	runningBudgets := int64(0)
	targetBudget := int64(0)
	for _, summary := range summaries {
		if summary.Name == "" || (summary.Status != "Running" && summary.Status != "Frozen" && summary.Status != "Stopped") {
			return "", protoError("unsafe_state", "Incus instance inventory is invalid")
		}
		if summary.Name == instance {
			if status != "" {
				return "", protoError("unsafe_state", "Incus instance inventory is ambiguous")
			}
			if !validManagedSandboxSummary(summary) {
				return "", protoError("unsafe_state", "managed sandbox inventory metadata is invalid")
			}
			budget, err := admissibleMemoryBudget(summary)
			if err != nil {
				return "", err
			}
			status = summary.Status
			targetBudget = budget
			continue
		}
		if summary.Status != "Running" && summary.Status != "Frozen" {
			continue
		}
		if !protocol.ValidSandboxInstance(summary.Name) {
			return "", protoError("unsafe_state", "Incus instance inventory contains an unmanaged running instance")
		}
		if !validManagedSandboxSummary(summary) {
			return "", protoError("unsafe_state", "managed sandbox inventory metadata is invalid")
		}
		budget, err := admissibleMemoryBudget(summary)
		if err != nil {
			return "", err
		}
		runningBudgets += budget
	}
	if status == "" {
		return "", protoError("invalid_state", "sandbox instance does not exist")
	}
	cap := memTotalMiB - reserve
	if runningBudgets+targetBudget > cap {
		return "", protoError("resource_exhausted", fmt.Sprintf(
			"host memory headroom exhausted: running sandbox budgets %d MiB + requested %d MiB exceed this host's cap %d MiB (MemTotal %d MiB - host_reserve_mib %d)",
			runningBudgets, targetBudget, cap, memTotalMiB, reserve))
	}
	return status, nil
}

// admitLaunch is the read-only admission gate for launch-path starts.
func admitLaunch(instance string) (map[string]interface{}, error) {
	if err := requireManagedSandbox(instance); err != nil {
		return nil, err
	}
	summaries, err := listInstanceStatuses()
	if err != nil {
		return nil, protoError("invalid_state", "sandbox instance inventory is unavailable")
	}
	status, err := admitSandboxStart(instance, summaries)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"instance":        instance,
		"instance_status": status,
		"admitted":        true,
	}, nil
}

// AdmitLaunchLocal lets the privileged launcher evaluate the same inventory
// and budget rules while it holds the lifecycle lock. It is deliberately
// read-only: a successful check is not a reservation once that lock is gone.
// The launcher must use this local path because the executor serves requests
// serially and cannot call itself through the supervisor during provisioning.
func AdmitLaunchLocal(instance string) error {
	_, err := admitLaunch(instance)
	return err
}

// sandboxBudgetFromConfig reads the declared budget from the instance
// catalog config. Instances created before the budget field default to
// the protocol default so their next start stays admissible; only a
// non-numeric value, one below the floor, or one above the arithmetic
// safety bound is invalid — there is no fixed product cap (idiom 4): the
// executor enforces the host-derived cap where /proc/meminfo is read.
func sandboxBudgetFromConfig(config map[string]string) (int64, error) {
	raw, present := config[budgetMetadataKey()]
	if !present || raw == "" {
		return protocol.DefaultSandboxBudgetMemoryMiB, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || !protocol.ValidMemoryBudget(value) {
		return 0, protoError("unsafe_state", "sandbox budget metadata is invalid")
	}
	return value, nil
}

// sandboxResourceBudget is one resource's recorded (declared) and enforced
// view. The record and the enforced value are written together by chasm; a
// sandbox changed behind chasm's back is in drift and admission counts the
// larger of the two (idiom 1).
type sandboxResourceBudget struct {
	declared            int64
	declaredPresent     bool
	enforced            int64
	enforcedPresent     bool
	enforcedUnsupported string // reason the enforced value could not be parsed; empty when parsed or absent
}

func (budget sandboxResourceBudget) drifted() bool {
	return budget.declaredPresent && budget.enforcedPresent && budget.declared != budget.enforced
}

// effective returns the value admission counts: the larger of declared and
// enforced, and the fallback only when neither side is present.
func (budget sandboxResourceBudget) effective(fallback int64) int64 {
	switch {
	case budget.declaredPresent && budget.enforcedPresent:
		if budget.declared > budget.enforced {
			return budget.declared
		}
		return budget.enforced
	case budget.declaredPresent:
		return budget.declared
	case budget.enforcedPresent:
		return budget.enforced
	default:
		return fallback
	}
}

// sandboxMemoryBudget derives a sandbox's declared and enforced memory
// budget in MiB from one inventory record: declared is the
// user.<prefix>.budget-memory-mib key (absent when unset), enforced is the
// expanded limits.memory parsed to MiB (absent when not set, e.g. when the
// inventory carries no expanded config). An unparseable enforced value
// fails closed.
func sandboxMemoryBudget(summary incusInstanceSummary) (sandboxResourceBudget, error) {
	var budget sandboxResourceBudget
	if raw := summary.Config[budgetMetadataKey()]; raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || !protocol.ValidMemoryBudget(value) {
			return budget, protoError("unsafe_state", "sandbox budget metadata is invalid")
		}
		budget.declared, budget.declaredPresent = value, true
	}
	limit, present := summary.ExpandedConfig["limits.memory"]
	if !present || limit == "" {
		return budget, nil
	}
	limitBytes, err := parseMemoryLimitBytes(limit)
	if err != nil {
		// The enforced value stays explicitly unsupported: admission fails
		// closed on it, while show and resize report it as an unknown with
		// this reason instead of dropping the resource (idiom 2).
		budget.enforcedUnsupported = fmt.Sprintf("limits.memory '%s' is not a supported size", limit)
		return budget, nil
	}
	budget.enforced = (limitBytes + (1 << 20) - 1) >> 20
	if budget.enforced < 1 || budget.enforced > protocol.SafetyBoundSandboxBudgetMemoryMiB {
		// The same bound the shell admission applies: only a positive
		// integer of at most 1048576 MiB can be accounted, so anything
		// else stays an explicit unsupported value (idiom 2).
		budget.enforcedUnsupported = fmt.Sprintf("limits.memory '%s' parses outside the supported 1..%d MiB range", limit, protocol.SafetyBoundSandboxBudgetMemoryMiB)
		budget.enforced, budget.enforcedPresent = 0, false
		return budget, nil
	}
	budget.enforcedPresent = true
	return budget, nil
}

// sandboxCpuBudget derives a sandbox's declared and enforced CPU budget.
// Enforced is the expanded limits.cpu: a plain integer count; a range or
// list form ("0-3", "0,2") is its CPU count; anything else fails closed.
func sandboxCpuBudget(summary incusInstanceSummary) (sandboxResourceBudget, error) {
	var budget sandboxResourceBudget
	if raw := summary.Config[cpuBudgetMetadataKey()]; raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || !protocol.ValidCpuBudget(value) {
			return budget, protoError("unsafe_state", "sandbox CPU budget metadata is invalid")
		}
		budget.declared, budget.declaredPresent = value, true
	}
	limit, present := summary.ExpandedConfig["limits.cpu"]
	if !present || limit == "" {
		return budget, nil
	}
	count, err := parseCpuLimitCount(limit)
	if err != nil {
		budget.enforcedUnsupported = fmt.Sprintf("limits.cpu '%s' is not a supported CPU count", limit)
		return budget, nil
	}
	budget.enforced, budget.enforcedPresent = count, true
	return budget, nil
}

// parseCpuLimitCount reads one limits.cpu value as a CPU count: a plain
// integer is the count itself; "a-b" spans b-a+1 CPUs; "a,b,c" lists
// them. Anything else is unparseable.
func parseCpuLimitCount(limit string) (int64, error) {
	limit = strings.TrimSpace(limit)
	if value, err := strconv.ParseInt(limit, 10, 64); err == nil {
		if value < 1 {
			return 0, fmt.Errorf("CPU limit %q is not a positive count", limit)
		}
		return value, nil
	}
	if hyphen := strings.IndexByte(limit, '-'); hyphen > 0 {
		low, lowErr := strconv.ParseInt(strings.TrimSpace(limit[:hyphen]), 10, 64)
		high, highErr := strconv.ParseInt(strings.TrimSpace(limit[hyphen+1:]), 10, 64)
		if lowErr == nil && highErr == nil && low >= 0 && high >= low {
			return high - low + 1, nil
		}
		return 0, fmt.Errorf("CPU limit %q is not a supported range", limit)
	}
	items := strings.Split(limit, ",")
	for _, item := range items {
		value, err := strconv.ParseInt(strings.TrimSpace(item), 10, 64)
		if err != nil || value < 0 {
			return 0, fmt.Errorf("CPU limit %q is not a supported list", limit)
		}
	}
	return int64(len(items)), nil
}

// parseMemoryLimitBytes reads one limits.memory value as bytes, accepting
// exactly the spellings Incus accepts for byte sizes. The grammar is:
//
//	size    := bytes | binary | decimal
//	bytes   := digits                          (a plain count of bytes)
//	binary  := digits ( 'KiB' | 'MiB' | 'GiB' | 'TiB')  (units of 1024)
//	decimal := digits ( 'kB' | 'KB' | 'MB' | 'GB' | 'TB') (units of 1000)
//	digits  := '0' | [1-9][0-9]*                (no sign, no leading zeros)
//
// Anything else — a percentage, a bare suffix like "3g", a signed value
// ("-4", "+4"), a leading-zero spelling ("04GiB", "007"), an empty value —
// is unsupported. The shell reader incus_size_to_mib accepts exactly the
// same set.
func parseMemoryLimitBytes(limit string) (int64, error) {
	if canonicalDigits(limit) {
		if value, err := strconv.ParseInt(limit, 10, 64); err == nil {
			return value, nil
		}
	}
	units := []struct {
		suffix string
		factor int64
	}{
		{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10},
		{"TB", 1000 * 1000 * 1000 * 1000}, {"GB", 1000 * 1000 * 1000},
		{"MB", 1000 * 1000}, {"kB", 1000}, {"KB", 1000},
	}
	for _, unit := range units {
		if !strings.HasSuffix(limit, unit.suffix) {
			continue
		}
		digits := strings.TrimSuffix(limit, unit.suffix)
		if digits == "" || !canonicalDigits(digits) {
			break
		}
		amount, err := strconv.ParseInt(digits, 10, 64)
		if err != nil || amount > ((1<<63)-1)/unit.factor {
			break
		}
		return amount * unit.factor, nil
	}
	return 0, fmt.Errorf("%q is not a supported size", limit)
}

// canonicalDigits reports whether s is exactly one digit grammar token of
// the shared byte-size contract: '0' or a positive number with no sign and
// no leading zero ("007" and "04GiB" are refused, as is any signed value).
func canonicalDigits(s string) bool {
	if s == "" || (len(s) > 1 && s[0] == '0') {
		return false
	}
	return allDigits(s)
}

// admissibleMemoryBudget returns the memory MiB admission counts for one
// sandbox: the larger of declared and enforced (idiom 1), falling back to
// the keyless-legacy protocol default only when neither is observable.
func admissibleMemoryBudget(summary incusInstanceSummary) (int64, error) {
	budget, err := sandboxMemoryBudget(summary)
	if err != nil {
		return 0, err
	}
	if budget.enforcedUnsupported != "" {
		return 0, protoError("unsafe_state", budget.enforcedUnsupported)
	}
	return budget.effective(protocol.DefaultSandboxBudgetMemoryMiB), nil
}

type instanceStateReader func() (map[string]interface{}, error)
type instanceStopper func() (int, error)

func stopGuest() (map[string]interface{}, error) {
	instance, err := guestInstance()
	if err != nil {
		return nil, err
	}
	return stopSandbox(instance, nil)
}

func stopSandbox(instance string, timings lifecycleTimings) (map[string]interface{}, error) {
	return withLifecycleLock(func() (map[string]interface{}, error) {
		if err := requireManagedSandbox(instance); err != nil {
			return nil, err
		}
		return stopSandboxTimed(instance, func() (map[string]interface{}, error) {
			return instanceStateFor(instance)
		}, func() (int, error) {
			// Best effort: a stale or failing catalog scan must not
			// wedge the sandbox lifecycle; stop proceeds and the next
			// successful scan refreshes the catalog.
			_, _ = scanSandbox(instance)
			endShutdown := timings.phase("shutdown_request")
			defer endShutdown()
			return incusStopInstance(instance)
		}, timings)
	})
}

// incusStopInstance stops one instance gracefully and falls back to a forced
// stop when the graceful timeout is exceeded (a first-boot VM may miss
// the graceful window). The postcondition check afterwards still requires a
// proven Stopped state, so a failed forced stop still fails closed.
func incusStopInstance(instance string) (int, error) {
	code, err := incusExec([]string{"stop", instance, "--timeout", "60"}, nil, nil, nil, 90*time.Second)
	if err == nil && code == 0 {
		return code, nil
	}
	return incusExec([]string{"stop", instance, "--force"}, nil, nil, nil, 90*time.Second)
}

func withLifecycleLock(operation func() (map[string]interface{}, error)) (map[string]interface{}, error) {
	return withLifecycleLockAt(lifecycleLockPath, 0, operation)
}

func withLifecycleLockAt(path string, expectedUID uint32, operation func() (map[string]interface{}, error)) (map[string]interface{}, error) {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CREAT|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, protoError("unsafe_state", "sandbox lifecycle lock is unavailable")
	}
	defer syscall.Close(fd)
	var info syscall.Stat_t
	if err := syscall.Fstat(fd, &info); err != nil || info.Mode&syscall.S_IFMT != syscall.S_IFREG || info.Uid != expectedUID || info.Mode&0o777 != 0o600 {
		return nil, protoError("unsafe_state", "sandbox lifecycle lock metadata is unsafe")
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		return nil, protoError("unsafe_state", "sandbox lifecycle lock could not be acquired")
	}
	defer syscall.Flock(fd, syscall.LOCK_UN)
	return operation()
}

func stopSandboxTimed(instance string, readState instanceStateReader, stop instanceStopper, timings lifecycleTimings) (map[string]interface{}, error) {
	if !protocol.ValidSandboxInstance(instance) {
		return nil, protoError("invalid_instance", "instance is not permitted")
	}
	before, err := readState()
	if err != nil {
		return nil, protoError("invalid_state", "sandbox instance state is unavailable")
	}
	status, _ := before["status"].(string)
	stopped := false
	switch status {
	case "Running":
		code, err := stop()
		if err != nil || code != 0 {
			return nil, protoError("invalid_state", "sandbox instance could not be stopped")
		}
		stopped = true
	case "Stopped":
	default:
		return nil, protoError("invalid_state", "sandbox instance is neither running nor stopped")
	}
	endConfirmation := timings.phase("stopped_confirmation")
	confirmed, err := readState()
	endConfirmation()
	if err != nil {
		return nil, protoError("postcondition_failed", "sandbox instance state is inconsistent")
	}
	pid, pidOK := numericInt64(confirmed["pid"])
	if confirmed["status"] != "Stopped" || !pidOK || pid != 0 {
		return nil, protoError("postcondition_failed", "sandbox instance did not reach the stopped state")
	}
	return map[string]interface{}{
		"instance":        instance,
		"instance_status": "Stopped",
		"instance_pid":    int64(0),
		"stopped":         stopped,
	}, nil
}

func pauseSandbox(instance string) (map[string]interface{}, error) {
	return withLifecycleLock(func() (map[string]interface{}, error) {
		if err := requireManagedSandbox(instance); err != nil {
			return nil, err
		}
		before, err := instanceStateFor(instance)
		if err != nil {
			return nil, protoError("invalid_state", "sandbox instance state is unavailable")
		}
		status, _ := before["status"].(string)
		if status != "Running" && status != "Frozen" {
			return nil, protoError("invalid_state", "sandbox instance must be running to pause")
		}
		if status == "Running" {
			code, err := incusExec([]string{"pause", instance}, nil, nil, nil, 90*time.Second)
			if err != nil || code != 0 {
				return nil, protoError("invalid_state", "sandbox instance could not be paused")
			}
		}
		confirmed, err := instanceStateFor(instance)
		if err != nil || confirmed["status"] != "Frozen" {
			return nil, protoError("postcondition_failed", "sandbox instance did not reach the paused state")
		}
		return map[string]interface{}{"instance": instance, "instance_status": "Frozen", "paused": status == "Running"}, nil
	})
}

func resumeSandbox(instance string) (map[string]interface{}, error) {
	return withLifecycleLock(func() (map[string]interface{}, error) {
		if err := requireManagedSandbox(instance); err != nil {
			return nil, err
		}
		return resumeSandboxWith(instance, func() (map[string]interface{}, error) {
			return instanceStateFor(instance)
		}, func() (int, error) {
			return incusExec([]string{"resume", instance}, nil, nil, nil, 90*time.Second)
		}, requestGuestTimeResync)
	})
}

// resumeSandboxWith is the pure resume path: it validates the instance,
// requires the Frozen state, resumes it, confirms the Running state, and
// reports the best-effort guest time resync outcome without ever failing
// the resume over it.
type instanceResumer func() (int, error)
type guestTimeResyncer func(string) string

func resumeSandboxWith(instance string, readState instanceStateReader, resume instanceResumer, timeResync guestTimeResyncer) (map[string]interface{}, error) {
	if !protocol.ValidSandboxInstance(instance) {
		return nil, protoError("invalid_instance", "instance is not permitted")
	}
	before, err := readState()
	if err != nil {
		return nil, protoError("invalid_state", "sandbox instance state is unavailable")
	}
	if status, _ := before["status"].(string); status != "Frozen" {
		return nil, protoError("invalid_state", "sandbox instance must be paused to resume")
	}
	if code, err := resume(); err != nil || code != 0 {
		return nil, protoError("invalid_state", "sandbox instance could not be resumed")
	}
	confirmed, err := readState()
	if err != nil || confirmed["status"] != "Running" {
		return nil, protoError("postcondition_failed", "sandbox instance did not reach the running state")
	}
	return map[string]interface{}{
		"instance":        instance,
		"instance_status": "Running",
		"resumed":         true,
		"time_resync":     timeResync(instance),
	}, nil
}

// guestTimeResyncTimeout bounds the in-guest time resync issued after a
// lifecycle transition; it must never delay a start or resume noticeably.
const guestTimeResyncTimeout = 15 * time.Second

// requestGuestTimeResync issues one bounded, best-effort NTP resync inside
// the guest after a lifecycle transition that can leave the guest clock
// stale: a frozen VM keeps its pre-pause wall clock, and long pauses have
// left resumed guests hours behind the host with no active NTP service,
// breaking timestamps and TLS until time was corrected by hand. The command
// runs as root inside the guest, touches no user data, and its outcome is
// informational only: "requested" when the resync was issued successfully,
// "failed" when it was not — never an error that fails the caller.
func requestGuestTimeResync(instance string) string {
	code, err := runIncus([]string{"exec", instance, "--", "systemctl", "restart", "systemd-timesyncd"}, nil, nil, nil, guestTimeResyncTimeout)
	if err != nil || code != 0 {
		return "failed"
	}
	return "requested"
}

func numericInt64(value interface{}) (int64, bool) {
	switch number := value.(type) {
	case float64:
		parsed := int64(number)
		return parsed, float64(parsed) == number
	case int64:
		return number, true
	case int:
		return int64(number), true
	case json.Number:
		parsed, err := number.Int64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

// guestInstance resolves the default sandbox instance the supervisor
// workflow addresses. The site must declare default_sandbox; an empty value
// fails closed naming the missing key.
func guestInstance() (string, error) {
	instance := siteconfig.Active().DefaultInstanceName()
	if instance == "" {
		return "", protoError("invalid_state", "site config default_sandbox is required for this operation")
	}
	return instance, nil
}

// --- handlers ---

func inspectDefaultSandbox() (map[string]interface{}, error) {
	instance, err := guestInstance()
	if err != nil {
		return nil, err
	}
	state, err := instanceStateFor(instance)
	if err != nil {
		return nil, err
	}
	services := []map[string]interface{}{}
	for _, name := range serviceNamesFor(siteconfig.Active()) {
		sample, err := systemdProperties(name)
		if err != nil {
			return nil, err
		}
		normalized, err := normalizedServiceSummary(sample)
		if err != nil {
			return nil, protoError("unsafe_state", err.Error())
		}
		services = append(services, normalized)
	}
	result := map[string]interface{}{
		"instance":        instance,
		"instance_status": state["status"],
		"instance_pid":    state["pid"],
		"services":        services,
	}
	// Idiom 4's host view rides along with the default sandbox's status:
	// the host line and pool-fill warnings are shown with or without a
	// named sandbox. The state call above is reused so the instance still
	// costs one /state call per show.
	if status, ok := state["status"].(string); ok {
		if pid, ok := numericInt64(state["pid"]); ok {
			attachResources(result, func() (map[string]interface{}, error) {
				views, err := sandboxShowViews(instance, status, pid, state)
				if err != nil {
					return nil, err
				}
				return map[string]interface{}{
					"host": views["host"],
					"pool": views["pool"],
				}, nil
			})
		}
	}
	attachLifecycleObservations(result, instance)
	return result, nil
}

// inspectSandbox is the seam for the full sandbox inspection; tests inject
// a stub so the adopt/re-create wiring runs without a live socket.
var inspectSandbox = func(instance string) (map[string]interface{}, error) {
	metadata, err := loadSandboxMetadata(instance)
	if err != nil {
		return nil, err
	}
	description, err := sandboxDescriptionFromMetadata(metadata)
	if err != nil {
		return nil, err
	}
	state, err := instanceStateFor(instance)
	if err != nil {
		return nil, protoError("invalid_state", "sandbox instance state is unavailable")
	}
	status, statusOK := state["status"].(string)
	pid, pidOK := numericInt64(state["pid"])
	if !statusOK || !pidOK || pid < 0 || (status != "Running" && status != "Frozen" && status != "Stopped") ||
		(status == "Stopped" && pid != 0) || (status == "Running" && pid == 0) {
		return nil, protoError("unsafe_state", "sandbox instance state is invalid")
	}
	result := map[string]interface{}{
		"instance":        instance,
		"logical_name":    logicalSandboxName(instance),
		"instance_status": status,
		"instance_pid":    pid,
		"description":     description,
	}
	if config, ok := metadata["config"].(map[string]interface{}); ok {
		stringConfig := make(map[string]string, len(config))
		for key, value := range config {
			if text, ok := value.(string); ok {
				stringConfig[key] = text
			}
		}
		budget, err := sandboxBudgetFromConfig(stringConfig)
		if err != nil {
			return nil, err
		}
		result["budget_memory_mib"] = budget
	}
	result["workspace"] = workspacePath
	result["shared"] = sharedPath
	// The full resource view set is informational, but it is never
	// dropped silently: when the collector cannot assemble it (for
	// example a pool that is unreachable), the Resources section still
	// appears carrying the reason (idiom 2).
	attachResources(result, func() (map[string]interface{}, error) {
		return sandboxShowViews(instance, status, pid, state)
	})
	attachLifecycleObservations(result, instance)
	return result, nil
}

// attachResources puts the collected resource views on the result. When
// the collector fails, the Resources section still appears — carrying the
// reason — so a failing collector never hides the section silently.
func attachResources(result map[string]interface{}, collect func() (map[string]interface{}, error)) {
	views, err := collect()
	if err != nil {
		var protocolErr *ProtocolError
		if errors.As(err, &protocolErr) {
			err = fmt.Errorf("%s: %s", protocolErr.Code, protocolErr.Message)
		}
		result["resources"] = map[string]interface{}{"unavailable": err.Error()}
		return
	}
	result["resources"] = views
}

func listSandboxes() (map[string]interface{}, error) {
	instances, err := listSandboxInstances()
	if err != nil {
		return nil, err
	}
	sort.Slice(instances, func(left, right int) bool { return instances[left].Name < instances[right].Name })
	items := make([]map[string]interface{}, 0, len(instances))
	for _, instance := range instances {
		item := map[string]interface{}{
			"instance":        instance.Name,
			"logical_name":    logicalSandboxName(instance.Name),
			"instance_status": instance.Status,
			"instance_pid":    instance.PID,
			"description":     instance.Description,
			"workspace":       workspacePath,
			"shared":          sharedPath,
		}
		items = append(items, item)
	}
	return map[string]interface{}{"sandboxes": items}, nil
}

func describeSandbox(instance string) (map[string]interface{}, error) {
	metadata, err := loadSandboxMetadata(instance)
	if err != nil {
		return nil, err
	}
	description, err := sandboxDescriptionFromMetadata(metadata)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"instance": instance, "logical_name": logicalSandboxName(instance), "description": description,
	}, nil
}

type sandboxMetadataLoader func(string) (map[string]interface{}, error)
type sandboxDescriptionMutator func(string, string) error

func setSandboxDescription(instance, description string) (map[string]interface{}, error) {
	return setSandboxDescriptionWith(instance, description, loadSandboxMetadata, mutateSandboxDescription)
}

func setSandboxDescriptionWith(instance, description string, load sandboxMetadataLoader, mutate sandboxDescriptionMutator) (map[string]interface{}, error) {
	if !protocol.ValidSandboxInstance(instance) {
		return nil, protoError("invalid_instance", "instance is not permitted")
	}
	if !protocol.ValidSandboxDescription(description) {
		return nil, protoError("invalid_description", "description must be one trimmed printable line of at most 120 characters")
	}
	metadata, err := load(instance)
	if err != nil {
		return nil, err
	}
	current, err := sandboxDescriptionFromMetadata(metadata)
	if err != nil {
		return nil, err
	}
	changed := current != description
	if changed {
		if err := mutate(instance, description); err != nil {
			return nil, err
		}
		observed, err := load(instance)
		if err != nil {
			return nil, err
		}
		actual, err := sandboxDescriptionFromMetadata(observed)
		if err != nil || actual != description {
			return nil, protoError("postcondition_failed", "sandbox description update was not proven")
		}
	}
	return map[string]interface{}{
		"instance": instance, "logical_name": logicalSandboxName(instance), "description": description, "changed": changed,
	}, nil
}

func mutateSandboxDescription(instance, description string) error {
	arguments := []string{"config", "unset", instance, descriptionKey()}
	if description != "" {
		arguments = []string{"config", "set", instance, descriptionKey(), description}
	}
	code, err := incusExec(arguments, nil, nil, nil, 30*time.Second)
	if err != nil || code != 0 {
		return protoError("invalid_state", "sandbox description could not be updated")
	}
	return nil
}

func sandboxDescriptionFromMetadata(metadata map[string]interface{}) (string, error) {
	return sandboxDescriptionFor(siteconfig.Active(), metadata)
}

func sandboxDescriptionFor(site siteconfig.Site, metadata map[string]interface{}) (string, error) {
	config, ok := metadata["config"].(map[string]interface{})
	if !ok {
		return "", protoError("unsafe_state", "sandbox metadata is invalid")
	}
	raw, present := config[site.MetadataKey("description")]
	if !present {
		return "", nil
	}
	description, ok := raw.(string)
	if !ok || !protocol.ValidSandboxDescription(description) {
		return "", protoError("unsafe_state", "sandbox description metadata is invalid")
	}
	return description, nil
}

func loadSandboxMetadata(instance string) (map[string]interface{}, error) {
	if !protocol.ValidSandboxInstance(instance) {
		return nil, protoError("invalid_instance", "instance is not permitted")
	}
	metadata, err := incusQueryGET("/1.0/instances/" + instance)
	if err != nil {
		return nil, protoError("invalid_state", "managed sandbox metadata is unavailable")
	}
	if err := ValidateManagedSandboxMetadata(instance, metadata); err != nil {
		return nil, err
	}
	return metadata, nil
}

// ValidateManagedSandboxMetadata applies the same ownership checks to a
// sandbox read by the lifecycle executor or the dedicated exec helper.
func ValidateManagedSandboxMetadata(instance string, metadata map[string]interface{}) error {
	return ValidateManagedSandboxMetadataFor(siteconfig.Active(), instance, metadata)
}

// ValidateManagedSandboxMetadataFor validates ownership using an explicit site.
func ValidateManagedSandboxMetadataFor(site siteconfig.Site, instance string, metadata map[string]interface{}) error {
	config, ok := metadata["config"].(map[string]interface{})
	if !ok {
		return protoError("unsafe_state", "sandbox metadata is invalid")
	}
	if _, err := sandboxDescriptionFor(site, metadata); err != nil {
		return err
	}
	devices, devicesOK := metadata["devices"].(map[string]interface{})
	if config[site.MetadataKey("managed")] != "true" ||
		config[site.MetadataKey("logical-name")] != logicalSandboxName(instance) ||
		config[site.MetadataKey("workspace-volume")] != site.WorkspaceVolumeName(instance) ||
		config[site.MetadataKey("shared-volume")] != site.SharedVolumeName() || !devicesOK ||
		!managedUntypedDiskConfigValid(devices["shared"], site.StoragePool, site.SharedVolumeName(), sharedPath) ||
		!managedUntypedDiskConfigValid(devices["workspace"], site.StoragePool, site.WorkspaceVolumeName(instance), workspacePath) {
		return protoError("unsafe_state", "managed sandbox metadata is invalid")
	}
	return nil
}

// requireManagedSandbox is the seam for the managed-sandbox metadata check.
var requireManagedSandbox = func(instance string) error {
	_, err := loadSandboxMetadata(instance)
	return err
}

func managedDiskConfigValid(device map[string]string, pool, source, path string) bool {
	return device["type"] == "disk" && device["pool"] == pool && device["source"] == source &&
		device["path"] == path && device["readonly"] == "false"
}

func managedUntypedDiskConfigValid(raw interface{}, pool, source, path string) bool {
	device, ok := raw.(map[string]interface{})
	return ok && device["type"] == "disk" && device["pool"] == pool && device["source"] == source &&
		device["path"] == path && device["readonly"] == "false"
}

// Dispatch is the Go mirror of host_control dispatch().
func Dispatch(request protocol.Request) (map[string]interface{}, error) {
	return dispatchWithLifecycle(request, dispatch, requireManagedSandbox, saveLifecycleObservation, os.Stderr)
}

func dispatchWithLifecycle(request protocol.Request, execute Dispatcher, managed func(string) error,
	save func(string, uint32, string, lifecycleObservation) error, log io.Writer) (map[string]interface{}, error) {
	operation := lifecycleOperation(request.Operation)
	if operation == "" {
		return execute(request)
	}
	if request.LifecycleTimings == nil {
		request.LifecycleTimings = make(map[string]int64)
	}
	timings := lifecycleTimings(request.LifecycleTimings)
	end := timings.phase("total")
	result, err := execute(request)
	end()
	observation := lifecycleObservation{Operation: operation, RequestID: request.RequestID(), ObservedAt: time.Now().UTC(), OK: err == nil, Timings: timings}
	if protocol.ValidSandboxInstance(request.Instance()) && managed(request.Instance()) == nil {
		if saveErr := save(lifecycleRoot(), 0, request.Instance(), observation); saveErr != nil {
			fmt.Fprintf(log, "chasm executor: lifecycle timing persistence failed for %s: %v\n", request.Instance(), saveErr)
		}
	}
	return result, err
}

func dispatch(request protocol.Request) (map[string]interface{}, error) {
	timings := lifecycleTimings(request.LifecycleTimings)
	switch request.Operation {
	case "inspect_default_sandbox":
		return inspectDefaultSandbox()
	case "create_sandbox":
		budget := int64(0)
		if value, ok := request.NumberField("budget_memory_mib"); ok {
			budget = value
		}
		cpu := int64(0)
		if value, ok := request.NumberField("budget_cpu"); ok {
			cpu = value
		}
		return createSandbox(request.Instance(), budget, cpu, timings)
	case "describe_sandbox":
		return describeSandbox(request.Instance())
	case "set_sandbox_description":
		return setSandboxDescription(request.Instance(), request.StringField("description"))
	case "list_sandboxes":
		return listSandboxes()
	case "inspect_sandbox":
		return inspectSandbox(request.Instance())
	case "sandbox_layout":
		depth, _ := request.NumberField("depth")
		return sandboxLayout(request.Instance(), int(depth))
	case "list_repositories":
		return listRepositories()
	case "locate_repository":
		return locateRepository(request.StringField("repository"))
	case "ensure_guest_running":
		return ensureSandboxRunning(request.Instance(), request.RequestID(), timings)
	case "stop_guest":
		return stopSandbox(request.Instance(), timings)
	case "pause_guest":
		return pauseSandbox(request.Instance())
	case "resize_sandbox":
		return resizeSandbox(request.Instance(), resizeTargetsFromRequest(request))
	case "resume_guest":
		return resumeSandbox(request.Instance())
	case "admit_launch":
		return admitLaunch(request.Instance())
	case "delete_sandbox":
		return deleteSandbox(request)
	case "collect_performance":
		return collectPerformance(request.StringField("phase"))
	default:
		return nil, protoError("unknown_operation", "operation is not permitted")
	}
}

// resizeTargetsFromRequest maps a resize request's optional fields onto
// the targets. Mutation checked: mapping one field into another, or
// dropping root_disk_mib, fails resizeTargetsFromRequest tests.
func resizeTargetsFromRequest(request protocol.Request) resizeTargets {
	var targets resizeTargets
	if value, ok := request.NumberField("budget_memory_mib"); ok {
		targets.memory = value
	}
	if value, ok := request.NumberField("budget_cpu"); ok {
		targets.cpu = value
	}
	if value, ok := request.NumberField("root_disk_mib"); ok {
		targets.rootDisk = value
	}
	return targets
}

// --- helpers ---

var (
	randReader = rand.Reader
)

func splitLines(text string) []string {
	return strings.FieldsFunc(text, func(r rune) bool { return r == '\n' || r == '\r' })
}

func contains(items []string, needle string) bool {
	for _, item := range items {
		if item == needle {
			return true
		}
	}
	return false
}

func allHex(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

func randomHex(bytesCount int) string {
	raw := make([]byte, bytesCount)
	if _, err := io.ReadFull(randReader, raw); err != nil {
		return ""
	}
	return hex.EncodeToString(raw)
}

func sanitizeID(value string) string {
	re := regexp.MustCompile(`[^A-Za-z0-9._-]`)
	return re.ReplaceAllString(value, "_")
}

func copyMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
