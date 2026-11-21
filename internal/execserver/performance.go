package execserver

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rahulrajaram/chasm/internal/hostidentity"
	"github.com/rahulrajaram/chasm/internal/siteconfig"
)

// The bounded host performance sample collected by the collect_performance
// operation.

var (
	statusFields  = stringSet("VmRSS", "VmHWM", "VmSize", "VmSwap", "VmData", "RssAnon", "RssFile", "Threads")
	smapsFields   = stringSet("Rss", "Pss", "Pss_Anon", "Pss_File", "Pss_Shmem", "Private_Clean", "Private_Dirty", "Shared_Clean", "Shared_Dirty", "Anonymous", "Swap")
	ioFields      = stringSet("read_bytes", "write_bytes")
	meminfoFields = stringSet("MemTotal", "MemFree", "MemAvailable", "Cached", "Buffers", "SwapTotal", "SwapFree", "AnonPages", "Mapped", "Shmem", "Slab", "SReclaimable", "SUnreclaim", "PageTables", "KernelStack")
	vmstatFields  = stringSet("pswpin", "pswpout", "pgfault", "pgmajfault", "oom_kill", "compact_stall")
)

var componentServiceRoles = []struct {
	service string
	role    string
}{
	{service: "chasm-supervisor.service", role: "sandbox-supervisor"},
	{service: "chasm-executor.service", role: "collector"},
}

func stringSet(items ...string) map[string]bool {
	set := make(map[string]bool, len(items))
	for _, item := range items {
		set[item] = true
	}
	return set
}

// numericKibRecords mirrors _numeric_kib_records: name <colon> value[ kB]
// lines, only admitted names, integer first token.
func numericKibRecords(path string, admitted map[string]bool) (map[string]interface{}, error) {
	records := map[string]interface{}{}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%s is unreadable", path)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		name, rest, found := strings.Cut(line, ":")
		if !found || !admitted[name] {
			continue
		}
		pieces := strings.Fields(rest)
		if len(pieces) == 0 || !allDigits(pieces[0]) {
			return nil, fmt.Errorf("%s has a non-numeric %s value", path, name)
		}
		value, _ := strconv.ParseInt(pieces[0], 10, 64)
		records[name] = value
	}
	return records, scanner.Err()
}

// plainNumericRecords mirrors _plain_numeric_records: "name value" lines.
func plainNumericRecords(path string, admitted map[string]bool) (map[string]interface{}, error) {
	records := map[string]interface{}{}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%s is unreadable", path)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		pieces := strings.Fields(scanner.Text())
		if len(pieces) == 2 && admitted[pieces[0]] {
			value, err := strconv.ParseInt(pieces[1], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("%s has a non-numeric %s value", path, pieces[0])
			}
			records[pieces[0]] = value
		}
	}
	return records, scanner.Err()
}

type psiClass struct {
	kind   string
	values map[string]interface{}
}

// psiRecords mirrors _psi_records: "some avg10=... avg60=... avg300=... total=..."
// (total is an integer, the rest floats).
func psiRecords(path string) ([]psiClass, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer file.Close()
	var classes []psiClass
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		kind := fields[0]
		values := map[string]interface{}{}
		for _, field := range fields[1:] {
			name, value, found := strings.Cut(field, "=")
			if !found {
				return nil, fmt.Errorf("%s has a malformed PSI field", path)
			}
			if name == "total" {
				parsed, err := strconv.ParseInt(value, 10, 64)
				if err != nil {
					return nil, fmt.Errorf("%s has a malformed PSI value", path)
				}
				values[name] = parsed
			} else {
				parsed, err := strconv.ParseFloat(value, 64)
				if err != nil {
					return nil, fmt.Errorf("%s has a malformed PSI value", path)
				}
				values[name] = parsed
			}
		}
		classes = append(classes, psiClass{kind: kind, values: values})
	}
	return classes, scanner.Err()
}

