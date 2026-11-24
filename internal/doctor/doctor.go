// Package doctor observes installed host and sandbox configuration.
package doctor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rahulrajaram/chasm/internal/execserver"
	"github.com/rahulrajaram/chasm/internal/guestexec"
	"github.com/rahulrajaram/chasm/internal/hostidentity"
	"github.com/rahulrajaram/chasm/internal/incuslocal"
	"github.com/rahulrajaram/chasm/internal/siteconfig"
)

type Check struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Summary string `json:"summary"`
	Source  string `json:"source"`
	Remedy  string `json:"remedy,omitempty"`
}

type Report struct {
	Version int     `json:"version"`
	Checks  []Check `json:"checks"`
}

const (
	provision   = "/usr/local/lib/chasm/provision"
	fixedPath   = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	siteFix     = "Check /etc/chasm/site.conf against the installed host-config/site.conf.example."
	agentFix    = "Install the distro's Incus agent package (incus-agent where packaged separately)."
	serviceFix  = "Inspect systemctl status and journalctl -u for this unit, then repair or start the unit."
	incusFix    = "Check incus.service and the local socket configured in /etc/chasm/incus-socket."
	templateFix = "Run sudo " + provision + "/scripts/build-sandbox-template.sh after reviewing the template prerequisites."
)

type probes struct {
	read    func(string) ([]byte, error)
	stat    func(string) (os.FileInfo, error)
	lstat   func(string) (os.FileInfo, error)
	group   func(string) (uint32, error)
	command func(string, []string, []string) (string, error)
	host    func() error
	socket  func() (string, error)
	observe func(string, string) (map[string]any, error)
}

func Collect(site siteconfig.Site) []Check {
	return collect(site, probes{
		read: os.ReadFile, stat: os.Stat, lstat: os.Lstat, host: hostidentity.RequireSiteHost,
		group: func(name string) (uint32, error) {
			group, err := user.LookupGroup(name)
			if err != nil {
				return 0, err
			}
			gid, err := strconv.ParseUint(group.Gid, 10, 32)
			return uint32(gid), err
		},
		socket: incuslocal.ConfiguredSocketPath,
		command: func(program string, args, settings []string) (string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, program, args...)
			cmd.Env = append([]string{"PATH=" + fixedPath, "LANG=C", "LC_ALL=C", "HOME=/dev/null"}, settings...)
			cmd.Dir = "/"
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			cmd.Cancel = func() error {
				err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				if errors.Is(err, syscall.ESRCH) {
					return os.ErrProcessDone
				}
				return err
			}
			cmd.WaitDelay = time.Second
			var output boundedDoctorOutput
			cmd.Stdout, cmd.Stderr = &output, &output
			err := cmd.Run()
			return strings.TrimSpace(output.String()), err
		},
		observe: func(socket, path string) (map[string]any, error) {
			return guestexec.NewClient(socket).Observe(context.Background(), path)
		},
	})
}

func Descriptor(err error) Check {
	if err != nil {
		return Check{"site_descriptor", "fail", err.Error(), siteconfig.DefaultPath, siteFix}
	}
	return Check{"site_descriptor", "ok", "Installed site descriptor validated", siteconfig.DefaultPath, ""}
}

