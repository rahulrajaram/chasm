package execserver

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/rahulrajaram/chasm/internal/protocol"
	"github.com/rahulrajaram/chasm/internal/siteconfig"
)

const (
	launcherPath       = "/usr/local/lib/chasm/provision/scripts/launch-sandbox-vm.sh"
	catalogVersion     = 1
	catalogMaxDepth    = 5
	repositoryMaxDepth = 12
	maxCatalogEntries  = 1024
	maxRepositories    = 256
	maxCatalogOutput   = 1024 * 1024
	maxCatalogFile     = 2 * 1024 * 1024
	workspacePath      = "/workspace"
	sharedPath         = "/shared"
)

type catalogEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
}

type catalogSnapshot struct {
	Version      int            `json:"version"`
	Instance     string         `json:"instance"`
	ObservedAt   string         `json:"observed_at"`
	MaxDepth     int            `json:"max_depth"`
	Entries      []catalogEntry `json:"entries"`
	Repositories []string       `json:"repositories"`
}

type boundedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
}

func (writer *boundedBuffer) Write(payload []byte) (int, error) {
	available := writer.limit - writer.buffer.Len()
	if available > 0 {
		stored := payload
		if len(stored) > available {
			stored = stored[:available]
		}
		_, _ = writer.buffer.Write(stored)
	}
	if len(payload) > available {
		writer.exceeded = true
	}
	return len(payload), nil
}

func (writer *boundedBuffer) Bytes() []byte { return writer.buffer.Bytes() }

func logicalSandboxName(instance string) string {
	return strings.TrimPrefix(instance, "sandbox-")
}

func safeRelativeCatalogPath(path string) bool {
	return path != "" && len(path) <= 1024 && utf8.ValidString(path) && !filepath.IsAbs(path) &&
		path == filepath.Clean(path) && path != ".." && !strings.HasPrefix(path, "../")
}

func catalogPathDepth(path string) int {
	if path == "." {
		return 0
	}
	return strings.Count(path, string(filepath.Separator)) + 1
}

func parseLayoutOutput(raw []byte) ([]catalogEntry, error) {
	if len(raw) > maxCatalogOutput || (len(raw) > 0 && raw[len(raw)-1] != 0) {
		return nil, protoError("unsafe_state", "sandbox layout output is malformed or oversized")
	}
	parts := bytes.Split(raw, []byte{0})
	entries := make([]catalogEntry, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		if len(part) == 0 {
			continue
		}
		kind, path, found := strings.Cut(string(part), "\t")
		if !found || len(kind) != 1 || !strings.Contains("dfl", kind) || !safeRelativeCatalogPath(path) || catalogPathDepth(path) > catalogMaxDepth {
			return nil, protoError("unsafe_state", "sandbox layout output contains an unsafe entry")
		}
		if _, duplicate := seen[path]; duplicate {
			return nil, protoError("unsafe_state", "sandbox layout output contains duplicate entries")
		}
		seen[path] = struct{}{}
		entryType := map[string]string{"d": "directory", "f": "file", "l": "symlink"}[kind]
		entries = append(entries, catalogEntry{Path: path, Type: entryType})
		if len(entries) > maxCatalogEntries {
			return nil, protoError("scan_bound_exceeded", "sandbox layout entry bound exceeded")
		}
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].Path < entries[right].Path })
	return entries, nil
}

func isAdditionalGuestWorktree(path string) bool {
	return path == ".worktrees" || strings.HasPrefix(path, ".worktrees/")
}

func parseRepositoryOutput(raw []byte) ([]string, error) {
	if len(raw) > maxCatalogOutput || (len(raw) > 0 && raw[len(raw)-1] != 0) {
		return nil, protoError("unsafe_state", "repository inventory output is malformed or oversized")
	}
	parts := bytes.Split(raw, []byte{0})
	repositories := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		if len(part) == 0 {
			continue
		}
		// find's %h includes ./ when scanning from the workspace root.
		path := strings.TrimPrefix(string(part), "./")
		if !safeRelativeCatalogPath(path) || catalogPathDepth(path) >= repositoryMaxDepth {
			return nil, protoError("unsafe_state", "repository inventory contains an unsafe path")
		}
		if isAdditionalGuestWorktree(path) {
			continue
		}
		if _, duplicate := seen[path]; duplicate {
			continue
		}
		seen[path] = struct{}{}
		repositories = append(repositories, path)
		if len(repositories) > maxRepositories {
			return nil, protoError("scan_bound_exceeded", "repository inventory bound exceeded")
		}
	}
	sort.Strings(repositories)
	return repositories, nil
}

func scanSandbox(instance string) (catalogSnapshot, error) {
	guestUID := siteconfig.Active().GuestUID
	layout := &boundedBuffer{limit: maxCatalogOutput}
	code, err := incusExec([]string{
		"exec", instance, "--user", strconv.Itoa(guestUID), "--group", strconv.Itoa(guestUID), "--cwd", workspacePath, "--",
		"/usr/bin/find", ".", "-xdev", "-mindepth", "1", "-maxdepth", strconv.Itoa(catalogMaxDepth),
		"(", "-type", "d", "-o", "-type", "f", "-o", "-type", "l", ")", "-printf", "%y\\t%P\\0",
	}, nil, layout, nil, 60*time.Second)
	if err != nil || code != 0 || layout.exceeded {
		return catalogSnapshot{}, protoError("unsafe_state", "sandbox layout scan failed")
	}
	entries, err := parseLayoutOutput(layout.Bytes())
	if err != nil {
		return catalogSnapshot{}, err
	}

	repositories, err := scanRepositories(instance)
	if err != nil {
		return catalogSnapshot{}, err
	}
	snapshot := catalogSnapshot{
		Version: catalogVersion, Instance: instance, ObservedAt: time.Now().UTC().Format(time.RFC3339),
		MaxDepth: catalogMaxDepth, Entries: entries, Repositories: repositories,
	}
	if err := writeCatalogSnapshot(snapshot); err != nil {
		return catalogSnapshot{}, err
	}
	return snapshot, nil
}

