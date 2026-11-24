#!/bin/sh
umask 022
set -eu

# Repo-local, unprivileged regression harness for provisioning steps that
# must admit a normal re-run on a live site — one whose sandboxes may be
# RUNNING — instead of refusing whenever anything runs.
#
# The harness sources the real resource-admission library
# (scripts/lib/host-resources.sh) and drives its verdicts against PATH-shimmed
# incus, df, and ip commands plus a fake /proc/meminfo through the library's
# documented CHASM_MEMINFO_FILE override (tests only), so no root, no Incus,
# and no host state is involved:
#
#   1. incus_size_to_mib parses the shared byte-size grammar (plain bytes,
#      KiB/MiB/GiB/TiB binary, kB/KB/MB/GB/TB decimal — the same contract
#      Go's parseMemoryLimitBytes applies) and fails closed on anything
#      else, including fractional numbers and the percentage form;
#   2. host_mem_available_mib reads MemAvailable (and only MemAvailable) from
#      the override path;
#   3. the template-build memory admission admits a builder beside running
#      instances when the host can carry it — MemTotal covers the builder
#      limit plus host_reserve plus margin plus the budget the executor
#      would count for every running sandbox (the larger of the declared
#      user.<prefix>.budget-memory-mib key and the enforced expanded
#      limits.memory — the CSV -c columns Incus reads from the expanded
#      config —, defaulted only when neither is present), and
#      MemAvailable covers the need — refusing with every number named when
#      either guard fails, when the profile carries no parseable
#      limits.memory, when a running managed sandbox has a
#      present-but-unreadable or out-of-range declared budget or an
#      unsupported enforced limits.memory (e.g. the percentage form or a
#      value outside the supported 1..1048576MiB enforced range), when
#      a running instance is not a managed sandbox of the site, when an
#      instance sits in an unaccountable state (e.g. ERROR), or when the
#      `incus list` inventory itself fails; a keyless
#      sandbox with no enforced limit is counted at the 3072MiB protocol
#      default, exactly as the executor accounts it; a drifted sandbox
#      (declared 8192MiB, enforced 48GiB) is counted at 49152MiB, so the
#      admission refuses where declared-only counting would admit;
#   4. the preflight disk-headroom verdict applies the full rule when the
#      pool does not exist, and the margin + remaining loop-file growth rule
#      when the site's loop pool already exists (enough / not enough growth
#      room), falling back to the full rule for a non-loop-backed pool;
#   5. the bridge-prefix collision verdict accepts only the exact connected
#      route implied by bridge_ipv4 (network address/24) on the site's own
#      Incus bridge and refuses any other overlapping route (foreign
#      interface, missing bridge, mismatched bridge subnet, or a different
#      overlapping destination on the site bridge device itself);
#   6. pool growth through provision (idiom 3): the pool resize plan is a
#      no-op when pool_size_gib equals the pool, approves a grow when the
#      site asks for more, and refuses a shrink with both numbers named;
#      an existing pool with no configured size (an operator-precreated,
#      non-loop pool) is reported as unmanaged and re-provision continues;
#      the grow's host-backing verdict requires the added ceiling (new size
#      minus the loop file's allocated bytes) plus the margin to fit in the
#      free space of the filesystem holding the loop file, and fails the
#      same way when the backing is insufficient or unreadable; a non-loop
#      pool is refused outright (its backing cannot be proven); and the
#      real apply_pool_resize function is run end to end: it must issue
#      exactly `incus storage set <pool> size=<N>GiB`, log the applied set,
#      and prove the grow by reading the new size back, refusing a stale
#      read-back;
#   6. the builder's lifecycle-lock wiring: the extracted acquire/release
#      functions really hold an exclusive lock on the lock file (created
#      0600, the only mode the executor accepts), the flock shim records
#      -x 9 on acquire and -u 9 on release, and the real builder script
#      calls acquire before builder_memory_admission and release after
#      `incus start` confirms RUNNING, with a release inside on_exit for
#      every failure path.
#
# Usage: scripts/test-provision-rerun.sh   (repository root; no arguments)

[ "$#" -eq 0 ] || { echo "usage: $0" >&2; exit 2; }

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
LIB=$ROOT/scripts/lib/host-resources.sh

die() {
    echo "test-provision-rerun: FAIL: $1" >&2
    exit 1
}

# shellcheck source=scripts/lib/host-resources.sh
. "$LIB"

WORK=$(mktemp -d)
trap 'rm -rf -- "$WORK"' EXIT HUP INT TERM
mkdir -p "$WORK/bin"

export PATH="$WORK/bin:$PATH"

# --- fake incus --------------------------------------------------------------
# State comes from exported HR_* variables so each scenario only sets what it
# needs: HR_PROFILE_LIMITS (limits.memory value; HR_PROFILE_GET_FAIL=1 makes
# even the query fail; HR_EXPECT_PROFILE pins the queried profile name),
# HR_POOL_EXISTS/HR_POOL_SIZE/HR_POOL_SOURCE (storage show YAML; when
# HR_POOL_STATE is set, `storage set` records the new size there and
# `storage show` reads it back, while HR_STORAGE_SET_READONLY=1 makes the
# set log but not take effect, simulating a stale read-back),
# HR_EXPECT_STORAGE_SHOW_POOL (pins the pool name of every `storage show`),
# HR_NETWORK_EXISTS/HR_NETWORK_IPV4 (network show/get), and HR_INSTANCES
# (the `incus list --format csv` inventory: one CSV row per instance —
# name,status,managed,budget,limits.memory — as the admission reader parses
# it). HR_LIST_FAIL=1 makes `incus list` exit 1. The fake refuses any list
# argv other than the exact `list -c n,s,user.<prefix>.managed,
# user.<prefix>.budget-memory-mib,limits.memory --format csv` (the prefix
# taken from HR_LIST_PREFIX, default chasm), so a mutated or dropped column
# — limits.memory included — or a dropped --format flag cannot pass
# silently.
cat >"$WORK/bin/incus" <<'STUB'
#!/bin/sh
if [ "$1" = profile ] && [ "$2" = get ]; then
    if [ -n "${HR_EXPECT_PROFILE:-}" ] && [ "$3" != "$HR_EXPECT_PROFILE" ]; then
        echo "fake incus: profile get asked for unexpected profile '$3' (want $HR_EXPECT_PROFILE)" >&2
        exit 9
    fi
    if [ "${HR_PROFILE_GET_FAIL:-0}" = 1 ]; then
        echo "Error: instance not found" >&2
        exit 1
    fi
    [ -n "${HR_PROFILE_LIMITS:-}" ] || exit 1
    printf '%s\n' "$HR_PROFILE_LIMITS"
    exit 0
fi
if [ "$1" = list ]; then
    hr_cols="n,s,user.${HR_LIST_PREFIX:-chasm}.managed,user.${HR_LIST_PREFIX:-chasm}.budget-memory-mib,limits.memory"
    if [ "$#" -ne 5 ] || [ "$2" != -c ] || [ "$3" != "$hr_cols" ] || [ "$4" != --format ] || [ "$5" != csv ]; then
        echo "fake incus: unexpected list argv (want: list -c $hr_cols --format csv): $*" >&2
        exit 9
    fi
    if [ "${HR_LIST_FAIL:-0}" = 1 ]; then
        echo "Error: daemon unavailable" >&2
        exit 1
    fi
    [ -n "${HR_INSTANCES:-}" ] && printf '%s\n' "$HR_INSTANCES"
    exit 0
fi
if [ "$1" = storage ] && [ "$2" = show ]; then
    if [ -n "${HR_EXPECT_STORAGE_SHOW_POOL:-}" ] && [ "$3" != "$HR_EXPECT_STORAGE_SHOW_POOL" ]; then
        echo "fake incus: storage show asked for unexpected pool '$3' (want $HR_EXPECT_STORAGE_SHOW_POOL)" >&2
        exit 9
    fi
    [ "${HR_POOL_EXISTS:-0}" = 1 ] || { echo "Error: not found" >&2; exit 1; }
    size=${HR_POOL_SIZE:-}
    if [ -n "${HR_POOL_STATE:-}" ]; then
        [ -f "$HR_POOL_STATE" ] && size=$(cat "$HR_POOL_STATE")
    fi
    echo "config:"
    [ -n "$size" ] && echo "  size: $size"
    [ -n "${HR_POOL_SOURCE:-}" ] && echo "  source: $HR_POOL_SOURCE"
    echo "driver: btrfs"
    exit 0
fi
if [ "$1" = storage ] && [ "$2" = set ]; then
    if [ -n "${HR_EXPECT_STORAGE_SET:-}" ] && [ "$*" != "storage set $HR_EXPECT_STORAGE_SET" ]; then
        echo "fake incus: unexpected storage set argv: $*" >&2
        exit 9
    fi
    printf 'storage set recorded: %s\n' "$*" >> "${HR_STORAGE_SET_LOG:-/dev/null}"
    if [ -n "${HR_POOL_STATE:-}" ] && [ "${HR_STORAGE_SET_READONLY:-0}" != 1 ]; then
        printf '%s\n' "${*#*size=}" >"$HR_POOL_STATE"
    fi
    exit 0
fi
if [ "$1" = network ] && [ "$2" = show ]; then
    [ "${HR_NETWORK_EXISTS:-0}" = 1 ] || { echo "Error: not found" >&2; exit 1; }
    exit 0
fi
if [ "$1" = network ] && [ "$2" = get ]; then
    [ -n "${HR_NETWORK_IPV4:-}" ] || { echo "Error: not found" >&2; exit 1; }
    printf '%s\n' "$HR_NETWORK_IPV4"
    exit 0
fi
echo "fake incus: unsupported invocation: $*" >&2
exit 1
STUB
chmod +x "$WORK/bin/incus"