func collect(site siteconfig.Site, p probes) []Check {
	var checks []Check
	add := func(id, status, summary, source, remedy string) {
		checks = append(checks, Check{id, status, summary, source, remedy})
	}
	distro := ""
	for _, path := range []string{"/etc/os-release", "/usr/lib/os-release"} {
		if raw, err := p.read(path); err == nil {
			for _, line := range strings.Split(string(raw), "\n") {
				if value, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
					distro = strings.Trim(value, "\"'")
					break
				}
			}
		}
		if distro != "" {
			break
		}
	}
	if distro == "" {
		add("distribution", "skipped", "Distribution identification unavailable", "os-release", "Check /etc/os-release.")
	} else {
		add("distribution", "ok", distro, "os-release", "")
	}
	kernel, err := p.read("/proc/sys/kernel/osrelease")
	if err != nil {
		add("kernel", "skipped", "Kernel release unavailable", "/proc/sys/kernel/osrelease", "Verify procfs is mounted.")
	} else {
		add("kernel", "ok", strings.TrimSpace(string(kernel)), "/proc/sys/kernel/osrelease", "")
	}
	if runtime.GOOS == "linux" && runtime.GOARCH == siteconfig.SupportedArchitecture {
		add("architecture", "ok", runtime.GOOS+"/"+runtime.GOARCH, "Chasm binary", "")
	} else {
		add("architecture", "fail", runtime.GOOS+"/"+runtime.GOARCH, "Chasm binary", "Run Chasm on Linux amd64.")
	}
	add("go_build_version", "ok", runtime.Version(), "Chasm build information", "")
	for _, component := range []struct {
		id, remedy string
		extra      []string
	}{
		{"virtiofsd", "Install the distro's virtiofsd package.", []string{"/usr/libexec/virtiofsd", "/usr/lib/virtiofsd", "/usr/lib/qemu/virtiofsd"}},
		{"incus-agent", agentFix, []string{"/usr/libexec/incus/incus-agent", "/usr/lib/incus/incus-agent", "/usr/libexec/incus-agent"}},
	} {
		var candidates []string
		for _, dir := range strings.Split(fixedPath, ":") {
			candidates = append(candidates, dir+"/"+component.id)
		}
		candidates = append(candidates, component.extra...)
		path := ""
		for _, candidate := range candidates {
			if info, err := p.stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
				path = candidate
				break
			}
		}
		if path == "" {
			add(component.id, "fail", "Required host executable missing", "Host executable search paths", component.remedy)
		} else {
			add(component.id, "ok", path, "Host executable search paths", "")
		}
	}
	checkKVM(p, add)
	checkSELinux(p, add)
	hostOK := p.host() == nil
	if hostOK {
		add("host_identity", "ok", "Machine matches the installed identity pin", siteconfig.MarkerPath, "")
	} else {
		add("host_identity", "fail", "Machine identity pin could not be verified", siteconfig.MarkerPath, "Verify this is the provisioned host and inspect the identity marker before reprovisioning.")
	}
	incusActive := false
	for _, unit := range []string{"incus.service", "chasm-firewall.service", "chasm-executor.service", "chasm-executor.socket", "chasm-supervisor.service", "chasm-supervisor.socket"} {
		out, err := p.command("/usr/bin/systemctl", []string{"show", "--property=LoadState,ActiveState,SubState", unit}, nil)
		properties := map[string]string{}
		for _, line := range strings.Split(out, "\n") {
			if k, v, ok := strings.Cut(line, "="); ok {
				properties[k] = v
			}
		}
		status, summary, remedy := "ok", properties["ActiveState"]+" ("+properties["SubState"]+")", ""
		switch {
		case properties["LoadState"] == "not-found":
			status, summary, remedy = "fail", "Unit is missing", "Install or upgrade the Chasm control plane and its host prerequisites."
		case err != nil || properties["LoadState"] == "" || properties["ActiveState"] == "":
			status, summary, remedy = "skipped", "Systemd state unavailable", "Check that the system systemd manager is reachable."
		case properties["LoadState"] != "loaded":
			status, summary, remedy = "fail", properties["LoadState"], serviceFix
		case unit == "incus.service":
			incusActive = properties["ActiveState"] == "active" && properties["SubState"] == "running"
			if !incusActive {
				status, remedy = "fail", serviceFix
			}
		case strings.HasSuffix(unit, ".socket"):
			if properties["ActiveState"] != "active" || properties["SubState"] != "listening" && properties["SubState"] != "running" {
				status, remedy = "fail", serviceFix
			}
		case properties["ActiveState"] == "active":
		case properties["ActiveState"] == "inactive" && (unit == "chasm-executor.service" || unit == "chasm-supervisor.service"):
			status, summary = "skipped", "Socket-activated service currently idle"
		default:
			status, remedy = "fail", serviceFix
		}
		add(unit, status, summary, "systemctl show "+unit, remedy)
	}
	for _, entry := range []struct{ id, path, group string }{
		{"executor_socket", siteconfig.ExecutorSocket, site.SupervisorUser},
		{"supervisor_socket", siteconfig.SupervisorSocket, site.OperatorUser},
	} {
		info, err := p.lstat(entry.path)
		gid, groupErr := p.group(entry.group)
		if err != nil || info.Mode()&os.ModeSocket == 0 {
			add(entry.id, "fail", "Unix socket unavailable", entry.path, "Inspect the matching systemd socket unit.")
		} else if owner, ok := info.Sys().(*syscall.Stat_t); !ok || groupErr != nil || owner.Uid != 0 || owner.Gid != gid || info.Mode().Perm() != 0660 {
			add(entry.id, "fail", "Socket must be root:"+entry.group+" mode 0660", entry.path, "Correct SocketUser, SocketGroup, and SocketMode in the matching systemd socket unit.")
		} else {
			add(entry.id, "ok", "Unix socket present with expected ownership and mode", entry.path, "")
		}
	}
	socket, err := p.socket()
	if err != nil {
		add("incus_socket", "fail", err.Error(), incuslocal.SocketConfigPath, incusFix)
	} else {
		add("incus_socket", "ok", socket, incuslocal.SocketConfigPath+" or standard local socket paths", "")
	}
	online := false
	if !incusActive || !hostOK || err != nil {
		add("incus_reachability", "skipped", "Probe requires an active Incus daemon, a verified site host and a local socket", "Local Incus API /1.0", incusFix)
		add("incus_version", "skipped", "Daemon version unavailable", "Local Incus API /1.0", incusFix)
	} else if metadata, err := p.observe(socket, "/1.0"); err != nil {
		add("incus_reachability", "fail", "Local Incus API did not respond", socket, incusFix)
		add("incus_version", "skipped", "Daemon version unavailable", "Local Incus API /1.0", incusFix)
	} else {
		online = true
		add("incus_reachability", "ok", "Local Incus API responded", socket, "")
		environment, _ := metadata["environment"].(map[string]any)
		if version, ok := environment["server_version"].(string); ok && version != "" {
			add("incus_version", "ok", version, "Local Incus API /1.0", "")
		} else {
			add("incus_version", "skipped", "Daemon did not report a version", "Local Incus API /1.0", incusFix)
		}
	}
	if !online {
		add("template_freshness", "skipped", "Requires a reachable local Incus API", provision, incusFix)
		add("pool_headroom", "skipped", "Requires a reachable local Incus API", site.StoragePool, incusFix)
		add("sandbox_readiness", "skipped", "Requires a reachable local Incus API", "Default sandbox", incusFix)
		return checks
	}
	checkIncus(site, socket, p, add)
	return checks
}

