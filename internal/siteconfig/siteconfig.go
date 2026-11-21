// Package siteconfig parses the chasm site descriptor.
//
// The site descriptor (/etc/chasm/site.conf, see
// host-config/site.conf.example) is the single source of truth for the host
// wiring that the control plane used to hardcode: host identity, bridge
// network, storage, profile, image, operator user, admission reserve, and
// subsystem switches.
//
// The format is flat POSIX key=value so that shell scripts can `.` source the
// same file that this package parses. Parsing is fail-closed: unknown keys,
// missing required keys, malformed values, and values that contradict each
// other are all errors. There are no hidden defaults beyond the optional keys
// documented below: site_name falls back to the hostname, operator_user to
// the invoking user, bootstrap_profile to the default in
// DefaultBootstrapProfile, and resource_prefix to DefaultResourcePrefix. No
// other values are invented here.
package siteconfig

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

const (
	// DefaultPath is the installed location of the site descriptor.
	DefaultPath = "/etc/chasm/site.conf"
	// PathEnvironment overrides DefaultPath. It exists so tests and
	// unprivileged dry runs can point at a fixture.
	PathEnvironment = "CHASM_SITE_CONF"

	// MarkerPath is the site identity marker written by provisioning and
	// verified by every later privileged entry. It is a product constant
	// rather than a site key so that systemd units can reference it
	// statically.
	MarkerPath = "/etc/chasm/site-host-id"

	// State and socket paths shared by the executor, the supervisor, and the
	// installed units.
	SupervisorStateRoot = "/var/lib/chasm/supervisor"
	ExecutorStateRoot   = "/var/lib/chasm/executor"
	ExecutorSocket      = "/run/chasm-executor.sock"
	SupervisorSocket    = "/run/chasm-supervisor.sock"

	// SupportedArchitecture gates the memfd_create syscall number, which is
	// amd64-specific. Other architectures must fail closed until they are
	// deliberately supported.
	SupportedArchitecture = "amd64"

	// DefaultBootstrapProfile is the profile used when bootstrap_profile is
	// absent or empty.
	DefaultBootstrapProfile = "minimal"

	// DefaultResourcePrefix names every Incus object this site owns when
	// resource_prefix is absent or empty: instance config keys
	// user.<prefix>.*, the shared and per-sandbox custom volumes, and the
	// sealed template instances. Host-side names (units, paths, users) are
	// product constants and never derive from it.
	DefaultResourcePrefix = "chasm"

	// MaxHostReserveMiB bounds host_reserve_mib (0..1048576 MiB, i.e. up to
	// 1 TiB — far beyond any supported host's reserve). The bound is shared
	// with the shell reader (scripts/lib/require-site-host.sh) so both
	// readers refuse the same descriptors, and it keeps the installer's MiB
	// admission arithmetic far inside signed 64-bit shell math, where an
	// unbounded reserve would wrap and silently invert the comparison.
	MaxHostReserveMiB = 1048576

	// MaxResourcePrefixLength bounds resource_prefix. Derived Incus names
	// (a workspace volume is <prefix>-workspace-sandbox-<name>) must stay
	// inside Incus's instance-name limit with room for the longest sandbox
	// name.
	MaxResourcePrefixLength = 16
)

// Site is a parsed, validated site descriptor.
type Site struct {
	// SiteName is informational.
	SiteName string

	BridgeName        string
	BridgeIPv4        string // CIDR, e.g. 10.42.0.1/24
	SandboxIPv4Prefix string // network prefix implied by BridgeIPv4, e.g. 10.42.0.

	StoragePool        string
	PoolSizeGiB        int
	ProfileRootDiskGiB int // Zero retains the shipped fresh-profile default.
	ProfileName        string
	BaseImageAlias     string
	BaseImageRemote    string

	OperatorUser   string
	GuestUID       int
	SupervisorUser string

	// ResourcePrefix derives every Incus object name and instance-config
	// metadata key this site owns; see DefaultResourcePrefix.
	ResourcePrefix string

	HostReserveMiB int

	// DefaultSandbox is the sandbox NAME used when a command omits it. Empty
	// means the caller must supply a name.
	DefaultSandbox string

	// BootstrapProfile names the sealed template bootstrap profile this site
	// launches sandboxes from. Empty means the default
	// (DefaultBootstrapProfile); Parse resolves it, mirroring site_name and
	// operator_user.
	BootstrapProfile string
}