func readFileText(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

func processStarttime(pid int64) (int64, error) {
	_, _, starttime, err := processCPUCounters(fmt.Sprintf("/proc/%d/stat", pid))
	return starttime, err
}

func processCPUCounters(path string) (int64, int64, int64, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, 0, err
	}
	text := string(raw)
	end := strings.LastIndex(text, ")")
	if end < 0 {
		return 0, 0, 0, errors.New("process has malformed stat data")
	}
	tail := strings.Fields(text[end+2:])
	if len(tail) < 20 {
		return 0, 0, 0, errors.New("process has malformed stat data")
	}
	user, userErr := strconv.ParseInt(tail[11], 10, 64)
	system, systemErr := strconv.ParseInt(tail[12], 10, 64)
	starttime, startErr := strconv.ParseInt(tail[19], 10, 64)
	if userErr != nil || systemErr != nil || startErr != nil || user < 0 || system < 0 || starttime <= 0 {
		return 0, 0, 0, errors.New("process has malformed stat counters")
	}
	return user, system, starttime, nil
}

func directoryEntryCount(path string) (int64, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return 0, err
	}
	return int64(len(entries)), nil
}

// processSample mirrors _process_sample; identity is re-checked afterwards.
func processSample(role string, pid int64) (map[string]interface{}, error) {
	processRoot := fmt.Sprintf("/proc/%d", pid)
	if pid <= 0 {
		return nil, fmt.Errorf("%s process is unavailable", role)
	}
	if info, err := os.Stat(processRoot); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("%s process is unavailable", role)
	}
	link, err := os.Readlink(filepath.Join(processRoot, "exe"))
	if err != nil {
		return nil, err
	}
	executable := filepath.Base(link)
	userTicks, systemTicks, starttime, err := processCPUCounters(filepath.Join(processRoot, "stat"))
	if err != nil {
		return nil, err
	}
	status, err := numericKibRecords(filepath.Join(processRoot, "status"), statusFields)
	if err != nil {
		return nil, err
	}
	smaps, err := numericKibRecords(filepath.Join(processRoot, "smaps_rollup"), smapsFields)
	if err != nil {
		return nil, err
	}
	ioCounters, err := numericKibRecords(filepath.Join(processRoot, "io"), ioFields)
	if err != nil {
		return nil, err
	}
	fdCount, err := directoryEntryCount(filepath.Join(processRoot, "fd"))
	if err != nil {
		return nil, err
	}
	taskCount, err := directoryEntryCount(filepath.Join(processRoot, "task"))
	if err != nil {
		return nil, err
	}
	sample := map[string]interface{}{
		"role":             role,
		"pid":              pid,
		"executable":       executable,
		"starttime_ticks":  starttime,
		"cpu_user_ticks":   userTicks,
		"cpu_system_ticks": systemTicks,
		"io":               sortedPairs(ioCounters),
		"status":           sortedPairs(status),
		"smaps_rollup":     sortedPairs(smaps),
		"fd_count":         fdCount,
		"task_count":       taskCount,
	}
	verified, err := processStarttime(pid)
	if err != nil || verified != starttime {
		return nil, fmt.Errorf("%s process identity changed during sampling", role)
	}
	return sample, nil
}

func parseIECBytes(value string) (int64, error) {
	units := map[string]int64{"B": 1, "KiB": 1024, "MiB": 1024 * 1024, "GiB": 1024 * 1024 * 1024}
	for _, unit := range []string{"GiB", "MiB", "KiB", "B"} {
		if !strings.HasSuffix(value, unit) {
			continue
		}
		digits := strings.TrimSuffix(value, unit)
		if digits == "" || !allDigits(digits) || digits[0] == '0' {
			break
		}
		amount, err := strconv.ParseInt(digits, 10, 64)
		if err != nil || amount > (1<<63-1)/units[unit] {
			break
		}
		return amount * units[unit], nil
	}
	return 0, errors.New("memory limit is not a supported exact IEC byte value")
}

func instanceMemoryLimitBytes() (int64, error) {
	instance, err := guestInstance()
	if err != nil {
		return 0, err
	}
	metadata, err := incusQueryGET("/1.0/instances/" + instance)
	if err != nil {
		return 0, err
	}
	expanded, ok := metadata["expanded_config"].(map[string]interface{})
	if !ok {
		return 0, errors.New("expanded instance configuration is unavailable")
	}
	limit, ok := expanded["limits.memory"].(string)
	if !ok {
		return 0, errors.New("instance memory limit is unavailable")
	}
	return parseIECBytes(limit)
}

