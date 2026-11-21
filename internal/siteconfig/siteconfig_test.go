package siteconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func fixturePath(t *testing.T, site string) string {
	t.Helper()
	path, err := testsupport.CopySiteConfig(filepath.Join(moduleRoot(), "testdata", "fixtures", site, "site.conf"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func examplePath(t *testing.T) string {
	t.Helper()
	path, err := testsupport.CopySiteConfig(filepath.Join(moduleRoot(), "host-config", "site.conf.example"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// historicalExpectations is the reviewed record of the constants that were
// compiled into the sandboxing checkout at master c661bbb. It lives in the
// fixture tree so no Go source has to name the historical host.
func TestTestSiteFixtureIsNeutral(t *testing.T) {
	site, err := Load(fixturePath(t, "testsite"))
	if err != nil {
		t.Fatalf("load testsite fixture: %v", err)
	}
	if site.DefaultSandbox != "" {
		t.Errorf("testsite must leave default_sandbox empty so tests name their sandbox, got %q", site.DefaultSandbox)
	}
	if site.SandboxIPv4Prefix != "10.42.0." {
		t.Errorf("testsite prefix = %q", site.SandboxIPv4Prefix)
	}
	if site.HostReserveMiB != 512 || site.PoolSizeGiB != 8 {
		t.Errorf("testsite sizing = reserve %d MiB, pool %d GiB", site.HostReserveMiB, site.PoolSizeGiB)
	}
}

// TestExampleDescriptorIsTheSupportedDebianDefault pins the shipped defaults
// that the installer copies onto a fresh host: a new site must start with every
// subsystem that needs absent infrastructure switched off.
func TestExampleDescriptorIsTheSupportedDebianDefault(t *testing.T) {
	site, err := Load(examplePath(t))
	if err != nil {
		t.Fatalf("load site.conf.example: %v", err)
	}
	if site.SiteName == "" {
		t.Error("example descriptor must resolve a site_name (hostname fallback)")
	}
	if site.DefaultSandbox != "" {
		t.Errorf("example descriptor must require an explicit NAME, got default_sandbox=%q", site.DefaultSandbox)
	}
	if site.BootstrapProfile != DefaultBootstrapProfile {
		t.Errorf("example descriptor bootstrap_profile = %q, want the default %q",
			site.BootstrapProfile, DefaultBootstrapProfile)
	}
	if site.HostReserveMiB < 1024 {
		t.Errorf("example descriptor host_reserve_mib = %d, want at least 1024", site.HostReserveMiB)
	}
	if site.DefaultInstanceName() != "" {
		t.Errorf("example descriptor must not imply a default instance, got %q", site.DefaultInstanceName())
	}
}

// TestParseRejectsUnsafeAndUnknownInput mutates the neutral fixture, so the
// negative cases never have to spell out a historical value.
func TestParseRejectsUnsafeAndUnknownInput(t *testing.T) {
	base := readFixture(t, "testsite")
	cases := []struct {
		name    string
		rewrite func(string) string
		match   string
	}{
		{"unknown key", func(s string) string { return s + "mystery=1\n" }, "not recognized"},
		{"repeated key", func(s string) string { return s + "storage_pool=other\n" }, "repeated"},
		{"missing key", func(s string) string { return strings.Replace(s, "guest_uid=1001", "", 1) }, `"guest_uid" is required`},
		{"prefix mismatch", func(s string) string {
			return strings.Replace(s, "sandbox_ipv4_prefix=10.42.0.", "sandbox_ipv4_prefix=10.43.0.", 1)
		}, "not derivable"},
		{"non /24 bridge", func(s string) string {
			return strings.Replace(s, "bridge_ipv4=10.42.0.1/24", "bridge_ipv4=10.42.0.1/16", 1)
		}, "must be a /24"},
		{"root operator", func(s string) string { return strings.Replace(s, "operator_user=chasmops", "operator_user=root", 1) }, "unprivileged user"},
		{"operator shares supervisor user", func(s string) string {
			return strings.Replace(s, "operator_user=chasmops", "operator_user=chasm-supervisor", 1)
		}, "must differ"},
		{"unsafe pool name", func(s string) string {
			return strings.Replace(s, "storage_pool=chasm-test", "storage_pool=../../etc", 1)
		}, "not a safe name"},
		{"quoted value", func(s string) string {
			return strings.Replace(s, "storage_pool=chasm-test", `storage_pool="chasm-test"`, 1)
		}, "plain token"},
		{"unsafe default sandbox", func(s string) string { return strings.Replace(s, "default_sandbox=", "default_sandbox=Bad Name", 1) }, "plain token"},
		{"unsafe bootstrap profile", func(s string) string { return s + "bootstrap_profile=Minimal\n" }, "bootstrap_profile"},
		{"bootstrap profile with space", func(s string) string { return s + "bootstrap_profile=two words\n" }, "plain token"},
		{"bootstrap profile leading hyphen", func(s string) string { return s + "bootstrap_profile=-minimal\n" }, "bootstrap_profile"},
		{"bootstrap profile too long", func(s string) string { return s + "bootstrap_profile=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n" }, "bootstrap_profile"},
		{"bootstrap profile boolean", func(s string) string { return s + "bootstrap_profile=true\n" }, "bootstrap_profile"},
		{"negative host reserve", func(s string) string { return strings.Replace(s, "host_reserve_mib=512", "host_reserve_mib=-1", 1) }, "canonical nonnegative integer"},
		{"host reserve above the bound", func(s string) string {
			return strings.Replace(s, "host_reserve_mib=512", "host_reserve_mib=1048577", 1)
		}, "host_reserve_mib is above the permitted range"},
		{"not key=value", func(s string) string { return s + "this line has no separator\n" }, "not a key=value pair"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(test.rewrite(base)))
			if err == nil {
				t.Fatalf("expected an error mentioning %q", test.match)
			}
			if !strings.Contains(err.Error(), test.match) {
				t.Fatalf("error %q does not mention %q", err, test.match)
			}
		})
	}
}

// TestHostReserveMiBRequiresCanonicalInteger pins the Go reader's half of
// the non-canonical integer rule: host_reserve_mib feeds the builder
// admission's shell $((...)) arithmetic (which reads a leading-zero constant
// as octal), so the reader must reject a leading '+' or leading zeros even
// though strconv.Atoi would parse them, while "0" and canonical decimals
// stay valid. The shell reader's matching rule is pinned by the
// reserve-leading-zero and reserve-plus descriptor parity fixtures.
func TestHostReserveMiBRequiresCanonicalInteger(t *testing.T) {
	base := readFixture(t, "testsite")
	cases := []struct {
		name    string
		value   string
		wantErr string
	}{
		{name: "leading zero", value: "01024", wantErr: "canonical nonnegative integer"},
		{name: "leading zero for a single-digit value", value: "0512", wantErr: "canonical nonnegative integer"},
		{name: "plus sign", value: "+1024", wantErr: "canonical nonnegative integer"},
		{name: "minus sign", value: "-1", wantErr: "canonical nonnegative integer"},
		{name: "plain zero", value: "0"},
		{name: "canonical value", value: "1024"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(strings.Replace(base, "host_reserve_mib=512", "host_reserve_mib="+test.value, 1)))
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("host_reserve_mib=%s must be accepted, got %v", test.value, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("host_reserve_mib=%s must be refused", test.value)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error %q does not mention %q", err, test.wantErr)
			}
		})
	}
}

func TestLoadRejectsWritableDescriptor(t *testing.T) {
	for _, mode := range []os.FileMode{0o664, 0o646, 0o666} {
		t.Run(fmt.Sprintf("%04o", mode), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "site.conf")
			if err := os.WriteFile(path, []byte(readFixture(t, "testsite")), 0o600); err != nil {
				t.Fatalf("write descriptor: %v", err)
			}
			if err := os.Chmod(path, mode); err != nil {
				t.Fatalf("chmod descriptor: %v", err)
			}
			if _, err := Load(path); err == nil {
				t.Fatalf("expected mode %04o to be refused", mode)
			}
		})
	}
}