# --- fake df: reports HR_DF_AVAIL_KIB for every -P query ---------------------
cat >"$WORK/bin/df" <<'STUB'
#!/bin/sh
avail=${HR_DF_AVAIL_KIB:-0}
echo "Filesystem 1024-blocks Used Available Capacity Mounted on"
echo "fakedev 100000000 1 $avail 1% /fakedir"
exit 0
STUB
chmod +x "$WORK/bin/df"

# --- fake ip: prints the scenario's route table from HR_ROUTES ---------------
cat >"$WORK/bin/ip" <<'STUB'
#!/bin/sh
if [ "${1:-}" = -4 ] && [ "${2:-}" = route ] && [ "${3:-}" = show ]; then
    [ "${HR_ROUTE_FAILURE:-0}" = 0 ] || exit 1
    printf '%s\n' "${HR_ROUTES:-}"
    exit 0
fi
echo "fake ip: unsupported invocation: $*" >&2
exit 1
STUB
chmod +x "$WORK/bin/ip"

# write_meminfo AVAILABLE_MIB: a fake /proc/meminfo via the library's
# documented tests-only override.
write_meminfo() {
    printf 'MemTotal: 16384000 kB\nMemFree: 1 kB\nMemAvailable: %d kB\n' \
        "$(( $1 * 1024 ))" >"$WORK/meminfo"
    CHASM_MEMINFO_FILE=$WORK/meminfo
    export CHASM_MEMINFO_FILE
}

# run_verdict FUNCTION [ARGS...]: run one lib verdict in a subshell, capturing
# status and combined output.
run_verdict() {
    status=0
    output=$("$@" 2>&1) || status=$?
}

assert_contains() { # haystack needle label
    case "$1" in
        *"$2"*) ;;
        *) die "$3: missing '$2' in:
$1" ;;
    esac
}

# inst NAME STATE MANAGED BUDGET LIMIT: one `incus list --format csv`
# inventory row, shaped as the admission reader parses it (name, state,
# managed mark, declared budget key and expanded limits.memory; empty
# arguments yield empty fields).
inst() {
    printf '%s,%s,%s,%s,%s\n' "$1" "$2" "$3" "$4" "$5"
}

# 1. Byte-size parsing: the shared grammar (plain bytes; KiB/MiB/GiB/TiB
# binary; kB/KB/MB/GB/TB decimal — 1536MB is 1536 x 10^6 bytes, which rounds
# up to 1465MiB), then fail-closed garbage: fractional numbers, percentages,
# bare-B and PiB spellings, leading zeros, and nonsense units.
check_size() { # value expected-mib
    got=$(incus_size_to_mib "$1" 2>/dev/null) ||
        die "size parse '$1' should succeed"
    [ "$got" = "$2" ] || die "size parse '$1' gave $got, expected $2"
}
check_size 3GiB 3072
check_size 512MiB 512
check_size 1073741824 1024
check_size 1536MB 1465
check_size 1000kB 1
check_size 2TiB 2097152
check_size 2000GB 1907349
for bad in '' '3GiX' '50%' 'GiB' 'three' '1.5GiB' '3B' '4PiB' '04GiB'; do
    incus_size_to_mib "$bad" >/dev/null 2>&1 &&
        die "size parse '$bad' must fail"
done

# 2. MemAvailable comes from the override path, not the host.
write_meminfo 4096
got=$(host_mem_available_mib) || die "meminfo read should succeed"
[ "$got" = 4096 ] || die "meminfo read gave $got, expected 4096"
CHASM_MEMINFO_FILE=$WORK/missing
export CHASM_MEMINFO_FILE
host_mem_available_mib >/dev/null 2>&1 && die "missing meminfo must fail"

# 3. Builder memory admission beside running instances. The fake meminfo
# carries MemTotal=16000MiB; every scenario below names its resource prefix
# and profile explicitly, and the fake `incus` refuses any list argv that
# does not match the exact column spec the admission must send, so a
# hard-coded prefix or a swapped/dropped column cannot pass silently.
write_meminfo 8192
HR_PROFILE_LIMITS=3GiB
export HR_PROFILE_LIMITS
HR_EXPECT_PROFILE=chasm
HR_LIST_PREFIX=chasm
export HR_EXPECT_PROFILE HR_LIST_PREFIX
run_verdict builder_memory_admission chasm 1024 256 chasm
[ "$status" -eq 0 ] || die "admission with 8192MiB available must pass:
$output"
assert_contains "$output" "MemAvailable=8192MiB" "admission numbers"
assert_contains "$output" "limits.memory=3072MiB" "admission builder limit"
assert_contains "$output" "need 4352MiB" "admission need"
assert_contains "$output" "running sandbox budgets=0MiB" "admission no budgets"

# A running sandbox's configured budget counts against capacity and is
# named in the verdict.
HR_INSTANCES=$(inst sandbox-demo RUNNING true 2048 "")
export HR_INSTANCES
write_meminfo 8192
run_verdict builder_memory_admission chasm 1024 256 chasm
[ "$status" -eq 0 ] || die "admission beside one 2048MiB sandbox must pass:
$output"
assert_contains "$output" "running sandbox budgets=2048MiB" "admission running budget"

# A stopped sandbox's budget is not capacity the builder must fit beside.
HR_INSTANCES=$(inst sandbox-demo STOPPED true 1048576 "")
export HR_INSTANCES
write_meminfo 8192
run_verdict builder_memory_admission chasm 1024 256 chasm
[ "$status" -eq 0 ] || die "admission beside a stopped sandbox must pass:
$output"
assert_contains "$output" "running sandbox budgets=0MiB" "stopped budget ignored"

# Capacity refusal: eight running budgets of 1048576MiB (8388608MiB in sum)
# cannot fit MemTotal=16000MiB even though MemAvailable alone would satisfy
# the old free-memory-only rule.
HR_INSTANCES="$(inst sandbox-a RUNNING true 1048576 ""
inst sandbox-b RUNNING true 1048576 ""
inst sandbox-c RUNNING true 1048576 ""
inst sandbox-d RUNNING true 1048576 ""
inst sandbox-e RUNNING true 1048576 ""
inst sandbox-f RUNNING true 1048576 ""
inst sandbox-g RUNNING true 1048576 ""
inst sandbox-h RUNNING true 1048576 "")"
export HR_INSTANCES
write_meminfo 15999
run_verdict builder_memory_admission chasm 1024 256 chasm
[ "$status" -eq 1 ] || die "admission whose budgets exceed MemTotal must refuse:
$output"
assert_contains "$output" "running sandbox budgets=8388608MiB" "capacity numbers"
assert_contains "$output" "exceeds MemTotal=16000MiB" "capacity refusal wording"

# A frozen sandbox still holds its budget, exactly as the executor counts
# Frozen as running for admission.
HR_INSTANCES=$(inst sandbox-demo FROZEN true 2048 "")
export HR_INSTANCES
write_meminfo 8192
run_verdict builder_memory_admission chasm 1024 256 chasm
[ "$status" -eq 0 ] || die "admission beside a frozen sandbox must pass:
$output"
assert_contains "$output" "running sandbox budgets=2048MiB" "frozen budget counted"

# Idiom 1: a drifted sandbox is counted at the LARGER of the declared key and
# the enforced expanded limits.memory. Declared 8192MiB beside the builder
# fits MemTotal=16000MiB by the declared-only sum (12544MiB), but the
# out-of-band `incus config set limits.memory 48GiB` raises the counted
# budget to 49152MiB and the capacity guard must refuse — exactly where the
# old declared-only counting would have admitted the build.
HR_INSTANCES=$(inst sandbox-drifted RUNNING true 8192 48GiB)
export HR_INSTANCES
run_verdict builder_memory_admission chasm 1024 256 chasm
[ "$status" -eq 1 ] || die "a drifted sandbox (declared 8192, enforced 48GiB) must refuse:
$output"
assert_contains "$output" "running sandbox budgets=49152MiB" "drifted budget counted at the larger side"
assert_contains "$output" "exceeds MemTotal=16000MiB" "drifted capacity refusal"

# The same declared budget with a matching enforced limit still admits: the
# larger-of rule, not the presence of limits.memory, drives the refusal.
HR_INSTANCES=$(inst sandbox-drifted RUNNING true 8192 8GiB)
export HR_INSTANCES
run_verdict builder_memory_admission chasm 1024 256 chasm
[ "$status" -eq 0 ] || die "a sandbox whose enforced limit matches its declared budget must be admitted:
$output"
assert_contains "$output" "running sandbox budgets=8192MiB" "matched enforced budget counted"

# A keyless sandbox whose enforced limit is present is counted at the
# enforced value (the default applies only when neither side is present).
HR_INSTANCES=$(inst sandbox-legacy RUNNING true "" 1GiB)
export HR_INSTANCES
write_meminfo 8192
run_verdict builder_memory_admission chasm 1024 256 chasm
[ "$status" -eq 0 ] || die "a keyless sandbox with an enforced 1GiB limit must be counted at 1024MiB:
$output"
assert_contains "$output" "running sandbox budgets=1024MiB" "keyless sandbox counted at enforced value"

# An enforced limits.memory the shared grammar cannot parse (the percentage
# form Incus also accepts) fails closed with the offending value named.
HR_INSTANCES=$(inst sandbox-pct RUNNING true 20480 50%)
export HR_INSTANCES
run_verdict builder_memory_admission chasm 1024 256 chasm
[ "$status" -eq 1 ] || die "an unsupported enforced limits.memory must fail closed:
$output"
assert_contains "$output" "unsupported enforced limits.memory (got '50%')" "unsupported enforced value naming"

# An enforced limits.memory that parses to a value outside the same bound
# the Go admission applies (a positive integer of at most 1048576 MiB) is
# unsupported too: the shell comparison arithmetic must never overflow.
HR_INSTANCES=$(inst sandbox-absurd RUNNING true 2048 9999999999999999TB)
export HR_INSTANCES
run_verdict builder_memory_admission chasm 1024 256 chasm
[ "$status" -eq 1 ] || die "an absurd enforced limits.memory must fail closed:
$output"
assert_contains "$output" "outside the supported 1..1048576MiB range" "absurd enforced value bound"