// HostnameResolver resolves the optional site_name default. It is a variable
// so tests can pin the derived name.
var HostnameResolver = os.Hostname

// OperatorUserResolver resolves the optional operator_user default. An empty
// operator_user means "the user who provisioned this site": the installer
// writes the resolved name into the installed descriptor, and this fallback
// keeps a not-yet-provisioned descriptor parseable for --check runs.
var OperatorUserResolver = func() (string, error) {
	for _, name := range []string{"SUDO_USER", "USER", "LOGNAME"} {
		if value := os.Getenv(name); value != "" {
			return value, nil
		}
	}
	return "", errors.New("site config operator_user is absent and the invoking user cannot be resolved")
}

var (
	safeNamePattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)
	userPattern     = regexp.MustCompile(`^[a-z_][a-z0-9_-]*$`)
	sandboxPattern  = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$`)
)

// Parse reads a site descriptor. The caller is responsible for having opened a
// trusted path; nothing here follows symlinks or reads beyond the reader.
func Parse(reader io.Reader) (Site, error) {
	values := map[string]string{}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		text := scanner.Text()
		// A '#' always starts a comment: no key in the schema admits one in
		// its value, which keeps inline documentation legal in the file.
		if comment := strings.IndexByte(text, '#'); comment >= 0 {
			text = text[:comment]
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		key, value, found := strings.Cut(text, "=")
		if !found {
			return Site{}, fmt.Errorf("site config line %d is not a key=value pair", line)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if !safeKey(key) {
			return Site{}, fmt.Errorf("site config line %d has an invalid key", line)
		}
		if _, duplicate := values[key]; duplicate {
			return Site{}, fmt.Errorf("site config key %q is repeated", key)
		}
		if value == "" {
			values[key] = ""
			continue
		}
		if strings.ContainsAny(value, " \t\"'\\$`") {
			return Site{}, fmt.Errorf("site config key %q has a value that is not a plain token", key)
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return Site{}, fmt.Errorf("site config is unreadable: %w", err)
	}

	site, err := decode(values)
	if err != nil {
		return Site{}, err
	}
	if err := site.Validate(); err != nil {
		return Site{}, err
	}
	return site, nil
}