func TestLoadRefusesARelativeDescriptorPath(t *testing.T) {
	// The descriptor decides which host facts every privileged component
	// trusts, so a path relative to the caller's working directory is refused
	// even when the file itself is safe.
	_, err := Load(filepath.Join("..", "..", "testdata", "fixtures", "testsite", "site.conf"))
	if err == nil {
		t.Fatal("a relative descriptor path was accepted")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("error %q does not name the absolute-path rule", err)
	}
}

func TestLoadDefaultHonoursEnvironmentOverride(t *testing.T) {
	path := fixturePath(t, "testsite")
	t.Setenv(PathEnvironment, path)
	if ResolvePath() != path {
		t.Fatalf("ResolvePath = %q, want %q", ResolvePath(), path)
	}
	site, err := LoadDefault()
	if err != nil {
		t.Fatalf("LoadDefault: %v", err)
	}
	if site.StoragePool != "chasm-test" {
		t.Fatalf("LoadDefault picked up the wrong descriptor: pool=%q", site.StoragePool)
	}
}

func TestOptionalKeysDeriveInsteadOfDemandingAnAnswer(t *testing.T) {
	previousHostname, previousOperator := HostnameResolver, OperatorUserResolver
	HostnameResolver = func() (string, error) { return "fixture-host", nil }
	OperatorUserResolver = func() (string, error) { return "operator", nil }
	t.Cleanup(func() {
		HostnameResolver, OperatorUserResolver = previousHostname, previousOperator
	})

	descriptor := strings.NewReplacer(
		"site_name=chasm-test", "site_name=",
		"operator_user=chasmops", "operator_user=",
	).Replace(readFixture(t, "testsite"))
	site, err := Parse(strings.NewReader(descriptor))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if site.SiteName != "fixture-host" {
		t.Errorf("site_name = %q, want the resolved hostname", site.SiteName)
	}
	if site.OperatorUser != "operator" {
		t.Errorf("operator_user = %q, want the resolved provisioning user", site.OperatorUser)
	}
}

