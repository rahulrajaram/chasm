package doctor

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rahulrajaram/chasm/internal/siteconfig"
)

func TestDoctorRootCommandOutputIsBounded(t *testing.T) {
	var output boundedDoctorOutput
	payload := bytes.Repeat([]byte("x"), 128<<10)
	if n, err := output.Write(payload); err != nil || n != len(payload) || output.Len() != 64<<10 {
		t.Fatalf("captured %d bytes, write %d, error %v", output.Len(), n, err)
	}
}

type fakeInfo struct {
	mode os.FileMode
	gid  uint32
}

func (f fakeInfo) Name() string       { return "" }
func (f fakeInfo) Size() int64        { return 0 }
func (f fakeInfo) Mode() os.FileMode  { return f.mode }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return false }
func (f fakeInfo) Sys() any           { return &syscall.Stat_t{Gid: f.gid} }

func diagnosticFixture(t *testing.T) (siteconfig.Site, probes, *[]string) {
	t.Helper()
	site := siteconfig.Site{StoragePool: "pool", DefaultSandbox: "demo", SupervisorUser: "sv", OperatorUser: "op", ResourcePrefix: "chasm"}
	calls := []string{}
	p := probes{
		read: func(path string) ([]byte, error) {
			switch path {
			case "/etc/os-release":
				return []byte("PRETTY_NAME=\"Example Linux\"\n"), nil
			case "/proc/sys/kernel/osrelease":
				return []byte("6.12\n"), nil
			case "/sys/module/kvm_intel/parameters/nested":
				return []byte("N\n"), nil
			default:
				return nil, os.ErrNotExist
			}
		},
		stat: func(path string) (os.FileInfo, error) {
			switch path {
			case "/dev/kvm":
				return fakeInfo{mode: os.ModeDevice | os.ModeCharDevice | 0660}, nil
			case "/usr/lib/virtiofsd", "/usr/lib/incus/incus-agent", provision + "/scripts/build-sandbox-template.sh":
				return fakeInfo{mode: 0755}, nil
			default:
				return nil, os.ErrNotExist
			}
		},
		lstat: func(path string) (os.FileInfo, error) {
			switch path {
			case siteconfig.ExecutorSocket:
				return fakeInfo{mode: os.ModeSocket | 0660, gid: 10}, nil
			case siteconfig.SupervisorSocket:
				return fakeInfo{mode: os.ModeSocket | 0600, gid: 11}, nil
			case "/run/incus/unix.socket":
				return fakeInfo{mode: os.ModeSocket | 0660}, nil
			case "/run/incus", "/run", "/":
				return fakeInfo{mode: os.ModeDir | 0755}, nil
			default:
				t.Fatalf("unexpected lstat %q", path)
				return nil, os.ErrNotExist
			}
		},
		group: func(group string) (uint32, error) {
			if group == "sv" {
				return 10, nil
			}
			return 11, nil
		},
		command: func(program string, args, settings []string) (string, error) {
			switch program {
			case "/usr/bin/systemctl":
				if len(args) != 3 || args[0] != "show" || len(settings) != 0 {
					t.Fatalf("non-observational systemctl call %v %v", args, settings)
				}
				return "LoadState=loaded\nActiveState=active\nSubState=running", nil
			case "/bin/sh":
				if !reflect.DeepEqual(args, []string{"-c", templateCheck, provision + "/scripts/build-sandbox-template.sh", "--check"}) ||
					!reflect.DeepEqual(settings, []string{"INCUS_SOCKET=/run/incus/unix.socket", "INCUS_CONF=/dev/null"}) {
					t.Fatalf("unsafe template probe arguments or environment")
				}
				return "template build: no current sealed template for profile minimal", errors.New("exit 1")
			default:
				t.Fatalf("unexpected command %q", program)
				return "", nil
			}
		},
		host:   func() error { return nil },
		socket: func() (string, error) { return "/run/incus/unix.socket", nil },
		observe: func(socket, path string) (map[string]any, error) {
			if socket != "/run/incus/unix.socket" {
				t.Fatalf("unexpected socket %q", socket)
			}
			calls = append(calls, path)
			instance := site.DefaultInstanceName()
			switch path {
			case "/1.0":
				return map[string]any{"environment": map[string]any{"server_version": "6.0.0"}}, nil
			case "/1.0/storage-pools/pool/resources":
				return map[string]any{"space": map[string]any{"total": 1e9, "used": 0.95e9}}, nil
			case "/1.0/instances/sandbox-demo":
				return map[string]any{
					"type": "virtual-machine", "status": "Running",
					"config": map[string]any{
						site.MetadataKey("managed"): "true", site.MetadataKey("initialized"): "true",
						site.MetadataKey("logical-name"):     "demo",
						site.MetadataKey("workspace-volume"): site.WorkspaceVolumeName(instance),
						site.MetadataKey("shared-volume"):    site.SharedVolumeName(),
					},
					"devices": map[string]any{
						"workspace": map[string]any{"type": "disk", "pool": "pool", "source": site.WorkspaceVolumeName(instance), "path": "/workspace", "readonly": "false"},
						"shared":    map[string]any{"type": "disk", "pool": "pool", "source": site.SharedVolumeName(), "path": "/shared", "readonly": "false"},
					},
				}, nil
			case "/1.0/instances/sandbox-demo/state":
				return map[string]any{"status": "Running", "processes": float64(50), "pid": float64(123)}, nil
			default:
				t.Fatalf("unexpected Incus GET %q", path)
				return nil, nil
			}
		},
	}
	return site, p, &calls
}