func parseUnifiedCgroup(raw string) (string, error) {
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "0::/") {
		return "", errors.New("process unified cgroup is malformed")
	}
	path := strings.TrimPrefix(lines[0], "0::")
	if filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\r\n") {
		return "", errors.New("process unified cgroup is unsafe")
	}
	return path, nil
}

func readNonnegativeInteger(path string) (int64, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	value := strings.TrimSpace(string(raw))
	if !allDigits(value) {
		return 0, errors.New("numeric resource counter is malformed")
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		return 0, errors.New("numeric resource counter is out of range")
	}
	return parsed, nil
}

func processCgroupMemory(pid int64) (map[string]interface{}, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return nil, err
	}
	relative, err := parseUnifiedCgroup(string(raw))
	if err != nil {
		return nil, err
	}
	root := filepath.Join("/sys/fs/cgroup", strings.TrimPrefix(relative, "/"))
	current, err := readNonnegativeInteger(filepath.Join(root, "memory.current"))
	if err != nil {
		return nil, err
	}
	peak, err := readNonnegativeInteger(filepath.Join(root, "memory.peak"))
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"current_bytes": current, "peak_bytes": peak}, nil
}

func statvfs(path string) (map[string]interface{}, error) {
	var info syscallStatfs
	if err := syscallStatfsFn(path, &info); err != nil {
		return nil, err
	}
	// statvfs blocks * frsize.
	bytesTotal := int64(info.Blocks) * int64(info.Frsize)
	bytesFree := int64(info.Bavail) * int64(info.Frsize)
	return map[string]interface{}{
		"bytes_total":                 bytesTotal,
		"bytes_free_for_unprivileged": bytesFree,
		"inodes_total":                int64(info.Files),
		"inodes_free":                 int64(info.Ffree),
	}, nil
}

func sortedPairs(records map[string]interface{}) [][]interface{} {
	keys := make([]string, 0, len(records))
	for key := range records {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	pairs := make([][]interface{}, 0, len(keys))
	for _, key := range keys {
		pairs = append(pairs, []interface{}{key, records[key]})
	}
	return pairs
}

func allDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func loadAverages() (float64, float64, float64, error) {
	raw, err := readFileText("/proc/loadavg")
	if err != nil {
		return 0, 0, 0, err
	}
	fields := strings.Fields(raw)
	if len(fields) < 3 {
		return 0, 0, 0, errors.New("load averages are malformed")
	}
	one, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, 0, 0, err
	}
	five, err := strconv.ParseFloat(fields[1], 64)
	if err != nil {
		return 0, 0, 0, err
	}
	fifteen, err := strconv.ParseFloat(fields[2], 64)
	if err != nil {
		return 0, 0, 0, err
	}
	return one, five, fifteen, nil
}

