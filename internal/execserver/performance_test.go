package execserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSortedPairsMatchesPythonTupleEncoding(t *testing.T) {
	pairs := sortedPairs(map[string]interface{}{"Pss": int64(2), "Anonymous": int64(1)})
	want := [][]interface{}{{"Anonymous", int64(1)}, {"Pss", int64(2)}}
	if !reflect.DeepEqual(pairs, want) {
		t.Fatalf("sorted pairs differ: got %#v want %#v", pairs, want)
	}
	encoded, err := json.Marshal(pairs)
	if err != nil {
		t.Fatalf("marshal pairs: %v", err)
	}
	if string(encoded) != `[["Anonymous",1],["Pss",2]]` {
		t.Fatalf("wrong wire encoding: %s", encoded)
	}
}

func TestServiceNormalizationMatchesPythonShapes(t *testing.T) {
	raw := map[string]interface{}{
		"name":           "incus.service",
		"ActiveState":    "active",
		"SubState":       "running",
		"Result":         "success",
		"MainPID":        int64(896),
		"NRestarts":      int64(0),
		"ExecMainStatus": int64(0),
	}
	summary, err := normalizedServiceSummary(raw)
	if err != nil {
		t.Fatalf("normalize summary: %v", err)
	}
	wantSummary := map[string]interface{}{
		"name": "incus.service", "active_state": "active",
		"result": "success", "n_restarts": int64(0),
	}
	if !reflect.DeepEqual(summary, wantSummary) {
		t.Fatalf("wrong summary: got %#v want %#v", summary, wantSummary)
	}
	performance, err := normalizedPerformanceService(raw)
	if err != nil {
		t.Fatalf("normalize performance service: %v", err)
	}
	for _, key := range []string{
		"name", "active_state", "sub_state", "result", "main_pid",
		"n_restarts", "exec_main_status",
	} {
		if _, ok := performance[key]; !ok {
			t.Fatalf("normalized performance service is missing %s", key)
		}
	}
	for _, forbidden := range []string{"ActiveState", "NRestarts", "MainPID"} {
		if _, ok := performance[forbidden]; ok {
			t.Fatalf("normalized performance service retained %s", forbidden)
		}
	}
}

func TestPerformanceParsersMatchFrozenPythonOracle(t *testing.T) {
	directory := t.TempDir()
	statusPath := filepath.Join(directory, "status")
	if err := os.WriteFile(statusPath, []byte("VmRSS:\t120 kB\nName:\tincusd\nThreads:\t22\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err := numericKibRecords(statusPath, stringSet("VmRSS", "Threads"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sortedPairs(status), [][]interface{}{{"Threads", int64(22)}, {"VmRSS", int64(120)}}) {
		t.Fatalf("status=%v", status)
	}

	vmstatPath := filepath.Join(directory, "vmstat")
	if err := os.WriteFile(vmstatPath, []byte("pgfault 42\npswpin 0\nunrelated 9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	vmstat, err := plainNumericRecords(vmstatPath, stringSet("pgfault", "pswpin"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(vmstat, map[string]interface{}{"pgfault": int64(42), "pswpin": int64(0)}) {
		t.Fatalf("vmstat=%v", vmstat)
	}

	ioPath := filepath.Join(directory, "io")
	if err := os.WriteFile(ioPath, []byte("rchar: 99\nread_bytes: 4096\nwrite_bytes: 8192\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ioCounters, err := numericKibRecords(ioPath, ioFields)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ioCounters, map[string]interface{}{"read_bytes": int64(4096), "write_bytes": int64(8192)}) {
		t.Fatalf("io=%v", ioCounters)
	}

	psiPath := filepath.Join(directory, "memory")
	if err := os.WriteFile(psiPath, []byte("some avg10=0.10 avg60=0.20 avg300=0.30 total=40\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	psi, err := psiRecords(psiPath)
	if err != nil {
		t.Fatal(err)
	}
	actual := psiMap(psi)
	if !reflect.DeepEqual(actual["some"], [][]interface{}{{"avg10", 0.1}, {"avg300", 0.3}, {"avg60", 0.2}, {"total", int64(40)}}) {
		t.Fatalf("psi=%v", actual)
	}
}

func TestPerformanceParsersRejectMalformedAdmittedValues(t *testing.T) {
	directory := t.TempDir()
	vmstatPath := filepath.Join(directory, "vmstat")
	if err := os.WriteFile(vmstatPath, []byte("pgfault not-a-number\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := plainNumericRecords(vmstatPath, stringSet("pgfault")); err == nil {
		t.Fatal("malformed vmstat was admitted")
	}
	psiPath := filepath.Join(directory, "memory")
	if err := os.WriteFile(psiPath, []byte("some avg10=not-a-number total=40\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := psiRecords(psiPath); err == nil {
		t.Fatal("malformed PSI was admitted")
	}
}

func TestProcessCountersMemoryLimitAndCgroupParsersAreClosed(t *testing.T) {
	directory := t.TempDir()
	statPath := filepath.Join(directory, "stat")
	stat := "123 (worker name) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21 22\n"
	if err := os.WriteFile(statPath, []byte(stat), 0o600); err != nil {
		t.Fatal(err)
	}
	user, system, starttime, err := processCPUCounters(statPath)
	if err != nil || user != 11 || system != 12 || starttime != 19 {
		t.Fatalf("counters=(%d,%d,%d) error=%v", user, system, starttime, err)
	}
	for input, expected := range map[string]int64{
		"1B": 1, "32KiB": 32 * 1024, "64MiB": 64 * 1024 * 1024, "3GiB": 3 * 1024 * 1024 * 1024,
	} {
		actual, err := parseIECBytes(input)
		if err != nil || actual != expected {
			t.Fatalf("parseIECBytes(%q)=(%d,%v), want %d", input, actual, err, expected)
		}
	}
	for _, input := range []string{"", "0GiB", "3GB", "3.5GiB", "-1GiB", "03GiB"} {
		if _, err := parseIECBytes(input); err == nil {
			t.Fatalf("unsafe limit admitted: %q", input)
		}
	}
	if actual, err := parseUnifiedCgroup("0::/system.slice/incus.service\n"); err != nil || actual != "/system.slice/incus.service" {
		t.Fatalf("cgroup=(%q,%v)", actual, err)
	}
	for _, input := range []string{"", "1:memory:/x", "0::/x/../y", "0::relative", "0::/x\n0::/y"} {
		if _, err := parseUnifiedCgroup(input); err == nil {
			t.Fatalf("unsafe cgroup admitted: %q", input)
		}
	}
}