func scanRepositories(instance string) ([]string, error) {
	guestUID := siteconfig.Active().GuestUID
	repos := &boundedBuffer{limit: maxCatalogOutput}
	code, err := runIncus([]string{
		"exec", instance, "--user", strconv.Itoa(guestUID), "--group", strconv.Itoa(guestUID), "--cwd", workspacePath, "--",
		"/usr/bin/find", ".", "-xdev", "-mindepth", "1", "-maxdepth", strconv.Itoa(repositoryMaxDepth),
		"(", "-type", "d", "-name", ".git", "-printf", "%h\\0", "-prune", ")", "-o",
		"(", "-type", "f", "-name", ".git", "-printf", "%h\\0", ")",
	}, nil, repos, nil, 60*time.Second)
	if err != nil || code != 0 || repos.exceeded {
		return nil, protoError("unsafe_state", "repository inventory scan failed")
	}
	repositories, err := parseRepositoryOutput(repos.Bytes())
	if err != nil {
		return nil, err
	}
	return repositories, nil
}

func catalogRoot() string { return filepath.Join(auditRoot, "catalog") }

func catalogFile(instance string) string { return filepath.Join(catalogRoot(), instance+".json") }

func validateCatalogSnapshot(snapshot catalogSnapshot, instance string) error {
	if snapshot.Version != catalogVersion || snapshot.Instance != instance || snapshot.MaxDepth != catalogMaxDepth {
		return protoError("unsafe_state", "sandbox catalog metadata is invalid")
	}
	if _, err := time.Parse(time.RFC3339, snapshot.ObservedAt); err != nil || len(snapshot.Entries) > maxCatalogEntries || len(snapshot.Repositories) > maxRepositories {
		return protoError("unsafe_state", "sandbox catalog metadata is invalid")
	}
	for _, entry := range snapshot.Entries {
		if !safeRelativeCatalogPath(entry.Path) || catalogPathDepth(entry.Path) > catalogMaxDepth ||
			(entry.Type != "directory" && entry.Type != "file" && entry.Type != "symlink") {
			return protoError("unsafe_state", "sandbox catalog entry is invalid")
		}
	}
	for _, repository := range snapshot.Repositories {
		if !safeRelativeCatalogPath(repository) || catalogPathDepth(repository) >= repositoryMaxDepth {
			return protoError("unsafe_state", "sandbox catalog repository is invalid")
		}
	}
	return nil
}

func writeCatalogSnapshot(snapshot catalogSnapshot) error {
	if !protocol.ValidSandboxInstance(snapshot.Instance) {
		return protoError("invalid_instance", "instance is not permitted")
	}
	if err := validateCatalogSnapshot(snapshot, snapshot.Instance); err != nil {
		return err
	}
	if err := os.MkdirAll(catalogRoot(), 0o700); err != nil {
		return protoError("unsafe_state", "sandbox catalog directory could not be created")
	}
	if err := statDir(catalogRoot(), 0o700); err != nil {
		return protoError("unsafe_state", "sandbox catalog directory metadata is unsafe")
	}
	raw, err := json.Marshal(snapshot)
	if err != nil || len(raw) > maxCatalogFile {
		return protoError("unsafe_state", "sandbox catalog could not be encoded")
	}
	temporary, err := os.CreateTemp(catalogRoot(), ".catalog-*")
	if err != nil {
		return protoError("unsafe_state", "sandbox catalog could not be written")
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return protoError("unsafe_state", "sandbox catalog could not be written")
	}
	if _, err := temporary.Write(raw); err != nil {
		temporary.Close()
		return protoError("unsafe_state", "sandbox catalog could not be written")
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return protoError("unsafe_state", "sandbox catalog could not be written")
	}
	if err := temporary.Close(); err != nil {
		return protoError("unsafe_state", "sandbox catalog could not be written")
	}
	if err := os.Rename(temporaryPath, catalogFile(snapshot.Instance)); err != nil {
		return protoError("unsafe_state", "sandbox catalog could not be committed")
	}
	return nil
}

func readCatalogSnapshot(instance string) (catalogSnapshot, bool, error) {
	path := catalogFile(instance)
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, syscall.ENOENT) {
		return catalogSnapshot{}, false, nil
	}
	if err != nil {
		return catalogSnapshot{}, false, protoError("unsafe_state", "sandbox catalog could not be opened")
	}
	defer syscall.Close(fd)
	var info syscall.Stat_t
	if err := syscall.Fstat(fd, &info); err != nil || info.Mode&syscall.S_IFMT != syscall.S_IFREG || info.Uid != 0 || info.Mode&0o777 != 0o600 || info.Size > maxCatalogFile {
		return catalogSnapshot{}, false, protoError("unsafe_state", "sandbox catalog metadata is unsafe")
	}
	file := os.NewFile(uintptr(fd), path)
	raw, err := io.ReadAll(io.LimitReader(file, maxCatalogFile+1))
	if err != nil || len(raw) > maxCatalogFile {
		return catalogSnapshot{}, false, protoError("unsafe_state", "sandbox catalog is invalid")
	}
	var snapshot catalogSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil || validateCatalogSnapshot(snapshot, instance) != nil {
		return catalogSnapshot{}, false, protoError("unsafe_state", "sandbox catalog is invalid")
	}
	return snapshot, true, nil
}