// Load parses the site descriptor at path. path must be absolute, and must be
// a regular, non-symlink file owned by root or by the current user that is
// neither group- nor world-writable: the descriptor decides which host facts
// every privileged component trusts, so a file another user can rewrite is not
// a descriptor.
func Load(path string) (Site, error) {
	if !filepath.IsAbs(path) {
		return Site{}, fmt.Errorf("site config %s is not an absolute path", path)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return Site{}, fmt.Errorf("site config %s is unavailable: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return Site{}, fmt.Errorf("site config %s is not a regular file", path)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return Site{}, fmt.Errorf("site config %s is group- or world-writable", path)
	}
	metadata, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return Site{}, fmt.Errorf("site config %s ownership is unreadable", path)
	}
	if current := os.Geteuid(); metadata.Uid != 0 && metadata.Uid != uint32(current) {
		return Site{}, fmt.Errorf("site config %s is owned by neither root nor the invoking user", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return Site{}, fmt.Errorf("site config %s is unreadable: %w", path, err)
	}
	defer file.Close()
	return Parse(file)
}

// LoadDefault parses the site descriptor from PathEnvironment when set,
// otherwise from DefaultPath.
func LoadDefault() (Site, error) {
	path := os.Getenv(PathEnvironment)
	if path == "" {
		path = DefaultPath
	}
	return Load(path)
}

// ResolvePath reports the path LoadDefault would read.
func ResolvePath() string {
	if path := os.Getenv(PathEnvironment); path != "" {
		return path
	}
	return DefaultPath
}

// Init loads the descriptor at path and binds it as the process site.
func Init(path string) error {
	site, err := Load(path)
	if err != nil {
		return err
	}
	return Set(site)
}

// InitDefault binds the descriptor LoadDefault would read.
func InitDefault() error {
	site, err := LoadDefault()
	if err != nil {
		return err
	}
	return Set(site)
}

// Set validates and binds a site as the process site.
func Set(site Site) error {
	if err := site.Validate(); err != nil {
		return err
	}
	active = site
	activeSet = true
	return nil
}

// Active returns the bound process site. Calling it before Init or Set is a
// programming error: every binary either loads the site descriptor at startup
// or refuses to run.
func Active() Site {
	if !activeSet {
		panic("siteconfig: the site descriptor was not loaded")
	}
	return active
}

// Loaded reports whether a process site has been bound.
func Loaded() bool { return activeSet }

var (
	active    Site
	activeSet bool
)

// Validate applies every cross-key rule. Parse calls it; callers that build a
// Site directly must call it too.
func (s Site) Validate() error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{"bridge_name", s.BridgeName},
		{"storage_pool", s.StoragePool},
		{"profile_name", s.ProfileName},
		{"supervisor_user", s.SupervisorUser},
		{"resource_prefix", s.ResourcePrefix},
	} {
		if !safeNamePattern.MatchString(field.value) {
			return fmt.Errorf("site config %s is not a safe name", field.name)
		}
	}
	if len(s.ResourcePrefix) > MaxResourcePrefixLength {
		return fmt.Errorf("site config resource_prefix is above the permitted range")
	}
	if !userPattern.MatchString(s.OperatorUser) || s.OperatorUser == "root" {
		return fmt.Errorf("site config operator_user is not an unprivileged user name")
	}
	if s.OperatorUser == s.SupervisorUser {
		return fmt.Errorf("site config operator_user and supervisor_user must differ")
	}
	if s.GuestUID <= 0 || s.GuestUID > 1<<31-1 {
		return fmt.Errorf("site config guest_uid is outside the permitted range")
	}
	if s.HostReserveMiB < 0 {
		return fmt.Errorf("site config host_reserve_mib must not be negative")
	}
	// Upper bound shared with the shell reader: keeps the installer's
	// preflight MiB arithmetic inside signed 64-bit shell math.
	if s.HostReserveMiB > MaxHostReserveMiB {
		return fmt.Errorf("site config host_reserve_mib is above the permitted range")
	}
	if s.PoolSizeGiB <= 0 {
		return fmt.Errorf("site config pool_size_gib must be positive")
	}
	// Upper bound shared with the shell reader: keeps the installer's
	// preflight KiB arithmetic inside signed 64-bit shell math.
	if s.PoolSizeGiB > 65536 {
		return fmt.Errorf("site config pool_size_gib is above the permitted range")
	}
	if s.ProfileRootDiskGiB < 0 || s.ProfileRootDiskGiB > 4096 {
		return fmt.Errorf("site config profile_root_disk_gib is outside the permitted range (1..4096)")
	}
	if s.BaseImageAlias == "" {
		return fmt.Errorf("site config base_image_alias is required")
	}
	if remote, _, found := strings.Cut(s.BaseImageRemote, ":"); !found || remote == "" {
		return fmt.Errorf("site config base_image_remote must be remote:image")
	}
	address, _, err := parseBridge(s.BridgeIPv4)
	if err != nil {
		return err
	}
	switch address.To4()[3] {
	case 0, 10, 11, 255:
		return fmt.Errorf("site config bridge_ipv4 uses a reserved host address (.0, .10, .11, .255)")
	}
	if expected := networkPrefix(address); s.SandboxIPv4Prefix != expected {
		return fmt.Errorf("site config sandbox_ipv4_prefix %q is not derivable from bridge_ipv4 %q",
			s.SandboxIPv4Prefix, s.BridgeIPv4)
	}
	if s.DefaultSandbox != "" && !sandboxPattern.MatchString(s.DefaultSandbox) {
		return fmt.Errorf("site config default_sandbox is not a safe sandbox name")
	}
	if s.BootstrapProfile != "" &&
		(s.BootstrapProfile == "true" || s.BootstrapProfile == "false" ||
			!sandboxPattern.MatchString(s.BootstrapProfile)) {
		// The name grammar alone would admit "true"/"false"; a boolean
		// literal is not a profile name, so it is refused explicitly.
		return fmt.Errorf("site config bootstrap_profile is not a safe sandbox name")
	}
	return nil
}