type addCheck func(id, status, summary, source, remedy string)

func checkKVM(p probes, add addCheck) {
	const nestingFix = "Install the matching kvm-intel-no-nesting.conf or kvm-amd-no-nesting.conf under /etc/modprobe.d and reboot the host."
	if info, err := p.stat("/dev/kvm"); err != nil || info.Mode()&os.ModeCharDevice == 0 {
		add("kvm_device", "fail", "KVM device unavailable", "/dev/kvm", "Enable host hardware virtualization and load the vendor KVM module.")
	} else {
		add("kvm_device", "ok", "KVM device present", "/dev/kvm", "")
	}
	found := false
	for _, vendor := range []string{"kvm_intel", "kvm_amd"} {
		path := "/sys/module/" + vendor + "/parameters/nested"
		if raw, err := p.read(path); err == nil {
			found = true
			switch value := strings.TrimSpace(string(raw)); value {
			case "N", "n", "0":
				add("nesting_"+vendor, "ok", "Nested KVM disabled", path, "")
			case "Y", "y", "1":
				add("nesting_"+vendor, "fail", "Nested KVM enabled", path, nestingFix)
			default:
				add("nesting_"+vendor, "skipped", "Nested KVM setting unrecognized", path, nestingFix)
			}
		}
	}
	if !found {
		add("nesting", "skipped", "No readable vendor KVM nesting parameter", "/sys/module", "Check that a vendor KVM module is loaded.")
	}
}