# A CSV row the admission cannot account for (a foreign, unmarked,
# non-sandbox name) fails closed: garbage inventory is never read as an
# empty, admitting inventory.
HR_INSTANCES='garbage-inventory,with,too,many,fields'
export HR_INSTANCES
run_verdict builder_memory_admission chasm 1024 256 chasm
[ "$status" -eq 1 ] || die "a malformed inventory row must fail closed:
$output"
assert_contains "$output" "garbage-inventory" "malformed inventory naming"
assert_contains "$output" "cannot account for" "malformed inventory refusal"

# A keyless running managed sandbox is counted at the protocol default
# (3072MiB), exactly as the executor's sandboxBudgetFromConfig accounts the
# same keyless legacy sandbox: the verdict names the defaulted budget and
# the build proceeds when the host can carry it. Stopping such a sandbox is
# never required for a template rebuild.
HR_INSTANCES=$(inst sandbox-legacy RUNNING true "" "")
export HR_INSTANCES
write_meminfo 8192
run_verdict builder_memory_admission chasm 1024 256 chasm
[ "$status" -eq 0 ] || die "a keyless running sandbox must be admitted at the protocol default:
$output"
assert_contains "$output" "running sandbox budgets=3072MiB" "defaulted budget in verdict"
assert_contains "$output" "sandbox-legacy" "defaulted sandbox naming"
assert_contains "$output" "counting the protocol default budget 3072MiB" "defaulted budget naming"
assert_contains "$output" "user.chasm.budget-memory-mib" "defaulted budget key naming"

# A present-but-non-canonical budget spelling is refused outright: the
# shell admission arithmetic reads a leading-zero constant as octal, and a
# signed value refuses every comparison.
for bad_budget in 01024 +2048; do
    HR_INSTANCES=$(inst sandbox-demo RUNNING true "$bad_budget" "")
    export HR_INSTANCES
    run_verdict builder_memory_admission chasm 1024 256 chasm
    [ "$status" -eq 1 ] || die "a non-canonical budget ($bad_budget) must fail closed:
$output"
    assert_contains "$output" "user.chasm.budget-memory-mib" "non-canonical budget key naming ($bad_budget)"
done

# Fail closed: a budget outside the protocol range cannot be accounted.
HR_INSTANCES=$(inst sandbox-demo RUNNING true 9 "")
export HR_INSTANCES
run_verdict builder_memory_admission chasm 1024 256 chasm
[ "$status" -eq 1 ] || die "an out-of-range budget must fail closed:
$output"
assert_contains "$output" "outside the supported 512..1048576MiB floor-and-safety-bound range" "budget range wording"

# Fail closed: an instance in an unaccountable state (e.g. ERROR) is
# refused, never skipped, exactly as the executor's listInstanceStatuses
# fails closed on a status outside Running/Frozen/Stopped.
HR_INSTANCES=$(inst sandbox-demo ERROR true 2048 "")
export HR_INSTANCES
run_verdict builder_memory_admission chasm 1024 256 chasm
[ "$status" -eq 1 ] || die "an ERROR-state instance must fail closed:
$output"
assert_contains "$output" "sandbox-demo" "ERROR-state instance naming"
assert_contains "$output" "state ERROR" "ERROR-state wording"

# Fail closed: when `incus list` itself fails, the inventory is unavailable
# and the admission refuses; it must never evaluate the capacity rule
# against a silently-zero sum.
HR_INSTANCES=$(inst sandbox-demo RUNNING true 2048 "")
HR_LIST_FAIL=1
export HR_INSTANCES HR_LIST_FAIL
run_verdict builder_memory_admission chasm 1024 256 chasm
[ "$status" -eq 1 ] || die "a failing incus list must fail closed:
$output"
assert_contains "$output" "running sandbox inventory is unavailable" "inventory failure wording"
unset HR_LIST_FAIL

# The capacity comparison runs against MemTotal, not MemAvailable: this host
# carries budgets of 8192MiB beside the builder on MemTotal=16000MiB while
# only 6000MiB are instantaneously available (capacity 12544MiB exceeds
# MemAvailable; the need 4352MiB does not).
HR_INSTANCES=$(inst sandbox-big RUNNING true 8192 "")
export HR_INSTANCES
write_meminfo 6000
run_verdict builder_memory_admission chasm 1024 256 chasm
[ "$status" -eq 0 ] || die "admission whose capacity passes MemTotal but exceeds MemAvailable must pass:
$output"
assert_contains "$output" "running sandbox budgets=8192MiB" "capacity-on-MemTotal budgets"
assert_contains "$output" "MemAvailable=6000MiB" "capacity-on-MemTotal availability"

# The capacity boundary is strict (-gt): an exact fit on MemTotal is
# admitted (11648MiB of budgets + 4352MiB of need = 16000MiB exactly), and
# one MiB more is refused.
HR_INSTANCES="$(inst sandbox-a RUNNING true 5824 ""
inst sandbox-b RUNNING true 5824 "")"
export HR_INSTANCES
write_meminfo 8192
run_verdict builder_memory_admission chasm 1024 256 chasm
[ "$status" -eq 0 ] || die "an exact-fit capacity on MemTotal must be admitted:
$output"
assert_contains "$output" "running sandbox budgets=11648MiB" "exact-fit budgets"
HR_INSTANCES="$(inst sandbox-a RUNNING true 5824 ""
inst sandbox-b RUNNING true 5825 "")"
export HR_INSTANCES
run_verdict builder_memory_admission chasm 1024 256 chasm
[ "$status" -eq 1 ] || die "capacity one MiB above MemTotal must refuse:
$output"
assert_contains "$output" "exceeds MemTotal=16000MiB" "exact-fit refusal wording"

# The profile and the resource prefix are distinct site keys: with profile
# chasm-prof and resource_prefix agent-sandbox, every inventory key and
# every refusal must name user.agent-sandbox.* and never a profile-derived
# key.
HR_PROFILE_LIMITS=3GiB
HR_EXPECT_PROFILE=chasm-prof
HR_LIST_PREFIX=agent-sandbox
HR_INSTANCES=$(inst sandbox-other RUNNING false 2048 "")
export HR_PROFILE_LIMITS HR_EXPECT_PROFILE HR_LIST_PREFIX HR_INSTANCES
run_verdict builder_memory_admission chasm-prof 1024 256 agent-sandbox
[ "$status" -eq 1 ] || die "an unmarked running sandbox under prefix agent-sandbox must fail closed:
$output"
assert_contains "$output" "not marked managed" "agent-sandbox unmarked wording"
assert_contains "$output" "user.agent-sandbox.managed" "agent-sandbox managed key naming"
HR_INSTANCES=$(inst sandbox-legacy RUNNING true "" "")
export HR_INSTANCES
write_meminfo 8192
run_verdict builder_memory_admission chasm-prof 1024 256 agent-sandbox
[ "$status" -eq 0 ] || die "a keyless running sandbox under prefix agent-sandbox must be admitted at the default:
$output"
assert_contains "$output" "running sandbox budgets=3072MiB" "agent-sandbox defaulted budget verdict"
assert_contains "$output" "user.agent-sandbox.budget-memory-mib" "agent-sandbox budget key naming"
HR_PROFILE_LIMITS=3GiB
HR_EXPECT_PROFILE=chasm
HR_LIST_PREFIX=chasm
export HR_PROFILE_LIMITS HR_EXPECT_PROFILE HR_LIST_PREFIX

# Fail closed: a running instance the site does not manage.
HR_INSTANCES=$(inst foreignvm RUNNING "" "" "")
export HR_INSTANCES
run_verdict builder_memory_admission chasm 1024 256 chasm
[ "$status" -eq 1 ] || die "a foreign running instance must fail closed:
$output"
assert_contains "$output" "foreignvm" "foreign instance naming"

# Fail closed: a sandbox-shaped name without the managed mark.
HR_INSTANCES=$(inst sandbox-other RUNNING "" "" "")
export HR_INSTANCES
run_verdict builder_memory_admission chasm 1024 256 chasm
[ "$status" -eq 1 ] || die "an unmarked running sandbox must fail closed:
$output"
assert_contains "$output" "not marked managed" "unmarked sandbox wording"

# Back to an empty inventory: the instantaneous MemAvailable guard refuses.
unset HR_INSTANCES
write_meminfo 2048
run_verdict builder_memory_admission chasm 1024 256 chasm
[ "$status" -eq 1 ] || die "admission with 2048MiB available must refuse:
$output"
assert_contains "$output" "MemAvailable=2048MiB" "refusal numbers"
assert_contains "$output" "insufficient host memory" "refusal wording"
assert_contains "$output" \
    "3072MiB + host_reserve_mib=1024MiB + margin=256MiB" "refusal breakdown"

HR_PROFILE_LIMITS=
export HR_PROFILE_LIMITS
run_verdict builder_memory_admission chasm 1024 256 chasm
[ "$status" -eq 1 ] || die "admission without limits.memory must fail closed:
$output"
assert_contains "$output" "unreadable" "unreadable limit wording"
unset HR_PROFILE_LIMITS

# 4. Preflight disk headroom. Margin is 10 GiB in KiB, as in the installer.
margin_kib=$((10 * 1048576))
pool_kib=$((64 * 1048576))

# 4a. No pool yet: the full rule, enough and not enough room.
HR_POOL_EXISTS=0
export HR_POOL_EXISTS
HR_DF_AVAIL_KIB=$((pool_kib + margin_kib + 1024))
export HR_DF_AVAIL_KIB
run_verdict disk_headroom_verdict chasm 64 10 /var/lib/incus
[ "$status" -eq 0 ] || die "fresh-pool headroom with room must pass:
$output"
assert_contains "$output" "does not exist yet" "fresh-pool rule"
HR_DF_AVAIL_KIB=$((pool_kib + margin_kib - 1024))
export HR_DF_AVAIL_KIB
run_verdict disk_headroom_verdict chasm 64 10 /var/lib/incus
[ "$status" -eq 1 ] || die "fresh-pool headroom without room must refuse:
$output"
assert_contains "$output" "insufficient" "fresh-pool refusal"