func snapshotFor(instance, status string) (catalogSnapshot, string, error) {
	// A running managed sandbox is scanned live; a stopped one renders its
	// last observed snapshot. Never start an instance merely to manufacture
	// catalog evidence.
	if status == "Running" {
		snapshot, err := scanSandbox(instance)
		return snapshot, "live", err
	}
	snapshot, found, err := readCatalogSnapshot(instance)
	if err != nil {
		return catalogSnapshot{}, "", err
	}
	if !found {
		return catalogSnapshot{Version: catalogVersion, Instance: instance, MaxDepth: catalogMaxDepth, Entries: []catalogEntry{}, Repositories: []string{}}, "unobserved", nil
	}
	return snapshot, "last_observed", nil
}

func scanSandboxLayout(instance string, depth int) ([]catalogEntry, error) {
	guestUID := siteconfig.Active().GuestUID
	layout := &boundedBuffer{limit: maxCatalogOutput}
	code, err := runIncus([]string{
		"exec", instance, "--user", strconv.Itoa(guestUID), "--group", strconv.Itoa(guestUID), "--cwd", workspacePath, "--",
		"/usr/bin/find", ".", "-xdev", "-mindepth", "1", "-maxdepth", strconv.Itoa(depth),
		"(", "-type", "d", "-o", "-type", "f", "-o", "-type", "l", ")", "-printf", "%y\\t%P\\0",
	}, nil, layout, nil, 60*time.Second)
	if err != nil || code != 0 || layout.exceeded {
		return nil, protoError("unsafe_state", "sandbox layout scan failed")
	}
	return parseLayoutOutput(layout.Bytes())
}

var runIncus = incusExec

func runLauncherWithBudget(instance string, leaveStopped bool, requestedBudget int64, timings lifecycleTimings) error {
	values, err := incusEnvironment()
	if err != nil {
		return protoError("unsafe_state", "Incus client environment is unsafe")
	}
	if requestedBudget != 0 {
		if !protocol.ValidMemoryBudget(requestedBudget) {
			return protoError("invalid_budget", protocol.BudgetRangeMessage())
		}
		values["BUDGET_MEMORY_MIB"] = strconv.FormatInt(requestedBudget, 10)
	}
	values["VM_NAME"] = instance
	address, err := allocateSandboxIPv4(instance)
	if err != nil {
		return err
	}
	values["VM_IPV4"] = address
	if leaveStopped {
		values["LEAVE_STOPPED"] = "1"
	} else {
		values["LEAVE_STOPPED"] = "0"
	}
	environment := make([]string, 0, len(values))
	for key, value := range values {
		environment = append(environment, key+"="+value)
	}
	// The launcher takes the lifecycle lock and creates or boots the sandbox
	// from the sealed template, refusing closed when the template is stale.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, launcherPath)
	command.Dir = "/"
	command.Env = environment
	ownProcessGroup(command)
	stdout := &boundedBuffer{limit: 64 * 1024}
	stderr := &boundedBuffer{limit: 64 * 1024}
	command.Stdout = stdout
	command.Stderr = stderr
	if timings == nil {
		err = command.Run()
	} else {
		err = runInstrumentedLauncher(command, timings)
	}
	if ctx.Err() != nil {
		return protoError("invalid_state", "sandbox creation timed out")
	}
	if err != nil || stdout.exceeded || stderr.exceeded {
		return protoError("invalid_state", "sandbox creation failed: "+launcherExcerpt(stderr.Bytes()))
	}
	return nil
}

// launcherExcerpt bounds the launcher's failure output to a short, single
// line so operator-facing errors explain the cause without leaking control
// state.
func launcherExcerpt(raw []byte) string {
	text := strings.Join(strings.Fields(string(raw)), " ")
	if len(text) > 300 {
		text = text[len(text)-300:]
	}
	if text == "" {
		text = "no diagnostic output was captured"
	}
	return text
}

// allocateSandboxIPv4 picks the first free static sandbox-bridge address for a
// new managed sandbox. Addresses the product derives statically are reserved
// and never allocated: the .1 gateway, the .10 fallback the Incus profile
// assigns to any instance without an explicit eth0 override, and the .11
// address the sealed-template builder uses. Claims are gathered through the
// same three sources the launcher's claim inventory
// (scripts/lib/incus-ipv4-claims.sh) enforces before any launch — the eth0
// ipv4.address device override (any state: a stopped instance reclaims the
// address on its next boot), the site's user.<prefix>.instance-ipv4
// metadata, and a live IPv4 of a RUNNING or FROZEN instance — so the
// allocator never proposes an address the launcher will refuse and retries
// advance to the next free address. The lifecycle lock serializes
// allocation.
func allocateSandboxIPv4(instance string) (string, error) {
	site := siteconfig.Active()
	inventory := &boundedBuffer{limit: 64 * 1024}
	code, err := runIncus([]string{"list", "--format", "csv", "-c", "ns4"}, nil, inventory, nil, 30*time.Second)
	if err != nil || code != 0 || inventory.exceeded {
		return "", protoError("unsafe_state", "sandbox address inventory is unavailable")
	}
	claims, err := parseSandboxAddressInventory(string(inventory.Bytes()))
	if err != nil {
		return "", protoError("unsafe_state", "sandbox address inventory is malformed")
	}
	for index := range claims {
		claims[index].override = readSandboxDeviceIPv4(claims[index].name)
		claims[index].metadata = readSandboxConfigValue(claims[index].name, site.MetadataKey(instanceIPv4MetadataField))
	}
	return selectSandboxIPv4(site, collectSandboxIPv4Claims(claims, instance))
}