func checkSELinux(p probes, add addCheck) {
	const path = "/sys/fs/selinux/enforce"
	value, err := p.read(path)
	if errors.Is(err, os.ErrNotExist) || (err == nil && strings.TrimSpace(string(value)) == "0") {
		add("selinux_labels", "skipped", "SELinux is absent or permissive", path, "")
		return
	}
	if err != nil || strings.TrimSpace(string(value)) != "1" {
		add("selinux_labels", "skipped", "SELinux mode unavailable", path, "Inspect getenforce and the selinuxfs mount.")
		return
	}
	for _, binary := range []string{"chasm", "chasmctl", "executor_server", "supervisor_server"} {
		binaryPath := "/usr/local/libexec/chasm/" + binary
		output, err := p.command("/usr/bin/ls", []string{"-Z", binaryPath}, nil)
		fields := strings.Fields(output)
		switch {
		case err == nil && len(fields) > 0 && strings.Contains(fields[0], ":bin_t:"):
			add("selinux_"+binary, "ok", fields[0], binaryPath, "")
		default:
			add("selinux_"+binary, "fail", "Executable label is not verified as bin_t", binaryPath, "Check the binary exists, then run restorecon -v "+binaryPath+".")
		}
	}
}

// Restrict the build script to the existing template check and only its
// read-only Incus subcommands. There is no client config directory.
const templateCheck = `
incus() {
    case "${1:-} ${2:-}" in
        "admin waitready") return 0 ;;
        "storage show"|"network show"|"profile show"|"image info"|"config get"|"config device"|"snapshot list") ;;
        "info "*) ;;
        *) return 126 ;;
    esac
    if [ "${1:-} ${2:-}" = "config device" ] && [ "${3:-}" != get ]; then return 126; fi
    /usr/bin/incus --force-local --project default "$@"
}
. "$0"
`

func checkIncus(site siteconfig.Site, socket string, p probes, add addCheck) {
	builder := provision + "/scripts/build-sandbox-template.sh"
	if _, err := p.stat(builder); err != nil {
		add("template_freshness", "skipped", "Installed template checker unavailable", builder, "Install or upgrade the Chasm provisioning payload.")
	} else if !trustedSocketPath(socket, p.lstat) {
		add("template_freshness", "skipped", "Socket path is replaceable by a non-root user", socket, "Configure a local Incus socket under root-owned directories.")
	} else {
		source := builder + " --check"
		output, err := p.command("/bin/sh", []string{"-c", templateCheck, builder, "--check"}, []string{"INCUS_SOCKET=" + socket, "INCUS_CONF=/dev/null"})
		switch {
		case err == nil:
			add("template_freshness", "ok", "Current sealed template verified", source, "")
		case staleTemplateEvidence(output):
			add("template_freshness", "warn", "Current sealed template missing or stale", source, templateFix)
		default:
			add("template_freshness", "skipped", "Current sealed template could not be verified", source, "Check the provisioned image, profile, and template receipt, then rerun doctor.")
		}
	}
	path := "/1.0/storage-pools/" + site.StoragePool + "/resources"
	pool, err := p.observe(socket, path)
	space, _ := pool["space"].(map[string]any)
	total, totalOK := number(space["total"])
	used, usedOK := number(space["used"])
	switch {
	case err != nil || !totalOK || !usedOK || total <= 0 || used < 0 || used > total:
		add("pool_headroom", "skipped", "Pool capacity or usage unavailable", path, "Inspect the site's Incus storage pool and underlying filesystem.")
	case total-used < total/10:
		add("pool_headroom", "warn", fmt.Sprintf("%d MiB available (under 10%%)", (total-used)>>20), path, "Free space in the storage pool before creating or growing sandboxes.")
	default:
		add("pool_headroom", "ok", fmt.Sprintf("%d MiB available", (total-used)>>20), path, "")
	}

	const ready = "sandbox_readiness"
	instance := site.DefaultInstanceName()
	if instance == "" {
		add(ready, "skipped", "No default sandbox configured", siteconfig.DefaultPath, "Set default_sandbox to a managed logical sandbox name to include readiness.")
		return
	}
	path = "/1.0/instances/" + instance
	metadata, err := p.observe(socket, path)
	config, _ := metadata["config"].(map[string]any)
	switch {
	case errors.Is(err, guestexec.ErrInstanceNotFound):
		add(ready, "warn", "Default sandbox does not exist", path, "Run chasm create "+site.DefaultSandbox+".")
	case err != nil:
		add(ready, "skipped", "Default sandbox metadata unavailable", path, incusFix)
	case metadata["type"] != "virtual-machine" || execserver.ValidateManagedSandboxMetadataFor(site, instance, metadata) != nil:
		add(ready, "fail", "Default sandbox is not a managed Chasm VM", path, "Check the configured default_sandbox and ownership of the named instance.")
	case config[site.MetadataKey("initialized")] != "true":
		add(ready, "warn", "Default sandbox has not finished initialization", path, "Run chasm create "+site.DefaultSandbox+" to complete provisioning.")
	case metadata["status"] == "Stopped" || metadata["status"] == "Frozen":
		add(ready, "skipped", "Sandbox is not running", path, "Run chasm start "+site.DefaultSandbox+" when readiness needs to be checked.")
	case metadata["status"] != "Running":
		add(ready, "skipped", "Sandbox running state unavailable", path, "Inspect the sandbox status with chasm show "+site.DefaultSandbox+".")
	default:
		state, err := p.observe(socket, path+"/state")
		processes, processesOK := number(state["processes"])
		pid, pidOK := number(state["pid"])
		if err == nil && state["status"] == "Running" && pidOK && pid > 0 && processesOK && processes > 0 {
			add(ready, "ok", "Running VM reports a guest process count", path+"/state", "")
		} else {
			add(ready, "skipped", "Incus did not report guest process evidence", path+"/state", "Check Incus agent service and VM logs if the sandbox has finished booting.")
		}
	}
}