# 4b. Existing loop pool: a real sparse backing file (apparent 4 MiB, ~2 MiB
# allocated), so growth room is real stat output, not a shim.
loop_file=$WORK/chasm.img
dd if=/dev/zero of="$loop_file" bs=1024 count=2048 2>/dev/null
truncate -s 4M "$loop_file"
growth_kib=$(loop_file_growth_kib "$loop_file")
[ "$growth_kib" -gt 1024 ] && [ "$growth_kib" -lt 4096 ] ||
    die "sparse loop file growth room is unexpected: $growth_kib KiB"

HR_POOL_EXISTS=1
HR_POOL_SOURCE=$loop_file
export HR_POOL_EXISTS HR_POOL_SOURCE
HR_DF_AVAIL_KIB=$((margin_kib + growth_kib + 1024))
export HR_DF_AVAIL_KIB
run_verdict disk_headroom_verdict chasm 64 10 /var/lib/incus
[ "$status" -eq 0 ] || die "existing-pool headroom with growth room must pass:
$output"
assert_contains "$output" "existing pool chasm" "existing-pool rule"
assert_contains "$output" "can still grow into" "growth-rule wording"

HR_DF_AVAIL_KIB=$((margin_kib + 1024))
export HR_DF_AVAIL_KIB
run_verdict disk_headroom_verdict chasm 64 10 /var/lib/incus
[ "$status" -eq 1 ] || die "existing-pool headroom without growth room must refuse:
$output"
assert_contains "$output" "insufficient" "growth-rule refusal"
assert_contains "$output" "existing pool chasm" "growth-rule refusal rule"

# 4c. Existing but not loop-backed (a directory source): the full rule again.
mkdir -p "$WORK/pool-dir"
HR_POOL_SOURCE=$WORK/pool-dir
export HR_POOL_SOURCE
HR_DF_AVAIL_KIB=$((pool_kib + margin_kib))
export HR_DF_AVAIL_KIB
run_verdict disk_headroom_verdict chasm 64 10 /var/lib/incus
[ "$status" -eq 0 ] || die "non-loop pool headroom must pass with full room:
$output"
assert_contains "$output" "not loop-backed" "non-loop fallback rule"

# 5. Bridge-prefix collision: the site bridge's own route is accepted; any
# other overlapping route refuses.
site_route='10.77.0.0/24 dev chasmbr0 proto kernel scope link src 10.77.0.1'
HR_NETWORK_EXISTS=1
HR_NETWORK_IPV4=10.77.0.1/24
export HR_NETWORK_EXISTS HR_NETWORK_IPV4

HR_ROUTES="$site_route"
export HR_ROUTES
run_verdict prefix_route_verdict chasmbr0 10.77.0.1/24 10.77.0.
[ "$status" -eq 0 ] || die "site-bridge route must be accepted:
$output"
assert_contains "$output" "existing site bridge chasmbr0" "site-bridge wording"

HR_ROUTES="$site_route
10.77.0.0/24 dev eth9 proto kernel scope link src 10.77.0.9"
run_verdict prefix_route_verdict chasmbr0 10.77.0.1/24 10.77.0.
[ "$status" -eq 1 ] || die "a foreign overlapping route must refuse:
$output"
assert_contains "$output" "dev eth9" "foreign-route naming"

HR_ROUTES="$site_route
192.168.1.0/24 dev eth0 proto kernel scope link src 192.168.1.2"
run_verdict prefix_route_verdict chasmbr0 10.77.0.1/24 10.77.0.
[ "$status" -eq 0 ] || die "unrelated routes must not interfere:
$output"
assert_contains "$output" "existing site bridge chasmbr0" "site-bridge wording"

# An overlapping but different destination on the site bridge device
# itself must refuse: only the exact connected route implied by
# bridge_ipv4 (the network address with its /24 mask) is the site's own.
HR_ROUTES='10.77.0.128/25 dev chasmbr0 proto kernel scope link src 10.77.0.1'
export HR_ROUTES
run_verdict prefix_route_verdict chasmbr0 10.77.0.1/24 10.77.0.
[ "$status" -eq 1 ] || die "an overlapping subnet on the site bridge must refuse:
$output"
assert_contains "$output" "10.77.0.128/25 dev chasmbr0" "on-bridge subnet naming"
assert_contains "$output" "only the site bridge chasmbr0 (10.77.0.1/24) may hold the connected route 10.77.0.0/24" "connected-route wording"

# A shorter mask on the same network address is still not the connected
# /24 route.
HR_ROUTES='10.77.0.0/25 dev chasmbr0 proto kernel scope link src 10.77.0.1'
export HR_ROUTES
run_verdict prefix_route_verdict chasmbr0 10.77.0.1/24 10.77.0.
[ "$status" -eq 1 ] || die "a shorter mask on the site bridge must refuse:
$output"
assert_contains "$output" "10.77.0.0/25 dev chasmbr0" "shorter-mask naming"

# The site's connected route plus a foreign overlapping route on another
# device: the foreign one still refuses.
HR_ROUTES="$site_route
10.77.0.192/26 dev eth9 proto kernel scope link src 10.77.0.9"
export HR_ROUTES
run_verdict prefix_route_verdict chasmbr0 10.77.0.1/24 10.77.0.
[ "$status" -eq 1 ] || die "a foreign overlapping route must refuse beside the site route:
$output"
assert_contains "$output" "10.77.0.192/26 dev eth9" "foreign-route beside site route"

HR_ROUTES="$site_route
10.76.0.0/15 dev tun0"
run_verdict prefix_route_verdict chasmbr0 10.77.0.1/24 10.77.0.
[ "$status" -eq 1 ] || die "a covering supernet must refuse: $output"
assert_contains "$output" "10.76.0.0/15 dev tun0" "covering supernet naming"

HR_ROUTES="$site_route
10.74.0.0/15 dev tun0
blackhole 10.77.1.77"
run_verdict prefix_route_verdict chasmbr0 10.77.0.1/24 10.77.0.
[ "$status" -eq 0 ] || die "an adjacent network must be allowed: $output"

HR_ROUTE_FAILURE=1
export HR_ROUTE_FAILURE
run_verdict prefix_route_verdict chasmbr0 10.77.0.1/24 10.77.0.
[ "$status" -eq 1 ] || die "an unreadable route table must refuse: $output"
assert_contains "$output" "could not read the main IPv4 route table" "route read failure"
unset HR_ROUTE_FAILURE

# Restore the clean site route for the later scenarios.
HR_ROUTES=$site_route
export HR_ROUTES

# No Incus bridge at all (fresh host, or the name is taken by a plain
# interface): the same route is a collision again.
HR_NETWORK_EXISTS=0
export HR_NETWORK_EXISTS
run_verdict prefix_route_verdict chasmbr0 10.77.0.1/24 10.77.0.
[ "$status" -eq 1 ] || die "route without the Incus site bridge must refuse:
$output"
assert_contains "$output" "overlaps the sandbox prefix 10.77.0." "collision wording"

# A bridge that exists but no longer carries the descriptor's subnet.
HR_NETWORK_EXISTS=1
HR_NETWORK_IPV4=10.99.0.1/24
export HR_NETWORK_EXISTS HR_NETWORK_IPV4
run_verdict prefix_route_verdict chasmbr0 10.77.0.1/24 10.77.0.
[ "$status" -eq 1 ] || die "bridge with a mismatched subnet must refuse:
$output"
assert_contains "$output" "overlaps the sandbox prefix 10.77.0." "mismatch wording"

# Clean table, clean verdict.
HR_ROUTES='default via 192.168.1.1 dev eth0'
export HR_ROUTES
run_verdict prefix_route_verdict chasmbr0 10.77.0.1/24 10.77.0.
[ "$status" -eq 0 ] || die "clean route table must pass:
$output"
assert_contains "$output" "no host route overlaps" "clean wording"

# 6. Builder lifecycle-lock wiring (scripts/build-sandbox-template.sh): the
# admission snapshot and `incus start` are one critical section under the
# same exclusive /run/lock/chasm-lifecycle.lock the executor and the
# launcher take, so a concurrent `chasm start` cannot be admitted against
# headroom the build is about to consume; once the builder is RUNNING the
# executor refuses every start by itself, and the lock is released both on
# the success path and inside on_exit. Proven in three parts below. The
# builder never calls the launcher or the executor while holding the lock
# (it talks to Incus directly), so the lock cannot deadlock: the other two
# sides only ever wait for it.
BUILDER_SCRIPT=$ROOT/scripts/build-sandbox-template.sh
[ -f "$BUILDER_SCRIPT" ] || die "builder script is missing: $BUILDER_SCRIPT"

# 6a. The lock constants pin the shared lock file and the absolute flock
# binary (the launcher's choice), overridable only for this harness.
grep -q '^LIFECYCLE_LOCK=${CHASM_LIFECYCLE_LOCK:-/run/lock/chasm-lifecycle.lock}$' "$BUILDER_SCRIPT" ||
    die "build-sandbox-template.sh no longer locks /run/lock/chasm-lifecycle.lock by default"
grep -q '^FLOCK_BIN=${CHASM_FLOCK_BIN:-/usr/bin/flock}$' "$BUILDER_SCRIPT" ||
    die "build-sandbox-template.sh no longer uses /usr/bin/flock by default"