// instanceIPv4MetadataField is the metadata field of the site's instance-ipv4
// claim, read exactly as the launcher's claim inventory reads it
// (user.<prefix>.instance-ipv4).
const instanceIPv4MetadataField = "instance-ipv4"

// sandboxAddressClaim is one instance's entry in the claim inventory: the
// `incus list --format csv -c ns4` record plus the per-instance config reads
// that complete the three claim sources the launcher enforces.
type sandboxAddressClaim struct {
	name     string
	status   string
	override string   // eth0 ipv4.address device override; empty when unset
	metadata string   // user.<prefix>.instance-ipv4; empty when unset
	live     []string // live IPv4 addresses, interface annotations stripped
}

// claimedAddresses returns the addresses the instance holds. The override
// and the metadata claim in every state (a stopped instance reclaims the
// override on its next boot and keeps its recorded metadata); the live
// addresses claim only while the instance is RUNNING or FROZEN.
func (claim sandboxAddressClaim) claimedAddresses() []string {
	addresses := make([]string, 0, 2+len(claim.live))
	if claim.override != "" {
		addresses = append(addresses, claim.override)
	}
	if claim.metadata != "" {
		addresses = append(addresses, claim.metadata)
	}
	switch strings.ToUpper(claim.status) {
	case "RUNNING", "FROZEN":
		addresses = append(addresses, claim.live...)
	}
	return addresses
}

// collectSandboxIPv4Claims maps every address claimed by an instance other
// than except (empty skips nothing) to true, from the same three claim
// sources the launcher's inventory enforces. It is pure so the claim rule
// can be tested without an Incus host.
func collectSandboxIPv4Claims(claims []sandboxAddressClaim, except string) map[string]bool {
	used := map[string]bool{}
	for _, claim := range claims {
		if claim.name == "" || claim.name == except {
			continue
		}
		for _, address := range claim.claimedAddresses() {
			used[address] = true
		}
	}
	return used
}

// sandboxAddressAnnotation strips the " (ifname)" annotation Incus renders
// after every live address.
var sandboxAddressAnnotation = regexp.MustCompile(`\([^)]*\)`)

// parseSandboxAddressInventory parses the shared claim inventory
// (`incus list --format csv -c ns4`: NAME,STATUS,IPv4-addresses). Incus
// renders an instance with several IPv4 addresses as newline-joined
// "addr (ifname)" entries inside ONE quoted field, so a record spans
// physical lines; encoding/csv handles the quoted multi-line field, and the
// live addresses are normalized only by dropping the " (ifname)"
// annotations so the later comparison stays exact. A malformed inventory is
// an error so the caller fails closed.
func parseSandboxAddressInventory(inventory string) ([]sandboxAddressClaim, error) {
	records, err := csv.NewReader(strings.NewReader(inventory)).ReadAll()
	if err != nil {
		return nil, err
	}
	claims := make([]sandboxAddressClaim, 0, len(records))
	for _, record := range records {
		if len(record) == 0 {
			continue
		}
		name := strings.TrimSpace(record[0])
		if name == "" {
			continue
		}
		claim := sandboxAddressClaim{name: name}
		if len(record) > 1 {
			claim.status = strings.TrimSpace(record[1])
		}
		if len(record) > 2 {
			claim.live = sandboxLiveAddresses(record[2])
		}
		claims = append(claims, claim)
	}
	return claims, nil
}

// sandboxLiveAddresses normalizes the live-address column to bare IPv4
// addresses: "addr (ifname)" entries joined by newlines or commas.
func sandboxLiveAddresses(column string) []string {
	tokens := strings.FieldsFunc(sandboxAddressAnnotation.ReplaceAllString(column, " "), func(character rune) bool {
		switch character {
		case ' ', '\t', '\n', '\r', ',', ';', '"':
			return true
		}
		return false
	})
	addresses := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if token = strings.TrimSpace(token); token != "" {
			addresses = append(addresses, token)
		}
	}
	return addresses
}

// readSandboxDeviceIPv4 reads one instance's eth0 ipv4.address device
// override; an unreadable value claims nothing, exactly as the launcher's
// shell inventory treats a failed config read.
func readSandboxDeviceIPv4(name string) string {
	return readSandboxConfigArgument([]string{"config", "device", "get", name, "eth0", "ipv4.address"})
}

// readSandboxConfigValue reads one instance-config value by key; an
// unreadable value claims nothing, exactly as the launcher's shell inventory
// treats a failed config read.
func readSandboxConfigValue(name, key string) string {
	return readSandboxConfigArgument([]string{"config", "get", name, key})
}