// The template checker invokes the fixed Incus CLI, which does not
// authenticate SO_PEERCRED itself. Keep its endpoint out of paths an
// unprivileged user could swap after the native reachability check.
func trustedSocketPath(socket string, lstat func(string) (os.FileInfo, error)) bool {
	return incuslocal.TrustedSocketPath(socket, lstat)
}

type boundedDoctorOutput struct{ bytes.Buffer }

func (output *boundedDoctorOutput) Write(payload []byte) (int, error) {
	const limit = 64 << 10
	remaining := limit - output.Len()
	if remaining <= 0 {
		return len(payload), nil
	}
	if len(payload) > remaining {
		_, _ = output.Buffer.Write(payload[:remaining])
		return len(payload), nil
	}
	return output.Buffer.Write(payload)
}

func staleTemplateEvidence(output string) bool {
	for _, finding := range []string{
		"no current sealed template", "is not imported", "existing template is not stopped",
		"existing template identity mismatch", "existing template bootstrap profile mismatch",
		"existing template profile manifest mismatch", "template receipt is missing",
		"template receipt identity mismatch", "template receipt instance mismatch",
		"template receipt did not pass", "template sealed snapshot is missing",
		"template retains a builder IPv4 address",
	} {
		if strings.Contains(output, "template build: "+finding) ||
			(strings.HasPrefix(finding, "is not imported") && strings.Contains(output, finding)) {
			return true
		}
	}
	return false
}

func number(value any) (int64, bool) {
	n, ok := value.(float64)
	if !ok {
		return 0, false
	}
	parsed, err := strconv.ParseInt(strconv.FormatFloat(n, 'f', -1, 64), 10, 64)
	return parsed, err == nil
}

func (r Report) ExitCode() int {
	for _, check := range r.Checks {
		if check.Status == "fail" {
			return 1
		}
	}
	return 0
}

func OperatorAllowed(site siteconfig.Site, user string, uid int) bool {
	return uid == 0 && user != "" && user == site.OperatorUser
}