# 6b. The extracted acquire/release functions really lock: with the real
# flock behind a logging shim, a second exclusive acquire fails while the
# first is held, the lock file is created 0600 (the only mode the executor's
# withLifecycleLockAt accepts), the shim records -x 9 on acquire and -u 9 on
# release, release is idempotent, and an unacquirable lock fails closed.
[ -x /usr/bin/flock ] || die "/usr/bin/flock is required for the lock-wiring checks"
cat >"$WORK/bin/flock-logging" <<'STUB'
#!/bin/sh
printf '%s\n' "flock $*" >>"$FLOCK_LOG"
exec /usr/bin/flock "$@"
STUB
chmod +x "$WORK/bin/flock-logging"
# Extract the constants and both functions from the real builder script
# (anchor ranges, as the guest-layer harness does for its fragments): the
# first range stops at the acquire function's closing brace, the second
# copies the whole release function.
extracted=$WORK/builder-lock-functions.sh
sed -n '/^LIFECYCLE_LOCK=/,/^}/p' "$BUILDER_SCRIPT" >"$extracted"
sed -n '/^builder_release_lifecycle_lock() {/,/^}/p' "$BUILDER_SCRIPT" >>"$extracted"
grep -q '^builder_acquire_lifecycle_lock() {$' "$extracted" ||
    die "lock-function extraction lost builder_acquire_lifecycle_lock"
cat >>"$extracted" <<'EOF'
fail() {
    echo "builder lock probe: $*" >&2
    exit 1
}
EOF

FLOCK_LOG=$WORK/flock.log
export FLOCK_LOG
: >"$FLOCK_LOG"
probe_lock=$WORK/lifecycle-probe.lock
(
    CHASM_LIFECYCLE_LOCK=$probe_lock
    CHASM_FLOCK_BIN=$WORK/bin/flock-logging
    export CHASM_LIFECYCLE_LOCK CHASM_FLOCK_BIN
    # The extracted file is generated above from the real builder script.
    # shellcheck disable=SC1090
    . "$extracted"
    builder_acquire_lifecycle_lock || die "acquire must succeed on a fresh lock file"
    [ "$(stat -c %a "$probe_lock")" = 600 ] || die "the lifecycle lock file must be created 0600"
    if /usr/bin/flock -n "$probe_lock" true 2>/dev/null; then
        die "a second exclusive acquire must fail while the builder holds the lock"
    fi
    case "$(cat "$FLOCK_LOG")" in
        *'flock -x 9'*) ;;
        *) die "acquire must run flock -x 9, got: $(cat "$FLOCK_LOG")" ;;
    esac
    builder_release_lifecycle_lock
    builder_release_lifecycle_lock
    case "$(cat "$FLOCK_LOG")" in
        *'flock -u 9'*) ;;
        *) die "release must run flock -u 9, got: $(cat "$FLOCK_LOG")" ;;
    esac
    [ "$(grep -c 'flock -u 9' "$FLOCK_LOG")" -eq 1 ] ||
        die "release must be idempotent (exactly one -u), got: $(cat "$FLOCK_LOG")"
    if ! /usr/bin/flock -n "$probe_lock" true 2>/dev/null; then
        die "the lock must be free after release"
    fi
) || die "lock probe subshell failed"
(
    CHASM_LIFECYCLE_LOCK=$probe_lock
    CHASM_FLOCK_BIN=/bin/false
    export CHASM_LIFECYCLE_LOCK CHASM_FLOCK_BIN
    # The extracted file is generated above from the real builder script.
    # shellcheck disable=SC1090
    . "$extracted"
    builder_acquire_lifecycle_lock
) >/dev/null 2>&1 && die "an unacquirable lock must fail the build closed"

# 6c. The real script wires the calls in order: acquire strictly before the
# builder_memory_admission call, a success-path release strictly after
# `incus start` confirms the builder RUNNING, and a release inside on_exit
# so every failure path (init/start failure, HUP/INT/TERM) releases too.
acquire_line=$(grep -n '^builder_acquire_lifecycle_lock$' "$BUILDER_SCRIPT" | cut -d: -f1)
admission_line=$(grep -n 'builder_admission=$(builder_memory_admission' "$BUILDER_SCRIPT" | cut -d: -f1)
start_line=$(grep -n '^incus start "\$builder_name"$' "$BUILDER_SCRIPT" | cut -d: -f1)
release_line=$(grep -n '^builder_release_lifecycle_lock$' "$BUILDER_SCRIPT" | head -1 | cut -d: -f1)
[ -n "$acquire_line" ] || die "build-sandbox-template.sh lost its acquire call"
[ -n "$admission_line" ] || die "build-sandbox-template.sh lost its builder_memory_admission call"
[ -n "$start_line" ] || die "build-sandbox-template.sh lost its incus start call"
[ -n "$release_line" ] || die "build-sandbox-template.sh lost its release call"
[ "$acquire_line" -lt "$admission_line" ] ||
    die "the lifecycle lock must be acquired before builder_memory_admission"
[ "$admission_line" -lt "$start_line" ] ||
    die "builder_memory_admission must precede incus start"
[ "$start_line" -lt "$release_line" ] ||
    die "the lifecycle lock must be released after incus start (first standalone release)"
sed -n '/^on_exit() {/,/^trap on_exit/p' "$BUILDER_SCRIPT" | grep -q 'builder_release_lifecycle_lock' ||
    die "on_exit must release the lifecycle lock so every failure path releases it"

# 7. Pool growth through provision (idiom 3). The loop file reuses the real
# sparse backing file from section 4b, so the allocated bytes are real stat
# output. HR_EXPECT_STORAGE_SET pins the exact argv a provision run must
# use, HR_STORAGE_SET_LOG records the applied set, and HR_POOL_STATE gives
# the fake incus a durable size that `storage set` writes and `storage show`
# reads back, so the read-back proof is exercised for real.
allocated_kib=$(loop_file_allocated_kib "$loop_file")
[ "$allocated_kib" -gt 0 ] || die "loop file allocated KiB must be positive: $allocated_kib"

HR_POOL_EXISTS=1
HR_POOL_SIZE=64GiB
HR_POOL_SOURCE=$loop_file
HR_EXPECT_STORAGE_SHOW_POOL=chasm
export HR_POOL_EXISTS HR_POOL_SIZE HR_POOL_SOURCE HR_EXPECT_STORAGE_SHOW_POOL

# 7a. Equal: pool_size_gib matches the pool — nothing to do.
run_verdict pool_resize_plan chasm 64
[ "$status" -eq 0 ] || die "equal pool plan must be a no-op:
$output"
assert_contains "$output" "already 64GiB" "equal plan wording"

# 7b. Grow approved, then the host-backing verdict: the added ceiling
# (new size minus allocated bytes) plus the margin must fit in the free
# space beside the loop file.
run_verdict pool_resize_plan chasm 96
[ "$status" -eq 2 ] || die "a larger pool_size_gib must approve the grow:
$output"
assert_contains "$output" "will grow from 64GiB to 96GiB" "grow plan wording"

HR_DF_AVAIL_KIB=$((96 * 1048576 - allocated_kib + margin_kib + 1024))
export HR_DF_AVAIL_KIB
run_verdict pool_grow_verdict chasm 96 10 /var/lib/incus
[ "$status" -eq 0 ] || die "grow with enough backing must pass:
$output"
assert_contains "$output" "covers the needed" "grow verdict pass wording"
assert_contains "$output" "loop file" "grow verdict names the loop file"

HR_DF_AVAIL_KIB=$((96 * 1048576 - allocated_kib + margin_kib - 1024))
export HR_DF_AVAIL_KIB
run_verdict pool_grow_verdict chasm 96 10 /var/lib/incus
[ "$status" -eq 1 ] || die "grow without enough backing must refuse:
$output"
assert_contains "$output" "insufficient" "grow verdict refusal wording"

# 7b-fail-closed. An unreadable free-space figure refuses too.
HR_DF_AVAIL_KIB=0
unset HR_DF_AVAIL_KIB
df() { echo "df: broken" >&2; return 1; }
run_verdict pool_grow_verdict chasm 96 10 /var/lib/incus
unset -f df
[ "$status" -eq 1 ] || die "grow with unreadable backing must refuse:
$output"
assert_contains "$output" "unreadable" "grow verdict unreadable wording"

# 7b-non-loop. A pool that is not loop-backed is refused outright: chasm
# creates only loop-backed pools, so a pool without a loop file was
# precreated by an operator and its host backing cannot be proven.
HR_POOL_SOURCE=$WORK/pool-dir
export HR_POOL_SOURCE
run_verdict pool_grow_verdict chasm 96 10 /var/lib/incus
[ "$status" -eq 1 ] || die "growing a non-loop pool must refuse:
$output"
assert_contains "$output" "not loop-backed" "non-loop pool refusal wording"
assert_contains "$output" "chasm" "non-loop pool refusal names the pool"
HR_POOL_SOURCE=$loop_file
export HR_POOL_SOURCE

# 7c. Shrink refused with both numbers named.
run_verdict pool_resize_plan chasm 32
[ "$status" -eq 3 ] || die "a smaller pool_size_gib must refuse the shrink:
$output"
assert_contains "$output" "shrinking a storage pool is not supported" "shrink refusal wording"
assert_contains "$output" "pool is 64GiB, site asks for 32GiB" "shrink refusal numbers"

# 7d. An existing pool with no configured size is an operator-precreated,
# non-loop pool: re-provision must NOT fail — the plan reports that chasm
# does not manage the pool's size and pool_size_gib is not applied.
HR_POOL_SIZE=
export HR_POOL_SIZE
run_verdict pool_resize_plan chasm 64
[ "$status" -eq 0 ] || die "a pool without a configured size must not fail re-provision:
$output"
assert_contains "$output" "has no configured size" "unmanaged pool wording"
assert_contains "$output" "chasm does not manage its size" "unmanaged pool statement"
assert_contains "$output" "pool_size_gib is not applied" "unmanaged pool outcome"
HR_POOL_SIZE=64GiB
export HR_POOL_SIZE