func readSandboxConfigArgument(arguments []string) string {
	buffer := &boundedBuffer{limit: 4096}
	code, err := runIncus(arguments, nil, buffer, nil, 30*time.Second)
	if err != nil || code != 0 || buffer.exceeded {
		return ""
	}
	return strings.TrimSpace(string(buffer.Bytes()))
}

// selectSandboxIPv4 returns the first allocatable address for the site given
// the addresses already claimed. It is pure so the reservation rule can be
// tested without an Incus host.
func selectSandboxIPv4(site siteconfig.Site, claimed map[string]bool) (string, error) {
	used := make(map[string]bool, len(claimed)+len(reservedSandboxHosts)+1)
	for address, taken := range claimed {
		used[address] = taken
	}
	used[strings.SplitN(site.BridgeIPv4, "/", 2)[0]] = true
	for _, reserved := range reservedSandboxHosts {
		used[site.SandboxIPv4(reserved)] = true
	}
	for last := 2; last <= 254; last++ {
		candidate := site.SandboxIPv4(last)
		if !used[candidate] {
			return candidate, nil
		}
	}
	return "", protoError("unsafe_state", "sandbox address pool is exhausted")
}

// reservedSandboxHosts lists the last-octet values that are never allocated
// because the product derives them statically: the Incus profile's eth0
// fallback (.10) and the sealed-template builder (.11). The allocator starts
// at .2 and excludes the configured bridge address separately.
var reservedSandboxHosts = []int{10, 11}

// requestedBudget is the declared memory budget in MiB for
// the created sandbox; 0 means "no preference" and records the protocol
// default. requestedCPU is the declared CPU count; 0 means "no preference"
// and records protocol.DefaultSandboxBudgetCPU. The budgets are catalog
// state on the instance config, are applied as the instance's actual
// limits.memory / limits.cpu ceilings, and are proven by read-back.
func createSandbox(instance string, requestedBudget int64, requestedCPU int64, timings lifecycleTimings) (map[string]interface{}, error) {
	if !protocol.ValidSandboxInstance(instance) {
		return nil, protoError("invalid_instance", "instance is not permitted")
	}
	// Idiom 4: commit only what the host can back. The explicit memory
	// budget is refused up front when it exceeds this host's cap — host
	// memory minus host_reserve_mib — naming the cap's source.
	if requestedBudget != 0 {
		cap, memTotal, reserve, capErr := hostMemoryCapMiB()
		if capErr != nil {
			return nil, capErr
		}
		if err := hostCapRefusalError(requestedBudget, cap, memTotal, reserve); err != nil {
			return nil, err
		}
	}
	// The CPU budget's cap is the host CPU count, named in the refusal.
	if requestedCPU != 0 {
		if err := refuseCpuBudgetAboveHostCap(requestedCPU); err != nil {
			return nil, err
		}
	}
	summaries, err := listInstanceStatuses()
	if err != nil {
		return nil, err
	}
	for _, summary := range summaries {
		if summary.Name == instance {
			if err := requireManagedSandbox(instance); err != nil {
				return nil, err
			}
			storedBudget, err := sandboxBudgetFromConfig(summary.Config)
			if err != nil {
				return nil, err
			}
			storedPresent := summary.Config[budgetMetadataKey()] != ""
			if storedPresent && requestedBudget != 0 && requestedBudget != storedBudget {
				return nil, protoError("invalid_state", fmt.Sprintf(
					"sandbox budget is already declared as %d MiB; delete and recreate the sandbox to change it",
					storedBudget))
			}
			cpuPresent := summary.Config[cpuBudgetMetadataKey()] != ""
			storedCPU, err := sandboxCpuBudgetValueFromConfig(summary.Config)
			if err != nil {
				return nil, err
			}
			if !cpuPresent {
				enforced, budgetErr := sandboxCpuBudget(summary)
				if budgetErr != nil {
					return nil, budgetErr
				}
				cpu := adoptBudgetValue(requestedCPU, enforced.enforced, enforced.enforcedPresent, protocol.ValidCpuBudget, protocol.DefaultSandboxBudgetCPU)
				if err := setSandboxCpuBudget(instance, cpu); err != nil {
					return nil, err
				}
				storedCPU = cpu
			} else if requestedCPU != 0 && requestedCPU != storedCPU {
				return nil, protoError("invalid_state", fmt.Sprintf(
					"sandbox CPU budget is already declared as %d; delete and recreate the sandbox to change it",
					storedCPU))
			}
			if !storedPresent {
				enforced, budgetErr := sandboxMemoryBudget(summary)
				if budgetErr != nil {
					return nil, budgetErr
				}
				adopted := adoptBudgetValue(requestedBudget, enforced.enforced, enforced.enforcedPresent, protocol.ValidMemoryBudget, protocol.DefaultSandboxBudgetMemoryMiB)
				if err := setSandboxBudget(instance, adopted); err != nil {
					return nil, err
				}
				storedBudget = adopted
			}
			// storedBudget reflects the reconciled key above: the requested
			// value when one was given, else the adopted enforced value or
			// the default.
			if err := setInstanceMemoryLimit(instance, storedBudget); err != nil {
				return nil, err
			}
			if err := setInstanceCpuLimit(instance, storedCPU); err != nil {
				return nil, err
			}
			if !sandboxInitialized(instance) {
				// A failed earlier create can leave a managed
				// instance with metadata but without guest provisioning.
				// Re-run the launcher so initialization completes before
				// the create is allowed to report success.
				if err := runLauncherWithBudget(instance, true, 0, timings); err != nil {
					return nil, err
				}
				if !sandboxInitialized(instance) {
					return nil, protoError("postcondition_failed", "sandbox guest initialization did not complete")
				}
			}
			result, err := inspectSandbox(instance)
			if err == nil {
				result["created"] = false
			}
			return result, err
		}
	}
	// Inventory proved this name absent. A deleted incarnation may have
	// left timing files if teardown was interrupted before cleanup.
	if err := removeLifecycleObservations(lifecycleRoot(), 0, instance); err != nil {
		return nil, protoError("operation_failed", "observations for the previous sandbox could not be cleared")
	}
	if err := runLauncherWithBudget(instance, true, requestedBudget, timings); err != nil {
		return nil, err
	}
	budget := requestedBudget
	if budget == 0 {
		budget = protocol.DefaultSandboxBudgetMemoryMiB
	}
	cpu := requestedCPU
	if cpu == 0 {
		cpu = protocol.DefaultSandboxBudgetCPU
	}
	if err := setSandboxBudget(instance, budget); err != nil {
		return nil, err
	}
	// Memory modeling honesty: the VM's actual memory ceiling is the
	// budget, not the profile default — the ledger and the limit are the
	// same number.
	if err := setInstanceMemoryLimit(instance, budget); err != nil {
		return nil, err
	}
	// The CPU budget is written together with the enforced limits.cpu and
	// proven by read-back, exactly like the memory pair.
	if err := setSandboxCpuBudget(instance, cpu); err != nil {
		return nil, err
	}
	if err := setInstanceCpuLimit(instance, cpu); err != nil {
		return nil, err
	}
	result, err := inspectSandbox(instance)
	if err != nil || result["instance_status"] != "Stopped" || result["instance_pid"] != int64(0) {
		return nil, protoError("postcondition_failed", "created sandbox state was not proven")
	}
	if result["budget_memory_mib"] != budget {
		return nil, protoError("postcondition_failed", "recorded sandbox budget was not proven")
	}
	snapshot := catalogSnapshot{
		Version: catalogVersion, Instance: instance, ObservedAt: time.Now().UTC().Format(time.RFC3339), MaxDepth: catalogMaxDepth,
		Entries: []catalogEntry{}, Repositories: []string{},
	}
	if err := writeCatalogSnapshot(snapshot); err != nil {
		return nil, err
	}
	result["created"] = true
	return result, nil
}