func TestBootstrapProfileFallsBackToTheDefaultMinimal(t *testing.T) {
	base := readFixture(t, "testsite")

	site, err := Parse(strings.NewReader(base))
	if err != nil {
		t.Fatalf("parse testsite: %v", err)
	}
	if site.BootstrapProfile != DefaultBootstrapProfile {
		t.Errorf("absent bootstrap_profile must resolve to %q, got %q",
			DefaultBootstrapProfile, site.BootstrapProfile)
	}

	site, err = Parse(strings.NewReader(base + "bootstrap_profile=\n"))
	if err != nil {
		t.Fatalf("parse empty bootstrap_profile: %v", err)
	}
	if site.BootstrapProfile != DefaultBootstrapProfile {
		t.Errorf("empty bootstrap_profile must resolve to %q, got %q",
			DefaultBootstrapProfile, site.BootstrapProfile)
	}

	site, err = Parse(strings.NewReader(base + "bootstrap_profile=wp2-default\n"))
	if err != nil {
		t.Fatalf("parse explicit bootstrap_profile: %v", err)
	}
	if site.BootstrapProfile != "wp2-default" {
		t.Errorf("explicit bootstrap_profile = %q, want wp2-default", site.BootstrapProfile)
	}
}

func TestResourcePrefixFallsBackToTheDefaultAndDerivesNames(t *testing.T) {
	base := readFixture(t, "testsite")

	site, err := Parse(strings.NewReader(base))
	if err != nil {
		t.Fatalf("parse testsite: %v", err)
	}
	if site.ResourcePrefix != DefaultResourcePrefix {
		t.Errorf("absent resource_prefix must resolve to %q, got %q", DefaultResourcePrefix, site.ResourcePrefix)
	}

	site, err = Parse(strings.NewReader(base + "resource_prefix=\n"))
	if err != nil {
		t.Fatalf("parse empty resource_prefix: %v", err)
	}
	if site.ResourcePrefix != DefaultResourcePrefix {
		t.Errorf("empty resource_prefix must resolve to %q, got %q", DefaultResourcePrefix, site.ResourcePrefix)
	}

	site, err = Parse(strings.NewReader(base + "resource_prefix=agent-sandbox\n"))
	if err != nil {
		t.Fatalf("parse explicit resource_prefix: %v", err)
	}
	if site.ResourcePrefix != "agent-sandbox" {
		t.Errorf("explicit resource_prefix = %q", site.ResourcePrefix)
	}
	if key := site.MetadataKey("managed"); key != "user.agent-sandbox.managed" {
		t.Errorf("MetadataKey = %q", key)
	}
	if volume := site.SharedVolumeName(); volume != "agent-sandbox-shared" {
		t.Errorf("SharedVolumeName = %q", volume)
	}
	if volume := site.WorkspaceVolumeName("sandbox-demo"); volume != "agent-sandbox-workspace-sandbox-demo" {
		t.Errorf("WorkspaceVolumeName = %q", volume)
	}
	if name := site.TemplateInstanceName("minimal", "0123abcd"); name != "agent-sandbox-template-minimal-0123abcd" {
		t.Errorf("TemplateInstanceName = %q", name)
	}

	// The default site derives the historical chasm names.
	site, err = Parse(strings.NewReader(base))
	if err != nil {
		t.Fatalf("reparse testsite: %v", err)
	}
	if site.MetadataKey("managed") != "user.chasm.managed" ||
		site.SharedVolumeName() != "chasm-shared" ||
		site.WorkspaceVolumeName("sandbox-demo") != "chasm-workspace-sandbox-demo" ||
		site.TemplateInstanceName("minimal", "0123abcd") != "chasm-template-minimal-0123abcd" {
		t.Fatalf("default-prefix derivations changed: %+v", site)
	}
}