func assertStatus(t *testing.T, checks []Check, id, status string) {
	t.Helper()
	for _, check := range checks {
		if check.ID == id {
			if check.Status != status || check.Source == "" || check.Summary == "" {
				t.Fatalf("check = %+v, want status %q and evidence", check, status)
			}
			return
		}
	}
	t.Fatalf("missing check %q", id)
}

func TestDoctorObservesOnlyRunningServiceAndManagedSandbox(t *testing.T) {
	site, p, calls := diagnosticFixture(t)
	checks := collect(site, p)
	for id, status := range map[string]string{
		"distribution": "ok", "kernel": "ok", "virtiofsd": "ok", "incus-agent": "ok",
		"executor_socket": "ok", "supervisor_socket": "fail", "incus_reachability": "ok",
		"incus_version": "ok", "pool_headroom": "warn", "template_freshness": "warn",
		"sandbox_readiness": "ok",
	} {
		assertStatus(t, checks, id, status)
	}
	if len(*calls) != 4 {
		t.Fatalf("observed paths = %v", *calls)
	}

	observe := p.observe
	p.observe = func(socket, path string) (map[string]any, error) {
		response, err := observe(socket, path)
		if path == "/1.0/instances/sandbox-demo" {
			response["config"].(map[string]any)[site.MetadataKey("logical-name")] = "wrong"
		}
		return response, err
	}
	*calls = nil
	assertStatus(t, collect(site, p), "sandbox_readiness", "fail")
	if len(*calls) != 3 {
		t.Fatalf("unsafe VM readiness probed: %v", *calls)
	}
}

func TestDoctorSkipsIncusWithoutActiveDaemonOrHostPin(t *testing.T) {
	for _, condition := range []string{"inactive", "wrong-host"} {
		t.Run(condition, func(t *testing.T) {
			site, p, calls := diagnosticFixture(t)
			command := p.command
			if condition == "inactive" {
				p.command = func(program string, args, settings []string) (string, error) {
					if program == "/bin/sh" {
						t.Fatal("template checker would contact Incus")
					}
					if args[2] == "incus.service" {
						return "LoadState=loaded\nActiveState=inactive\nSubState=dead", nil
					}
					return command(program, args, settings)
				}
			} else {
				p.host = func() error { return errors.New("host pin differs") }
			}
			checks := collect(site, p)
			for _, id := range []string{"incus_reachability", "incus_version", "pool_headroom", "template_freshness", "sandbox_readiness"} {
				assertStatus(t, checks, id, "skipped")
			}
			if len(*calls) != 0 {
				t.Fatalf("observed paths = %v", *calls)
			}
		})
	}
}

func TestDoctorStoppedAndUninitializedAreDistinct(t *testing.T) {
	for _, initialized := range []bool{true, false} {
		site, p, calls := diagnosticFixture(t)
		query := p.observe
		p.observe = func(socket, path string) (map[string]any, error) {
			response, err := query(socket, path)
			if filepath.Base(path) == "sandbox-demo" {
				response["status"] = "Stopped"
				if !initialized {
					delete(response["config"].(map[string]any), site.MetadataKey("initialized"))
				}
			}
			return response, err
		}
		status := "warn"
		if initialized {
			status = "skipped"
		}
		assertStatus(t, collect(site, p), "sandbox_readiness", status)
		if strings.Contains(strings.Join(*calls, " "), "/state") {
			t.Fatalf("nonrunning guest state was probed: %v", *calls)
		}
	}
}