// setSandboxBudget records the declared budget on the instance catalog
// and proves the write by reading the key back: the recorded ledger entry
// is only trusted once Incus confirms it (idiom 1).
func setSandboxBudget(instance string, budget int64) error {
	if !protocol.ValidMemoryBudget(budget) {
		return protoError("invalid_budget", protocol.BudgetRangeMessage())
	}
	value := strconv.FormatInt(budget, 10)
	code, err := runIncus([]string{"config", "set", instance, budgetMetadataKey(), value}, nil, nil, nil, 30*time.Second)
	if err != nil || code != 0 {
		return protoError("operation_failed", "sandbox budget could not be recorded")
	}
	readBack := readSandboxConfigValue(instance, budgetMetadataKey())
	if readBack == "" {
		return protoError("operation_failed", "sandbox budget could not be read back")
	}
	if readBack != value {
		return protoError("postcondition_failed", "recorded sandbox budget was not proven")
	}
	return nil
}

// sandboxCpuBudgetValueFromConfig reads the declared CPU budget from the
// instance catalog config; a keyless instance reads as the protocol default
// (the value its limits.cpu is about to be set to) and only a non-numeric,
// sub-floor, or over-bound value is invalid.
func sandboxCpuBudgetValueFromConfig(config map[string]string) (int64, error) {
	raw, present := config[cpuBudgetMetadataKey()]
	if !present || raw == "" {
		return protocol.DefaultSandboxBudgetCPU, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || !protocol.ValidCpuBudget(value) {
		return 0, protoError("unsafe_state", "sandbox CPU budget metadata is invalid")
	}
	return value, nil
}

// setSandboxCpuBudget records the declared CPU budget on the instance
// catalog and proves the write by reading the key back.
func setSandboxCpuBudget(instance string, cpu int64) error {
	if !protocol.ValidCpuBudget(cpu) {
		return protoError("invalid_cpu_budget", protocol.CpuBudgetRangeMessage())
	}
	value := strconv.FormatInt(cpu, 10)
	code, err := runIncus([]string{"config", "set", instance, cpuBudgetMetadataKey(), value}, nil, nil, nil, 30*time.Second)
	if err != nil || code != 0 {
		return protoError("operation_failed", "sandbox CPU budget could not be recorded")
	}
	readBack := readSandboxConfigValue(instance, cpuBudgetMetadataKey())
	if readBack == "" {
		return protoError("operation_failed", "sandbox CPU budget could not be read back")
	}
	if readBack != value {
		return protoError("postcondition_failed", "recorded sandbox CPU budget was not proven")
	}
	return nil
}

// memoryLimitValue renders the budget as the instance-level incus memory
// ceiling. Memory-modeling honesty: the admission ledger entry and the
// VM's actual memory limit are the same number, so a small budget means a
// genuinely small VM instead of an overcommitted ledger line.
func memoryLimitValue(budget int64) string {
	return strconv.FormatInt(budget, 10) + "MiB"
}

// setInstanceMemoryLimit applies the budget as the instance's actual
// limits.memory ceiling and proves it from a fresh config read. Called at
// create time while the instance is stopped; pre-budget instances keep the
// profile default (3GiB), which already matches their default ledger entry.
func setInstanceMemoryLimit(instance string, budget int64) error {
	if !protocol.ValidMemoryBudget(budget) {
		return protoError("invalid_budget", protocol.BudgetRangeMessage())
	}
	limit := memoryLimitValue(budget)
	code, err := runIncus([]string{"config", "set", instance, "limits.memory", limit}, nil, nil, nil, 30*time.Second)
	if err != nil || code != 0 {
		return protoError("operation_failed", "sandbox memory limit could not be applied")
	}
	var readBack strings.Builder
	code, err = runIncus([]string{"config", "get", instance, "limits.memory"}, nil, &readBack, nil, 30*time.Second)
	if err != nil || code != 0 {
		return protoError("operation_failed", "sandbox memory limit could not be read back")
	}
	if strings.TrimSpace(readBack.String()) != limit {
		return protoError("postcondition_failed", "sandbox memory limit was not proven")
	}
	return nil
}

// setInstanceCpuLimit applies the CPU budget as the instance's actual
// limits.cpu count and proves it from a fresh config read, mirroring
// setInstanceMemoryLimit. Called at create time while the instance is
// stopped.
func setInstanceCpuLimit(instance string, cpu int64) error {
	if !protocol.ValidCpuBudget(cpu) {
		return protoError("invalid_cpu_budget", protocol.CpuBudgetRangeMessage())
	}
	limit := strconv.FormatInt(cpu, 10)
	code, err := runIncus([]string{"config", "set", instance, "limits.cpu", limit}, nil, nil, nil, 30*time.Second)
	if err != nil || code != 0 {
		return protoError("operation_failed", "sandbox CPU limit could not be applied")
	}
	var readBack strings.Builder
	code, err = runIncus([]string{"config", "get", instance, "limits.cpu"}, nil, &readBack, nil, 30*time.Second)
	if err != nil || code != 0 {
		return protoError("operation_failed", "sandbox CPU limit could not be read back")
	}
	if strings.TrimSpace(readBack.String()) != limit {
		return protoError("postcondition_failed", "sandbox CPU limit was not proven")
	}
	return nil
}

// hostMemoryCapMiB returns this host's per-sandbox memory cap (MemTotal
// minus the site's host_reserve_mib) together with the MemTotal figure, so
// refusals can name the cap's source (idiom 4).
func hostMemoryCapMiB() (cap int64, memTotal int64, reserve int64, err error) {
	meminfo, meminfoErr := numericKibRecords("/proc/meminfo", meminfoFields)
	if meminfoErr != nil {
		return 0, 0, 0, protoError("unsafe_state", "host memory inventory is unavailable")
	}
	totalKiB, ok := numericInt64(meminfo["MemTotal"])
	if !ok || totalKiB <= 0 {
		return 0, 0, 0, protoError("unsafe_state", "host memory inventory is invalid")
	}
	memTotal = totalKiB / 1024
	reserve = int64(siteconfig.Active().HostReserveMiB)
	if reserve < 0 || reserve > memTotal {
		return 0, 0, 0, protoError("unsafe_state", "host memory inventory is invalid")
	}
	return memTotal - reserve, memTotal, reserve, nil
}

// hostCapRefusalError rejects a memory budget above a host's cap,
// naming the cap and its source. It is pure so tests can inject MemTotal
// and reserve instead of reading the real host.
func hostCapRefusalError(budget, cap, memTotal, reserve int64) error {
	if budget > cap {
		return protoError("resource_exhausted", fmt.Sprintf(
			"budget %d MiB exceeds this host's cap %d MiB (MemTotal %d MiB - host_reserve_mib %d)",
			budget, cap, memTotal, reserve))
	}
	return nil
}

// refuseBudgetAboveHostCap rejects a memory budget above this host's cap —
// MemTotal minus host_reserve_mib — naming the cap and its source. The cap
// is host-derived; there is no fixed product ceiling (idiom 4).
func refuseBudgetAboveHostCap(budget int64) error {
	if !protocol.ValidMemoryBudget(budget) {
		return protoError("invalid_budget", protocol.BudgetRangeMessage())
	}
	cap, memTotal, reserve, err := hostMemoryCapMiB()
	if err != nil {
		return err
	}
	return hostCapRefusalError(budget, cap, memTotal, reserve)
}

// hostCpuCount is the seam for the host's CPU count: the cap a per-sandbox
// CPU budget may never exceed at create or resize (idiom 4). Tests inject
// a count so the wiring is exercised without depending on this machine.
var hostCpuCount = func() int64 { return int64(runtime.NumCPU()) }

// refuseCpuBudgetAboveHostCap rejects a CPU budget above the host CPU
// count, naming the cap's source.
func refuseCpuBudgetAboveHostCap(cpu int64) error {
	if !protocol.ValidCpuBudget(cpu) {
		return protoError("invalid_cpu_budget", protocol.CpuBudgetRangeMessage())
	}
	cap := hostCpuCount()
	if cpu > cap {
		return protoError("resource_exhausted", fmt.Sprintf(
			"budget %d CPU exceeds this host's cap %d CPU (host CPU count)", cpu, cap))
	}
	return nil
}

// adoptBudgetValue decides the budget recorded for an existing sandbox
// whose declared key is absent: the requested value when one was given,
// else the value Incus already enforces — the enforced truth is adopted as
// the record and the limit is left unchanged (idiom 1) — and only when
// nothing is enforced the protocol default. An enforced value outside the
// valid range falls back to the default.
func adoptBudgetValue(requested, enforced int64, enforcedPresent bool, valid func(int64) bool, fallback int64) int64 {
	if requested != 0 {
		return requested
	}
	if enforcedPresent && valid(enforced) {
		return enforced
	}
	return fallback
}

func sandboxLayout(instance string, depth int) (map[string]interface{}, error) {
	if depth < 1 || depth > catalogMaxDepth {
		return nil, protoError("invalid_depth", "layout depth is not permitted")
	}
	if err := requireManagedSandbox(instance); err != nil {
		return nil, err
	}
	state, err := inspectSandbox(instance)
	if err != nil {
		return nil, err
	}
	status, _ := state["instance_status"].(string)
	var entries []catalogEntry
	var freshness, observedAt string
	if status == "Running" {
		entries, err = scanSandboxLayout(instance, depth)
		if err != nil {
			return nil, err
		}
		freshness, observedAt = "live", time.Now().UTC().Format(time.RFC3339)
	} else {
		snapshot, snapshotFreshness, snapshotErr := snapshotFor(instance, status)
		if snapshotErr != nil {
			return nil, snapshotErr
		}
		entries, freshness, observedAt = snapshot.Entries, snapshotFreshness, snapshot.ObservedAt
	}
	resultEntries := make([]map[string]interface{}, 0, len(entries))
	for _, entry := range entries {
		if catalogPathDepth(entry.Path) <= depth {
			resultEntries = append(resultEntries, map[string]interface{}{"path": filepath.Join(workspacePath, entry.Path), "type": entry.Type})
		}
	}
	return map[string]interface{}{
		"instance": instance, "logical_name": logicalSandboxName(instance), "root": workspacePath, "depth": depth,
		"entries": resultEntries, "freshness": freshness, "observed_at": observedAt, "complete": freshness != "unobserved",
	}, nil
}

func repositoryInventory() ([]map[string]interface{}, []map[string]interface{}, bool, error) {
	instances, err := listSandboxInstances()
	if err != nil {
		return nil, nil, false, err
	}
	repositories := []map[string]interface{}{}
	sandboxes := []map[string]interface{}{}
	complete := true
	for _, instance := range instances {
		var repositoriesForInstance []string
		var freshness, observedAt string
		if instance.Status == "Running" || instance.Status == "Frozen" {
			repositoriesForInstance, err = scanRepositories(instance.Name)
			if err != nil {
				return nil, nil, false, err
			}
			freshness, observedAt = "live", time.Now().UTC().Format(time.RFC3339)
		} else {
			snapshot, snapshotFreshness, snapshotErr := snapshotFor(instance.Name, instance.Status)
			if snapshotErr != nil {
				return nil, nil, false, snapshotErr
			}
			repositoriesForInstance, freshness, observedAt = snapshot.Repositories, snapshotFreshness, snapshot.ObservedAt
		}
		if freshness == "unobserved" {
			complete = false
		}
		sandboxes = append(sandboxes, map[string]interface{}{
			"instance": instance.Name, "logical_name": logicalSandboxName(instance.Name), "freshness": freshness, "observed_at": observedAt,
		})
		for _, relative := range repositoriesForInstance {
			// Older cached scans can still contain additional worktrees. They
			// remain visible in layout, but are not independent projects.
			if isAdditionalGuestWorktree(relative) {
				continue
			}
			name := filepath.Base(relative)
			if relative == "." {
				name = logicalSandboxName(instance.Name)
			}
			repositories = append(repositories, map[string]interface{}{
				"name": name, "instance": instance.Name, "logical_name": logicalSandboxName(instance.Name),
				"path": filepath.Join(workspacePath, relative), "freshness": freshness, "observed_at": observedAt,
			})
			if len(repositories) > 1024 {
				return nil, nil, false, protoError("scan_bound_exceeded", "global repository inventory bound exceeded")
			}
		}
	}
	sort.Slice(repositories, func(left, right int) bool {
		return fmt.Sprint(repositories[left]["instance"], repositories[left]["path"]) < fmt.Sprint(repositories[right]["instance"], repositories[right]["path"])
	})
	return repositories, sandboxes, complete, nil
}

func listRepositories() (map[string]interface{}, error) {
	repositories, sandboxes, complete, err := repositoryInventory()
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"repositories": repositories, "sandboxes": sandboxes, "complete": complete}, nil
}

func locateRepository(repository string) (map[string]interface{}, error) {
	if repository == "" {
		return nil, protoError("invalid_repository", "repository identifier is not permitted")
	}
	repositories, sandboxes, complete, err := repositoryInventory()
	if err != nil {
		return nil, err
	}
	matches := []map[string]interface{}{}
	for _, candidate := range repositories {
		if candidate["name"] == repository {
			matches = append(matches, candidate)
		}
	}
	return map[string]interface{}{"repository": repository, "matches": matches, "sandboxes": sandboxes, "complete": complete}, nil
}