// BridgeAddress returns the host address of the bridge (bridge_ipv4 without
// its prefix length).
func (s Site) BridgeAddress() string {
	address, _, err := parseBridge(s.BridgeIPv4)
	if err != nil {
		return ""
	}
	return address.String()
}

// InstanceNamePrefix is the prefix that turns a logical sandbox name into its
// Incus instance name.
const InstanceNamePrefix = "sandbox-"

// MetadataKey returns the instance-config key for one site-owned metadata
// field, e.g. MetadataKey("managed") == "user.chasm.managed" for the
// default prefix. Volume metadata keys use the same namespace.
func (s Site) MetadataKey(field string) string {
	return "user." + s.ResourcePrefix + "." + field
}

// SharedVolumeName is the site's global shared custom volume.
func (s Site) SharedVolumeName() string { return s.ResourcePrefix + "-shared" }

// WorkspaceVolumeName is the per-sandbox custom volume of one instance.
func (s Site) WorkspaceVolumeName(instance string) string {
	return s.ResourcePrefix + "-workspace-" + instance
}

// TemplateInstanceName is the sealed template instance name for one
// bootstrap profile and template identity prefix (the first 24 hex
// characters of the identity).
func (s Site) TemplateInstanceName(profile, identityPrefix string) string {
	return s.ResourcePrefix + "-template-" + profile + "-" + identityPrefix
}

// DefaultInstanceName maps the site's default logical sandbox name to the
// instance a bare command addresses. Empty means the site requires an explicit
// NAME.
func (s Site) DefaultInstanceName() string {
	if s.DefaultSandbox == "" {
		return ""
	}
	return InstanceNamePrefix + s.DefaultSandbox
}

// TemplateBuilderIPv4 returns the static address the sealed template builder
// uses on the agent bridge: the last host address of the sandbox range.
func (s Site) TemplateBuilderIPv4() string {
	return s.SandboxIPv4Prefix + "11"
}

// SandboxIPv4 returns the static address for one allocated sandbox.
func (s Site) SandboxIPv4(lastOctet int) string {
	return fmt.Sprintf("%s%d", s.SandboxIPv4Prefix, lastOctet)
}

// SupervisorSocketGroup is the group that may talk to the supervisor socket.
func (s Site) SupervisorSocketGroup() string { return s.OperatorUser }

func decode(values map[string]string) (Site, error) {
	site := Site{}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	for _, key := range keys {
		value := values[key]
		switch key {
		case "site_name":
			site.SiteName = value
		case "bridge_name":
			site.BridgeName = value
		case "bridge_ipv4":
			site.BridgeIPv4 = value
		case "sandbox_ipv4_prefix":
			site.SandboxIPv4Prefix = value
		case "storage_pool":
			site.StoragePool = value
		case "pool_size_gib":
			site.PoolSizeGiB = requireInt(key, value)
			if site.PoolSizeGiB == invalidInt {
				return Site{}, fmt.Errorf("site config %s is not an integer", key)
			}
		case "profile_root_disk_gib":
			if !canonicalPositiveInt(value) {
				return Site{}, fmt.Errorf("site config profile_root_disk_gib must be a canonical positive integer")
			}
			site.ProfileRootDiskGiB = requireInt(key, value)
		case "profile_name":
			site.ProfileName = value
		case "base_image_alias":
			site.BaseImageAlias = value
		case "base_image_remote":
			site.BaseImageRemote = value
		case "operator_user":
			site.OperatorUser = value
		case "guest_uid":
			if !canonicalPositiveInt(value) {
				return Site{}, fmt.Errorf("site config %s is not a canonical positive integer (no sign, no leading zeros)", key)
			}
			site.GuestUID = requireInt(key, value)
			if site.GuestUID == invalidInt {
				return Site{}, fmt.Errorf("site config %s is not an integer", key)
			}
		case "supervisor_user":
			site.SupervisorUser = value
		case "resource_prefix":
			site.ResourcePrefix = value
		case "host_reserve_mib":
			if !canonicalNonnegativeInt(value) {
				return Site{}, fmt.Errorf("site config %s is not a canonical nonnegative integer (no sign, no leading zeros)", key)
			}
			site.HostReserveMiB = requireInt(key, value)
			if site.HostReserveMiB == invalidInt {
				return Site{}, fmt.Errorf("site config %s is not an integer", key)
			}
		case "default_sandbox":
			site.DefaultSandbox = value
		case "bootstrap_profile":
			site.BootstrapProfile = value
		default:
			return Site{}, fmt.Errorf("site config key %q is not recognized", key)
		}
	}
	if err := requireKeys(values); err != nil {
		return Site{}, err
	}
	if site.SiteName == "" {
		hostname, err := HostnameResolver()
		if err != nil || strings.TrimSpace(hostname) == "" {
			return Site{}, errors.New("site config site_name is absent and the hostname is unavailable")
		}
		site.SiteName = strings.TrimSpace(hostname)
	}
	if site.OperatorUser == "" {
		operator, err := OperatorUserResolver()
		if err != nil {
			return Site{}, err
		}
		site.OperatorUser = strings.TrimSpace(operator)
	}
	if site.BootstrapProfile == "" {
		site.BootstrapProfile = DefaultBootstrapProfile
	}
	if site.ResourcePrefix == "" {
		site.ResourcePrefix = DefaultResourcePrefix
	}
	return site, nil
}