# 7e. The REAL apply_pool_resize from scripts/install-chasm.sh, end to end:
# it must issue exactly `incus storage set chasm size=96GiB`, log the set,
# and prove the grow by reading the new size back through `storage show`.
INSTALLER=$ROOT/scripts/install-chasm.sh
[ -f "$INSTALLER" ] || die "installer script is missing: $INSTALLER"
sed -n '/^apply_pool_resize() {/,/^}/p' "$INSTALLER" >"$WORK/apply-fn.sh"
grep -q '^apply_pool_resize() {' "$WORK/apply-fn.sh" ||
    die "apply_pool_resize extraction lost the function"
cat >"$WORK/apply-stubs.sh" <<'EOF'
approve() { printf 'approve: %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
EOF
# run_apply_resize: run the extracted function with the lib in scope; stdin,
# stdout and stderr pass through so callers can capture output and status.
run_apply_resize() {
    # shellcheck disable=SC1090
    (
        . "$WORK/apply-stubs.sh"
        # shellcheck disable=SC1090
        . "$WORK/apply-fn.sh"
        apply_pool_resize
    )
}

SITE_STORAGE_POOL=chasm
SITE_POOL_SIZE_GIB=96
POOL_HEADROOM_GIB=10
HR_POOL_STATE=$WORK/pool.state
HR_EXPECT_STORAGE_SET='chasm size=96GiB'
HR_STORAGE_SET_LOG=$WORK/storage-set.log
HR_DF_AVAIL_KIB=$((96 * 1048576 + margin_kib + 1024))
export SITE_STORAGE_POOL SITE_POOL_SIZE_GIB POOL_HEADROOM_GIB \
    HR_POOL_STATE HR_EXPECT_STORAGE_SET HR_STORAGE_SET_LOG HR_DF_AVAIL_KIB
printf '64GiB\n' >"$HR_POOL_STATE"
: >"$HR_STORAGE_SET_LOG"
run_apply_resize >"$WORK/out" 2>"$WORK/err" ||
    die "apply_pool_resize must grow the pool: $(cat "$WORK/err")"
assert_contains "$(cat "$WORK/out")" "approve: grow the storage pool" "apply approval wording"
assert_contains "$(cat "$WORK/out")" "grown to 96GiB and proven by read-back" "apply read-back proof wording"
grep -Fx 'storage set recorded: storage set chasm size=96GiB' "$HR_STORAGE_SET_LOG" >/dev/null ||
    die "apply_pool_resize must issue exactly 'incus storage set chasm size=96GiB': $(cat "$HR_STORAGE_SET_LOG")"
[ "$(cat "$HR_POOL_STATE")" = 96GiB ] ||
    die "the grow must take effect: pool state is $(cat "$HR_POOL_STATE")"

# 7e-stale. The read-back proof must be real: when the set does not take
# effect (the fake incus logs it but leaves the pool size unchanged), the
# grow must fail instead of claiming success.
HR_STORAGE_SET_READONLY=1
export HR_STORAGE_SET_READONLY
printf '64GiB\n' >"$HR_POOL_STATE"
: >"$HR_STORAGE_SET_LOG"
run_apply_resize >"$WORK/out" 2>"$WORK/err" &&     die "a stale pool-size read-back must fail the grow: $(cat "$WORK/out")"
assert_contains "$(cat "$WORK/err")" "was not proven" "stale read-back refusal wording"
unset HR_STORAGE_SET_READONLY

# The applied size is the site size in GiB: a regression to MiB units fails
# the pinned argv check above (HR_EXPECT_STORAGE_SET='chasm size=96GiB'),
# which is exactly the mutation this section exists to catch.

# 7f. The REAL preflight Incus acquisition boundary from
# scripts/install-chasm.sh (incus_preflight_check) chained into the REAL
# install_incus_step: absent Incus fails the preflight closed without
# --install-incus (and the step refuses: core installs no packages); with
# --install-incus and the plugin present the preflight defers (no failure
# counted) and the step reaches the real plugin, which apt-installs incus
# AND virtiofsd from a trixie fixture; a missing plugin still fails both;
# a present Incus is never re-acquired (no apt call, INCUS_INSTALL_NEEDED
# stays 0) while a missing virtiofsd beside it stays a hard failure. No
# root, no live package actions: the apt-get shim only logs argv, the
# plugin's documented OS_RELEASE_FILE/APT_SOURCES_ROOT harness overrides
# point at fixtures, and VIRTIOFSD_LIBEXEC replaces the /usr/libexec
# probe. The HR incus fake from the earlier groups is removed here so
# presence is controlled by per-scenario PATH shims.
rm -f "$WORK/bin/incus"
sed -n '/^incus_preflight_check() {/,/^}/p' "$INSTALLER" >"$WORK/incus-preflight-fn.sh"
grep -q '^incus_preflight_check() {' "$WORK/incus-preflight-fn.sh" ||
    die "incus_preflight_check extraction lost the function"
sed -n '/^virtiofsd_probe() {/,/^}/p' "$INSTALLER" | sed "s|/usr/lib/virtiofsd|$WORK/absent-virtio-2|; s|/usr/lib/qemu/virtiofsd|$WORK/absent-virtio-3|" >"$WORK/incus-probe-fn.sh"
sed -n '/^incus_agent_probe() {/,/^}/p' "$INSTALLER" | sed "s|/usr/libexec/incus/incus-agent|$WORK/absent-agent-1|; s|/usr/lib/incus/incus-agent|$WORK/absent-agent-2|; s|/usr/libexec/incus-agent|$WORK/absent-agent-3|; s|/usr/lib/incus/agents|$WORK/agents-usrlib|; s|/usr/libexec/incus/agents|$WORK/agents-usrlibexec|; s|/usr/share/incus/agents|$WORK/agents-usrshare|" >>"$WORK/incus-probe-fn.sh"
grep -q '^virtiofsd_probe() {' "$WORK/incus-probe-fn.sh" ||
    die "virtiofsd_probe extraction lost the function"
sed -n '/^install_incus_step() {/,/^}/p' "$INSTALLER" >"$WORK/incus-step-fn.sh"
grep -q '^install_incus_step() {' "$WORK/incus-step-fn.sh" ||
    die "install_incus_step extraction lost the function"
cat >"$WORK/incus-stubs.sh" <<'EOF'
PREFLIGHT_FAILURES=0
INCUS_INSTALL_NEEDED=0
INCUS_INSTALL_PLUGIN=$ROOT/plugins/install-incus/incus-install.sh
p_ok()   { printf 'ok    %s\n' "$*"; }
p_note() { printf 'note  %s\n' "$*"; }
p_fail() { printf 'FAIL  %s\n' "$*"; PREFLIGHT_FAILURES=$((PREFLIGHT_FAILURES + 1)); }
fail() { printf 'FAIL  %s\n' "$*" >&2; exit 1; }
approve() { printf 'approve: %s\n' "$*"; }
template_instance_for_profile() { [ "${TEST_SEALED:-0}" = 1 ] && printf 'sandbox-template-test\n'; }
template_sealed() { [ "${TEST_SEALED:-0}" = 1 ]; }
SITE_BOOTSTRAP_PROFILE=test
EOF
cat >"$WORK/bin/apt-get" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >> "$INCUS_APT_LOG"
if [ "${INCUS_ACQUIRE_MOCK:-}" = both ] || [ "${INCUS_ACQUIRE_MOCK:-}" = incus-only ]; then
    printf '#!/bin/sh\nexit 0\n' >"$INCUS_ACQUIRED_BIN/incus"
    chmod 0755 "$INCUS_ACQUIRED_BIN/incus"
    if [ "$INCUS_ACQUIRE_MOCK" = both ]; then
        printf '#!/bin/sh\nexit 0\n' >"$INCUS_ACQUIRED_BIN/virtiofsd"
        printf '#!/bin/sh\nexit 0\n' >"$INCUS_ACQUIRED_BIN/incus-agent"
        chmod 0755 "$INCUS_ACQUIRED_BIN/virtiofsd" "$INCUS_ACQUIRED_BIN/incus-agent"
    fi
fi
EOF
chmod +x "$WORK/bin/apt-get"
: >"$WORK/incus-apt.log"
INCUS_APT_LOG=$WORK/incus-apt.log
INCUS_APT_LOG=$INCUS_APT_LOG export INCUS_APT_LOG
mkdir -p "$WORK/incus-apt-root"
mkdir -p "$WORK/incus-acquired-bin"
INCUS_ACQUIRED_BIN=$WORK/incus-acquired-bin
export INCUS_ACQUIRED_BIN
mkdir -p "$WORK/incus-step-bin"
ln -s "$WORK/bin/apt-get" "$WORK/incus-step-bin/apt-get"
for step_tool in sed head chmod; do
    ln -s "$(command -v "$step_tool")" "$WORK/incus-step-bin/$step_tool"
done
printf 'ID=debian\nVERSION_ID=13\n' >"$WORK/incus-os-release"
VIRTIOFSD_LIBEXEC=$WORK/incus-virtiofsd-absent
INSTALL_INCUS=0     # caller toggles; the stubs never reset it
mkdir -p "$WORK/incus-boundary-bin"
run_incus_boundary() {   # runs preflight check; caller pre-sets INSTALL_INCUS
    (
        # shellcheck disable=SC1090
        . "$WORK/incus-stubs.sh"
        # Extracted installer function.
        # shellcheck disable=SC1090
        . "$WORK/incus-probe-fn.sh"
        # shellcheck disable=SC1090
        . "$WORK/incus-preflight-fn.sh"
        export VIRTIOFSD_LIBEXEC
        # The extracted installer function reads this variable.
        # shellcheck disable=SC2034
        INCUS_INSTALL_PLUGIN=$1
        # Isolated PATH: presence of incus/virtiofsd is decided only by the
        # per-scenario shims in $WORK/incus-boundary-bin, never by the host.
        # Isolates command lookup from host Incus.
        # shellcheck disable=SC2123
        PATH=$WORK/incus-boundary-bin
        incus_preflight_check
        printf 'boundary failures=%s needed=%s\n' "$PREFLIGHT_FAILURES" "$INCUS_INSTALL_NEEDED"
    )
}
run_incus_step() {       # runs install_incus_step; caller pre-sets INSTALL_INCUS
    (
        # shellcheck disable=SC1090
        . "$WORK/incus-stubs.sh"
        # Extracted installer function.
        # shellcheck disable=SC1090
        . "$WORK/incus-probe-fn.sh"
        # shellcheck disable=SC1090
        . "$WORK/incus-step-fn.sh"
        export OS_RELEASE_FILE=$WORK/incus-os-release APT_SOURCES_ROOT=$WORK/incus-apt-root
        export VIRTIOFSD_LIBEXEC
        # The extracted installer function reads this variable.
        # shellcheck disable=SC2034
        INCUS_INSTALL_PLUGIN=$1
        INCUS_INSTALL_NEEDED=$2
        PATH=$INCUS_ACQUIRED_BIN:$WORK/incus-step-bin
        install_incus_step
        printf 'step done needed=%s\n' "$INCUS_INSTALL_NEEDED"
    )
}

# T1: absent Incus without --install-incus: preflight failure + step refusal.
out=$(run_incus_boundary "$ROOT/plugins/install-incus/incus-install.sh" 2>&1)
assert_contains "$out" "FAIL  incus is not installed" "T1 preflight fail wording"
assert_contains "$out" "boundary failures=1 needed=1" "T1 preflight counter"
status=0
out=$(run_incus_step "$ROOT/plugins/install-incus/incus-install.sh" 1 2>&1) || status=$?
if [ "$status" -eq 0 ]; then
    die "T1: the step must refuse without --install-incus: $out"
fi
assert_contains "$out" "core installs no packages" "T1 step refusal wording"
if [ -s "$INCUS_APT_LOG" ]; then
    die "T1: no apt call is allowed without --install-incus: $(cat "$INCUS_APT_LOG")"
fi

# T2: absent Incus with --install-incus and the plugin: preflight defers,
# the step reaches the real plugin and apt-installs incus virtiofsd.
INSTALL_INCUS=1
out=$(run_incus_boundary "$ROOT/plugins/install-incus/incus-install.sh" 2>&1)
assert_contains "$out" "note  incus is not installed; deferred" "T2 deferral wording"
assert_contains "$out" "boundary failures=0 needed=1" "T2 deferral counter"
: >"$INCUS_APT_LOG"
INCUS_ACQUIRE_MOCK=both
export INCUS_ACQUIRE_MOCK
out=$(run_incus_step "$ROOT/plugins/install-incus/incus-install.sh" 1 2>&1) ||
    die "T2: the step must reach the plugin: $out"
assert_contains "$out" "approve: install Incus from this Debian release's own repositories" "T2 approval wording"
assert_contains "$out" "step done needed=1" "T2 step completion"
grep -Fx 'install -y incus virtiofsd' "$INCUS_APT_LOG" >/dev/null ||
    die "T2: apt must install incus AND virtiofsd: $(cat "$INCUS_APT_LOG")"
rm -f "$INCUS_ACQUIRED_BIN/incus" "$INCUS_ACQUIRED_BIN/virtiofsd"
INCUS_ACQUIRE_MOCK=incus-only
status=0
out=$(run_incus_step "$ROOT/plugins/install-incus/incus-install.sh" 1 2>&1) || status=$?
[ "$status" -ne 0 ] || die "T2: acquisition without virtiofsd must fail: $out"
assert_contains "$out" "virtiofsd is missing after the install-incus acquisition step" "T2 post-acquisition probe"
rm -f "$INCUS_ACQUIRED_BIN/incus" "$INCUS_ACQUIRED_BIN/virtiofsd"
INCUS_ACQUIRE_MOCK=
status=0
out=$(run_incus_step "$ROOT/plugins/install-incus/incus-install.sh" 1 2>&1) || status=$?
[ "$status" -ne 0 ] || die "T2: acquisition without incus must fail: $out"
assert_contains "$out" "incus is missing after the install-incus acquisition step" "T2 post-acquisition incus probe"

# T3: absent Incus with --install-incus but the plugin missing: both fail closed.
out=$(run_incus_boundary "$WORK/incus-plugin-missing.sh" 2>&1)
assert_contains "$out" "boundary failures=1 needed=1" "T3 preflight failure counter"
status=0
out=$(run_incus_step "$WORK/incus-plugin-missing.sh" 1 2>&1) || status=$?
if [ "$status" -eq 0 ]; then
    die "T3: the step must refuse without the plugin: $out"
fi
assert_contains "$out" "install-incus plugin, which is not present" "T3 step refusal wording"
# Read by the extracted installer function.
# shellcheck disable=SC2034
INSTALL_INCUS=0

# T4: present Incus and virtiofsd: ok, no acquisition, no apt call.
printf '#!/bin/sh\necho /usr/bin/incus\n' >"$WORK/incus-boundary-bin/incus"
printf '#!/bin/sh\necho /usr/bin/virtiofsd\n' >"$WORK/incus-boundary-bin/virtiofsd"
printf '#!/bin/sh\nexit 0\n' >"$WORK/incus-boundary-bin/incus-agent"
chmod +x "$WORK/incus-boundary-bin/incus" "$WORK/incus-boundary-bin/virtiofsd" "$WORK/incus-boundary-bin/incus-agent"
: >"$INCUS_APT_LOG"
out=$(run_incus_boundary "$ROOT/plugins/install-incus/incus-install.sh" 2>&1)
assert_contains "$out" "ok    incus present" "T4 incus present wording"
assert_contains "$out" "ok    virtiofsd present" "T4 virtiofsd present wording"
assert_contains "$out" "ok    Incus host agent present" "T4 Incus agent present"
assert_contains "$out" "boundary failures=0 needed=0" "T4 no acquisition needed"
out=$(run_incus_step "$ROOT/plugins/install-incus/incus-install.sh" 0 2>&1) ||
    die "T4: the step must be a no-op when Incus is present: $out"
if [ -s "$INCUS_APT_LOG" ]; then
    die "T4: a present Incus must never be re-acquired: $(cat "$INCUS_APT_LOG")"
fi

# T5: present Incus, missing virtiofsd: hard preflight failure, no deferral.
rm -f "$WORK/incus-boundary-bin/virtiofsd"
out=$(run_incus_boundary "$ROOT/plugins/install-incus/incus-install.sh" 2>&1)
assert_contains "$out" "FAIL  virtiofsd is missing" "T5 virtiofsd refusal wording"
assert_contains "$out" "boundary failures=1 needed=0" "T5 virtiofsd counter"
mv "$WORK/incus-boundary-bin/incus-agent" "$WORK/incus-boundary-bin/virtiofsd"
out=$(run_incus_boundary "$ROOT/plugins/install-incus/incus-install.sh" 2>&1)
assert_contains "$out" "FAIL  Incus host agent is missing" "T6 missing Incus agent refusal"
assert_contains "$out" "boundary failures=1 needed=0" "T6 Incus agent counter"
TEST_SEALED=1
export TEST_SEALED
out=$(run_incus_boundary "$ROOT/plugins/install-incus/incus-install.sh" 2>&1)
assert_contains "$out" "WARNING: Incus host agent is missing" "T6 existing sealed template warning"
assert_contains "$out" "boundary failures=0 needed=0" "T6 existing sealed template allowed"
unset TEST_SEALED
rm -f "$WORK/incus-boundary-bin/incus"

sed -n '/^go_version_at_least() {/,/^}/p' "$INSTALLER" >"$WORK/portability-go.sh"
(
    # shellcheck disable=SC1090
    . "$WORK/portability-go.sh"
    go_version_at_least go1.23.0 1.23 && go_version_at_least go1.100.1 1.23 &&
        go_version_at_least go1.27.4-X:nodwarf5 1.23 && go_version_at_least go1.24rc1 1.23 ||
        die "Go minimum rejected a supported version or suffix"
    if go_version_at_least go1.22.99 1.23 || go_version_at_least go1.22rc1 1.23 ||
        go_version_at_least garbage 1.23 || go_version_at_least go 1.23; then
        die "Go minimum accepted an old, empty, or garbage version"
    fi
)
(
    # shellcheck disable=SC1090
    . "$WORK/incus-probe-fn.sh"
    mkdir -p "$WORK/agents-usrlib" "$WORK/agent-path" "$WORK/incus-agent-bin"
    cat >"$WORK/incus-agent-bin/uname" <<'EOF'
#!/bin/sh
printf '%s\n' x86_64
EOF
    /bin/chmod +x "$WORK/incus-agent-bin/uname"
    # shellcheck disable=SC2123
    PATH=$WORK/incus-agent-bin
    printf '#!/bin/sh\nexit 0\n' >"$WORK/agents-usrlib/incus-agent.linux.x86_64"
    /bin/chmod +x "$WORK/agents-usrlib/incus-agent.linux.x86_64"
    incus_agent_path=
    unset INCUS_AGENT_PATH
    incus_agent_probe &&
        [ "$incus_agent_path" = "$WORK/agents-usrlib/incus-agent.linux.x86_64" ] ||
        die "Incus architecture-specific agents directory was not detected"
    /bin/rm -f "$WORK/agents-usrlib/incus-agent.linux.x86_64"
    printf '#!/bin/sh\nexit 0\n' >"$WORK/agent-path/incus-agent.linux.x86_64"
    /bin/chmod +x "$WORK/agent-path/incus-agent.linux.x86_64"
    INCUS_AGENT_PATH=$WORK/agent-path
    export INCUS_AGENT_PATH
    incus_agent_path=
    incus_agent_probe && [ "$incus_agent_path" = "$WORK/agent-path/incus-agent.linux.x86_64" ] ||
        die "INCUS_AGENT_PATH architecture-specific agent was not detected"
)
(
    # shellcheck disable=SC1090
    . "$WORK/incus-probe-fn.sh"
    # Only the fixture tools are visible in the fallback-path tests.
    # shellcheck disable=SC2123
    PATH=$WORK/incus-step-bin
    virtiofsd_path=
    for active_index in 1 2 3; do
        /bin/rm -f "$WORK/virtio-libexec" "$WORK/virtio-lib" "$WORK/virtio-qemu"
        set -- "$WORK/virtio-libexec" "$WORK/virtio-lib" "$WORK/virtio-qemu"
        case "$active_index" in
            1) active=$1 ;;
            2) active=$2 ;;
            3) active=$3 ;;
        esac
        printf '#!/bin/sh\nexit 0\n' >"$active"
        chmod +x "$active"
        virtiofsd_probe "$@" && [ "$virtiofsd_path" = "$active" ] || die "virtiofsd fallback $active_index was not found"
    done
)