// collectPerformance mirrors collect_sample(phase).
func collectPerformance(phase string) (map[string]interface{}, error) {
	instance, err := guestInstance()
	if err != nil {
		return nil, err
	}
	state, err := instanceStateFor(instance)
	if err != nil {
		return nil, protoError("unsafe_state", "sandbox instance state is unavailable")
	}
	status, _ := state["status"].(string)
	if status != "Running" {
		return nil, protoError("unsafe_state", instance+" is not running")
	}
	qemuPID := int64(0)
	if raw, ok := state["pid"].(float64); ok {
		qemuPID = int64(raw)
	}

	services := []map[string]interface{}{}
	var incusMainPID int64
	for _, name := range serviceNamesFor(siteconfig.Active()) {
		sample, err := systemdProperties(name)
		if err != nil {
			return nil, err
		}
		normalized, err := normalizedPerformanceService(sample)
		if err != nil {
			return nil, protoError("unsafe_state", err.Error())
		}
		services = append(services, normalized)
		if name == "incus.service" {
			incusMainPID = sample["MainPID"].(int64)
		}
	}
	if incusMainPID <= 0 {
		return nil, protoError("unsafe_state", "incus.service has no main PID")
	}

	one, five, fifteen, err := loadAverages()
	if err != nil {
		return nil, protoError("unsafe_state", "load averages are unreadable")
	}
	bootID, err := readFileText("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return nil, protoError("unsafe_state", "boot identity is unreadable")
	}
	meminfo, err := numericKibRecords("/proc/meminfo", meminfoFields)
	if err != nil {
		return nil, protoError("unsafe_state", "meminfo is unreadable")
	}
	vmstat, err := plainNumericRecords("/proc/vmstat", vmstatFields)
	if err != nil {
		return nil, protoError("unsafe_state", "vmstat is unreadable")
	}
	memPsi, err := psiRecords("/proc/pressure/memory")
	if err != nil {
		return nil, protoError("unsafe_state", "memory PSI is unreadable")
	}
	ioPsi, err := psiRecords("/proc/pressure/io")
	if err != nil {
		return nil, protoError("unsafe_state", "io PSI is unreadable")
	}

	incusd, err := processSample("incusd", incusMainPID)
	if err != nil {
		return nil, protoError("unsafe_state", err.Error())
	}
	qemu, err := processSample("qemu", qemuPID)
	if err != nil {
		return nil, protoError("unsafe_state", err.Error())
	}
	componentProcesses := []map[string]interface{}{}
	for _, component := range componentServiceRoles {
		properties, err := systemdProperties(component.service)
		if err != nil {
			return nil, err
		}
		mainPID, ok := properties["MainPID"].(int64)
		if !ok || mainPID <= 0 || properties["ActiveState"] != "active" || properties["Result"] != "success" {
			return nil, protoError("unsafe_state", component.service+" is unavailable")
		}
		sample, err := processSample(component.role, mainPID)
		if err != nil {
			return nil, protoError("unsafe_state", err.Error())
		}
		componentProcesses = append(componentProcesses, sample)
	}
	ceilingBytes, err := instanceMemoryLimitBytes()
	if err != nil {
		return nil, protoError("unsafe_state", "instance memory ceiling is unavailable")
	}
	incusCgroupMemory, err := processCgroupMemory(incusMainPID)
	if err != nil {
		return nil, protoError("unsafe_state", "Incus cgroup memory is unavailable")
	}

	uptimeRaw, err := readFileText("/proc/uptime")
	if err != nil {
		return nil, protoError("unsafe_state", "uptime is unreadable")
	}
	uptimeFields := strings.Fields(uptimeRaw)
	if len(uptimeFields) == 0 {
		return nil, protoError("unsafe_state", "uptime is malformed")
	}
	uptime, err := strconv.ParseFloat(uptimeFields[0], 64)
	if err != nil {
		return nil, protoError("unsafe_state", "uptime is malformed")
	}
	rootFilesystem, err := statvfs("/")
	if err != nil {
		return nil, protoError("unsafe_state", "root filesystem statistics are unavailable")
	}

	digest := sha256Sum(bootID)
	payload := map[string]interface{}{
		"schema_version": 1,
		"collected_at":   time.Now().UTC().Format(time.RFC3339Nano),
		"phase":          phase,
		"boot_id_sha256": digest,
		"system": map[string]interface{}{
			"uptime_seconds":  uptime,
			"load_1m":         one,
			"load_5m":         five,
			"load_15m":        fifteen,
			"meminfo_kib":     meminfo,
			"vmstat":          vmstat,
			"memory_psi":      psiMap(memPsi),
			"io_psi":          psiMap(ioPsi),
			"root_filesystem": rootFilesystem,
		},
		"services": services,
		"instance": map[string]interface{}{
			"name":               instance,
			"status":             status,
			"pid":                qemuPID,
			"started_at":         state["started_at"],
			"process_count":      state["processes"],
			"memory":             state["memory"],
			"memory_limit_bytes": ceilingBytes,
		},
		"processes":           []map[string]interface{}{incusd, qemu},
		"component_processes": componentProcesses,
		"incus_cgroup_memory": incusCgroupMemory,
	}
	// The wire corpora carry the pinned site identity fingerprint; when it
	// cannot be resolved (marker missing, machine drifted), the key is
	// omitted rather than fabricated.
	if fingerprint, err := hostidentity.SiteFingerprint(); err == nil {
		payload["host_fingerprint"] = fingerprint
	}
	return payload, nil
}

func psiMap(classes []psiClass) map[string]interface{} {
	out := map[string]interface{}{}
	for _, class := range classes {
		out[class.kind] = sortedPairs(class.values)
	}
	return out
}

type syscallStatfs = syscall.Statfs_t

func syscallStatfsFn(path string, info *syscall.Statfs_t) error {
	return syscall.Statfs(path, info)
}

func sha256Sum(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