func TestResourcePrefixIsHeldToTheSafeNameRules(t *testing.T) {
	base := readFixture(t, "testsite")
	for value, want := range map[string]string{
		"Agent-Sandbox":            "not a safe name",
		"agent_sandbox":            "not a safe name",
		"1agent":                   "not a safe name",
		"-agent":                   "not a safe name",
		"agent--sandbox":           "not a safe name",
		"agent-sandbox-extra-long": "permitted range",
	} {
		_, err := Parse(strings.NewReader(base + "resource_prefix=" + value + "\n"))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("resource_prefix=%q: err=%v, want it to name %q", value, err, want)
		}
	}
}

func TestActiveIsBoundOnceAndRefusesUninitialisedUse(t *testing.T) {
	previousSite, previousSet := active, activeSet
	t.Cleanup(func() { active, activeSet = previousSite, previousSet })

	active, activeSet = Site{}, false
	if Loaded() {
		t.Fatal("Loaded reports a bound site after reset")
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("Active on an unbound process site must be a programming error")
			}
		}()
		_ = Active()
	}()
	if err := Init(fixturePath(t, "testsite")); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if !Loaded() || Active().SiteName != "chasm-test" {
		t.Fatalf("Active = %+v after Init", Active())
	}
	if instance := Active().DefaultInstanceName(); instance != "" {
		t.Fatalf("testsite must not imply a default instance, got %q", instance)
	}
}

func readFixture(t *testing.T, site string) string {
	t.Helper()
	content, err := os.ReadFile(fixturePath(t, site))
	if err != nil {
		t.Fatalf("read %s fixture: %v", site, err)
	}
	return string(content)
}