# Exercise the initializer's actual EXIT trap in a child shell. Its normal
# path clears the preseed filename before exiting.
awk '/^preseed_tmp=$/{copy=1} copy && /^incus admin waitready/{exit} copy{print}' "$ROOT/scripts/initialize-incus.sh" > "$WORK/initializer-cleanup.sh"
sh -ec '. "$1"' sh "$WORK/initializer-cleanup.sh" ||
    die "initializer cleanup changed a successful exit into a failure"
preseed="$WORK/initializer-preseed"
touch "$preseed"
status=0
sh -ec '. "$1"; preseed_tmp=$2; exit 7' sh "$WORK/initializer-cleanup.sh" "$preseed" || status=$?
[ "$status" = 7 ] || die "initializer cleanup obscured the failing exit status"
[ ! -e "$preseed" ] || die "initializer cleanup did not remove the preseed"

touch "$preseed"
status=0
sh -ec '. "$1"; preseed_tmp=$2; kill -TERM "$$"; exit 99' sh "$WORK/initializer-cleanup.sh" "$preseed" || status=$?
[ "$status" = 143 ] || die "initializer continued after TERM or obscured the signal status"
[ ! -e "$preseed" ] || die "initializer signal path did not remove the preseed"

# A reprovision installs new firewall files before this phase. An active
# unit must reload the rules, and a reload failure must stop provisioning.
awk '/^initialize_incus\(\) \{/{copy=1} copy{print} copy && /^}/{exit}' \
    "$ROOT/scripts/install-chasm.sh" > "$WORK/initialize-phase.sh"