const invalidInt = -1 << 62

// canonicalNonnegativeInt reports whether value is a nonnegative decimal
// integer in canonical form: digits only, no sign, and no leading zeros
// ("0" itself is canonical). host_reserve_mib is the one schema integer the
// builder admission math consumes, and it consumes it through shell
// $((...)) arithmetic that reads a leading-zero constant as octal, so both
// this reader and the shell reader (scripts/lib/require-site-host.sh
// site_is_canonical_mib) reject non-canonical spellings; the descriptor-case
// fixtures reserve-leading-zero and reserve-plus pin that parity.
func canonicalNonnegativeInt(value string) bool {
	if value == "" || (len(value) > 1 && value[0] == '0') {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

// canonicalPositiveInt is canonicalNonnegativeInt without "0": the first
// digit must be 1-9. guest_uid is the one schema integer that must be
// strictly positive (the range check below the parse enforces the same
// bound), and it flows into the template identity preimage and the guest
// layer's textual UID comparison, where the kernel normalizes "04242" to
// "4242" and a "+" never survives: a non-canonical spelling would be
// accepted here and then fail guest verification. The shell reader
// (scripts/lib/require-site-host.sh site_is_canonical_uid) applies the
// identical rule; the descriptor-case fixtures guest-uid-leading-zero and
// guest-uid-plus pin that parity.
func canonicalPositiveInt(value string) bool {
	if value == "" || value[0] == '0' {
		return false
	}
	return canonicalNonnegativeInt(value)
}

func requireInt(key, value string) int {
	if value == "" {
		return invalidInt
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return invalidInt
	}
	return parsed
}

// requiredKeys lists every key Parse insists on. site_name is optional and
// falls back to the host name.
func requireKeys(values map[string]string) error {
	for _, key := range []string{
		"bridge_name",
		"bridge_ipv4",
		"sandbox_ipv4_prefix",
		"storage_pool",
		"pool_size_gib",
		"profile_name",
		"base_image_alias",
		"base_image_remote",
		"operator_user",
		"guest_uid",
		"supervisor_user",
		"host_reserve_mib",
	} {
		if _, present := values[key]; !present {
			return fmt.Errorf("site config key %q is required", key)
		}
	}
	return nil
}

func safeKey(key string) bool {
	if key == "" {
		return false
	}
	for index, character := range key {
		switch {
		case character >= 'a' && character <= 'z':
		case character == '_':
		case character >= '0' && character <= '9' && index > 0:
		default:
			return false
		}
	}
	return true
}

func parseBridge(value string) (net.IP, *net.IPNet, error) {
	address, network, err := net.ParseCIDR(value)
	if err != nil || address.To4() == nil {
		return nil, nil, fmt.Errorf("site config bridge_ipv4 %q is not an IPv4 CIDR", value)
	}
	ones, bits := network.Mask.Size()
	if bits != 32 || ones != 24 {
		return nil, nil, fmt.Errorf("site config bridge_ipv4 %q must be a /24", value)
	}
	return address, network, nil
}

func networkPrefix(address net.IP) string {
	quad := address.To4()
	return fmt.Sprintf("%d.%d.%d.", quad[0], quad[1], quad[2])
}
