package execserver

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rahulrajaram/chasm/internal/protocol"
)

type lifecycleTimings map[string]int64

func (timings lifecycleTimings) phase(name string) func() {
	if timings == nil {
		return func() {}
	}
	start := time.Now()
	return func() { timings[name] = time.Since(start).Milliseconds() }
}

type lifecycleObservation struct {
	Operation  string           `json:"operation"`
	RequestID  string           `json:"request_id"`
	ObservedAt time.Time        `json:"observed_at"`
	OK         bool             `json:"ok"`
	Timings    lifecycleTimings `json:"phase_timings_ms"`
}

func lifecycleOperation(operation string) string {
	switch operation {
	case "create_sandbox":
		return "create"
	case "ensure_guest_running":
		return "start"
	case "stop_guest":
		return "stop"
	default:
		return ""
	}
}

var lifecycleOperations = [...]string{"create", "start", "stop"}

func lifecycleRoot() string { return filepath.Join(auditRoot, "performance") }

func lifecyclePath(root, instance, operation string) string {
	return filepath.Join(root, instance+"."+operation+".json")
}

func runInstrumentedLauncher(command *exec.Cmd, timings lifecycleTimings) error {
	reader, writer, err := os.Pipe()
	if err != nil {
		return err
	}
	defer reader.Close()
	command.ExtraFiles = []*os.File{writer}
	command.Env = append(command.Env, "CHASM_LIFECYCLE_TIMING=1")
	if err := command.Start(); err != nil {
		_ = writer.Close()
		return err
	}
	_ = writer.Close()
	err = command.Wait()
	// The launcher emits at most ten small records. Read after it exits to
	// prevent an inherited descriptor from extending the launcher's timeout.
	_ = reader.SetReadDeadline(time.Now().Add(time.Second))
	const max = 1024
	raw, readErr := io.ReadAll(io.LimitReader(reader, max+1))
	if readErr == nil && len(raw) <= max {
		parseLauncherTimings(string(raw), timings)
	}
	return err
}

func parseLauncherTimings(raw string, timings lifecycleTimings) {
	starts := map[string]int64{}
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		phase := fields[0]
		switch phase {
		case "clone", "boot", "guest_agent_ready", "shutdown_request", "stopped_confirmation":
		default:
			continue
		}
		seconds, fraction, ok := strings.Cut(fields[2], ".")
		if !ok || len(fraction) != 2 {
			continue
		}
		whole, err := strconv.ParseUint(seconds, 10, 48)
		centiseconds, fractionErr := strconv.ParseUint(fraction, 10, 8)
		if err != nil || fractionErr != nil || centiseconds > 99 {
			continue
		}
		millis := int64(whole*1000 + centiseconds*10)
		switch fields[1] {
		case "begin":
			starts[phase] = millis
		case "end":
			if start, exists := starts[phase]; exists && millis >= start {
				timings[phase] = millis - start
				delete(starts, phase)
			}
		}
	}
}

func saveLifecycleObservation(root string, uid uint32, instance string, observation lifecycleObservation) error {
	if !protocol.ValidSandboxInstance(instance) {
		return errors.New("lifecycle instance invalid")
	}
	if err := ensureOutcomeDirectory(root, uid); err != nil {
		return err
	}
	file, err := os.CreateTemp(root, ".lifecycle-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	err = json.NewEncoder(file).Encode(observation)
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(file.Name(), lifecyclePath(root, instance, observation.Operation)); err != nil {
		return err
	}
	return syncDir(root)
}

func loadLifecycleObservations(root string, uid uint32, instance string) ([]lifecycleObservation, error) {
	if !protocol.ValidSandboxInstance(instance) {
		return nil, errors.New("lifecycle instance invalid")
	}
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 || !ownedBy(info, uid) {
		return nil, errors.New("lifecycle directory metadata unsafe")
	}
	var observations []lifecycleObservation
	for _, op := range lifecycleOperations {
		path := lifecyclePath(root, instance, op)
		file, err := openPrivateOutcome(path, uid)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		const max = 8192
		raw, err := io.ReadAll(io.LimitReader(file, max+1))
		_ = file.Close()
		var observation lifecycleObservation
		if err != nil || len(raw) > max || json.Unmarshal(raw, &observation) != nil ||
			observation.Operation != op || observation.ObservedAt.IsZero() || len(observation.Timings) == 0 {
			return nil, errors.New("lifecycle observation invalid")
		}
		observations = append(observations, observation)
	}
	return observations, nil
}

func attachLifecycleObservations(result map[string]interface{}, instance string) {
	observations, err := loadLifecycleObservations(lifecycleRoot(), 0, instance)
	if err != nil {
		result["lifecycle_observations_unavailable"] = true
	} else if len(observations) > 0 {
		result["lifecycle_observations"] = observations
	}
}

func removeLifecycleObservations(root string, uid uint32, instance string) error {
	if !protocol.ValidSandboxInstance(instance) {
		return errors.New("lifecycle instance invalid")
	}
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 || !ownedBy(info, uid) {
		return errors.New("lifecycle directory metadata unsafe")
	}
	for _, op := range lifecycleOperations {
		if err := os.Remove(lifecyclePath(root, instance, op)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return syncDir(root)
}