run_firewall_phase() (
    . "$WORK/initialize-phase.sh"
    SITE_STORAGE_POOL=trial
    export SITE_BRIDGE_NAME=trialbr0
    incus() { return 0; }
    apply_pool_resize() { return 0; }
    approve() { return 0; }
    fail() { echo "$*"; exit 1; }
    systemctl() {
        case "$*" in
            'is-active chasm-firewall.service') echo "$FIREWALL_STATE" ;;
            'is-enabled chasm-firewall.service') echo disabled ;;
            'enable chasm-firewall.service') echo "enable at boot" ;;
            'reload chasm-firewall.service') echo "reload"; return "$RELOAD_STATUS" ;;
            'enable --now chasm-firewall.service') echo "enable" ;;
            *) return 99 ;;
        esac
    }
    FIREWALL_STATE=$1
    RELOAD_STATUS=$2
    initialize_incus
)
out=$(run_firewall_phase active 0) || die "active firewall reload refused"
assert_contains "$out" reload "active firewall reload"
assert_contains "$out" "enable at boot" "enable an active disabled firewall"
out=$(run_firewall_phase inactive 0) || die "inactive firewall enable refused"
assert_contains "$out" enable "inactive firewall enable"
if out=$(run_firewall_phase active 1); then
    die "firewall reload failure allowed provisioning to continue"
fi
assert_contains "$out" "reloading the active firewall failed" "firewall reload failure"

# Verification must check the actual hook and first verdict.
awk '/^firewall_chain_verified\(\) \{/{copy=1} copy{print} copy && /^}/{exit}' \
    "$ROOT/scripts/install-chasm.sh" >"$WORK/chain-verification.sh"
verify_chain_fixture() (
    . "$WORK/chain-verification.sh"
    SITE_BRIDGE_NAME=trialbr0
    nft() { printf '%s\n' "$fixture"; }
    fixture=$(printf '%s\n' 'table inet chasm {' 'chain input {' "$1" "$2" \
        'iifname "trialbr0" meta nfproto ipv6 counter drop' '}' '}')
    firewall_chain_verified input
)
verify_chain_fixture 'type filter hook input priority -10; policy accept;' '' || die "valid firewall chain refused"
if verify_chain_fixture 'type filter hook input priority -10; policy accept;' 'accept'; then
    die "shadowed IPv6 firewall drop accepted"
fi
if verify_chain_fixture '' ''; then
    die "detached firewall chain accepted"
fi

# Verify that a present bridge with different addressing is still drift.
awk '/^    if incus network show "\$SITE_BRIDGE_NAME"/{copy=1} copy{print} copy && /^    fi$/{exit}' \
    "$ROOT/scripts/install-chasm.sh" > "$WORK/bridge-verification.sh"
run_bridge_verification() (
    export SITE_BRIDGE_NAME=trialbr0 SITE_BRIDGE_IPV4=10.42.0.1/24
    ACTUAL_IPV4=$1
    ACTUAL_IPV6=$2
    incus() {
        case "$*" in
            'network show trialbr0') return 0 ;;
            'network get trialbr0 ipv4.address') printf '%s\n' "$ACTUAL_IPV4" ;;
            'network get trialbr0 ipv6.address') printf '%s\n' "$ACTUAL_IPV6" ;;
            *) return 99 ;;
        esac
    }
    ok() { echo "ok $*"; }
    drift() { echo "drift $*"; }
    . "$WORK/bridge-verification.sh"
)
out=$(run_bridge_verification 10.42.0.1/24 none)
assert_contains "$out" "ok incus bridge IPv4 matches" "matching IPv4"
assert_contains "$out" "ok incus bridge IPv6 addressing is disabled" "matching IPv6"
out=$(run_bridge_verification 10.42.0.9/24 fd00::1/64)
assert_contains "$out" "drift incus bridge IPv4 differs" "changed IPv4"
assert_contains "$out" "drift incus bridge IPv6 differs" "enabled IPv6"

# A failed first boot must release the newly created builder's address.
# An explicit resume target and a previously booted builder are retained.
awk '/^on_exit\(\) \{/{copy=1} copy{print} copy && /^trap .* TERM$/{exit}' \
    "$ROOT/scripts/build-sandbox-template.sh" > "$WORK/builder-cleanup.sh"
run_builder_cleanup() (
    export builder_created=1
    export builder_booted=$1
    export resume_builder=$2
    export committed=0
    export template_moved=0
    export builder_name=trial-builder
    receipt=$WORK/builder-receipt
    : > "$receipt"
    incus() {
        case "$*" in
            'info trial-builder') echo "Status: STOPPED" ;;
            'delete trial-builder') echo deleted >> "$WORK/builder-deletions" ;;
            *) return 99 ;;
        esac
    }
    builder_release_lifecycle_lock() { :; }
    . "$WORK/builder-cleanup.sh"
    exit 7
)
status=0
run_builder_cleanup 0 "" || status=$?
[ "$status" = 7 ] || die "discarding a failed builder changed the failure status"
grep -qx 'discarded_unbooted_builder=trial-builder' "$WORK/builder-receipt" ||
    die "unbooted builder was retained"
status=0
run_builder_cleanup 0 trial-builder || status=$?
[ "$status" = 7 ] || die "resume cleanup changed the failure status"
grep -qx 'preserved_builder=trial-builder' "$WORK/builder-receipt" ||
    die "explicit resume builder was discarded"
status=0
run_builder_cleanup 1 "" || status=$?
[ "$status" = 7 ] || die "post-boot cleanup changed the failure status"
grep -qx 'preserved_builder=trial-builder' "$WORK/builder-receipt" ||
    die "previously booted builder was discarded"
[ "$(wc -l < "$WORK/builder-deletions")" -eq 1 ] ||
    die "cleanup deleted more than the newly created unbooted builder"

status=0
receipt=$WORK/interrupted-builder-receipt
sh -ec '
    receipt=$2
    builder_created=0 committed=0 template_moved=0
    builder_release_lifecycle_lock() { :; }
    . "$1"
    kill -TERM "$$"
    exit 99
' sh "$WORK/builder-cleanup.sh" "$receipt" || status=$?
[ "$status" = 143 ] || die "template builder continued after TERM or changed the signal status"
grep -qx 'exit_status=143' "$receipt" || die "template builder did not record its signal status"

# A small new pool can choose a smaller default root disk. An impossible
# preseed must fail before emitting a document for Incus to apply.
root_disk_fixture=$ROOT/testdata/fixtures/descriptor-cases/root-disk-ok.conf
sed 's/^pool_size_gib=64$/pool_size_gib=24/' "$root_disk_fixture" > "$WORK/root-disk-site.conf"
out=$(CHASM_SITE_CONF="$WORK/root-disk-site.conf" "$ROOT/scripts/render-incus-preseed.sh") ||
    die "smaller matching pool and root disk could not render"
assert_contains "$out" "size: 16GiB" "configured profile root size"
sed 's/^pool_size_gib=64$/pool_size_gib=8/' "$root_disk_fixture" > "$WORK/root-disk-site.conf"
if CHASM_SITE_CONF="$WORK/root-disk-site.conf" "$ROOT/scripts/render-incus-preseed.sh" \
    >"$WORK/root-disk-out" 2>"$WORK/root-disk-error"; then
    die "preseed accepted a root disk larger than its new pool"
fi
[ ! -s "$WORK/root-disk-out" ] || die "invalid preseed produced output"
assert_contains "$(cat "$WORK/root-disk-error")" "root disk needs 16GiB" "root disk exceeds pool"

echo "test-provision-rerun: ok (12 scenario groups)"
