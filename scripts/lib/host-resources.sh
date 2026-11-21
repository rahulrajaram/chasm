#!/bin/sh

# Host resource admission for chasm provisioning steps.
#
# Source this file (it defines functions only and never runs anything on its
# own) to get read-only probes and admission verdicts shared by
# scripts/install-chasm.sh (preflight disk headroom and bridge-prefix
# collision) and scripts/build-sandbox-template.sh (the builder VM memory
# budget). Nothing here mutates Incus, the firewall, or any instance: the
# verdicts exist so provisioning steps admit work on a live site — one whose
# sandboxes may be RUNNING — instead of refusing whenever anything runs.
#
# Test overrides (used only by tests; never set on a provisioned host):
#   CHASM_MEMINFO_FILE   path to a fake /proc/meminfo (default /proc/meminfo)
#
# Every MiB sum in this library is computed through hr_add_mib, which refuses
# when the result would leave the shell's signed 64-bit integer range: plain
# $((...)) addition wraps there (a wrapped negative need would invert the
# admission comparisons), so overflow always fails closed.

# The memory budget counted for a running managed sandbox whose
# user.<prefix>.budget-memory-mib key is absent or empty. This must equal
# protocol.DefaultSandboxBudgetMemoryMiB (internal/protocol/protocol.go):
# sandboxBudgetFromConfig (internal/execserver/handlers.go) applies that
# default to the same keyless legacy instances, so the builder admission and
# the executor's start admission account for them identically. A parity test
# (internal/execserver/shell_budget_parity_test.go) greps this constant and
# fails when the two drift.
HR_DEFAULT_SANDBOX_BUDGET_MIB=3072

# hr_is_mib VALUE: true iff VALUE is a nonnegative decimal integer that fits
# the shell's signed 64-bit arithmetic, so it is safe to add and compare
# without wrapping.
hr_is_mib() {
    case "$1" in
        ''|*[!0-9]*) return 1 ;;
    esac
    if [ "${#1}" -ge 19 ]; then
        # Lexical comparison is exact for fixed-length decimal strings, so
        # awk keeps this deterministic without shell arithmetic overflow.
        hr_bound="$1" awk 'BEGIN {
            v = ENVIRON["hr_bound"]
            if (length(v) > 19 || (length(v) == 19 && v > "9223372036854775807")) exit 1
        }' || return 1
    fi
    return 0
}

# hr_add_mib A B: print A+B for two validated nonnegative MiB integers, or
# fail when the sum would overflow the shell's signed 64-bit range.
hr_add_mib() {
    if [ "$1" -gt $((9223372036854775807 - $2)) ]; then
        return 1
    fi
    echo $(( $1 + $2 ))
}

# host_mem_total_mib: print MemTotal from the meminfo file as whole MiB
# (floor), or fail with a named reason. The capacity rule compares against
# the host's total memory, exactly as the executor's start admission does.
host_mem_total_mib() {
    hr_meminfo=${CHASM_MEMINFO_FILE:-/proc/meminfo}
    hr_total_kib=$(awk '/^MemTotal:/ {print $2; exit}' "$hr_meminfo" 2>/dev/null)
    case "$hr_total_kib" in
        ''|*[!0-9]*)
            echo "host resources: MemTotal is missing or not numeric in $hr_meminfo" >&2
            return 1
            ;;
    esac
    awk -v kib="$hr_total_kib" 'BEGIN { printf "%d\n", int(kib / 1024) }'
}

# host_mem_available_mib: print MemAvailable from the meminfo file as whole
# MiB (floor, the conservative direction), or fail with a named reason.
host_mem_available_mib() {
    hr_meminfo=${CHASM_MEMINFO_FILE:-/proc/meminfo}
    hr_available_kib=$(awk '/^MemAvailable:/ {print $2; exit}' "$hr_meminfo" 2>/dev/null)
    case "$hr_available_kib" in
        ''|*[!0-9]*)
            echo "host resources: MemAvailable is missing or not numeric in $hr_meminfo" >&2
            return 1
            ;;
    esac
    awk -v kib="$hr_available_kib" 'BEGIN { printf "%d\n", int(kib / 1024) }'
}

# incus_size_to_mib VALUE: parse one Incus byte-size value into whole MiB,
# rounding up so an odd number never under-admits. The grammar is the one
# contract shared with Go's parseMemoryLimitBytes
# (internal/execserver/handlers.go):
#
#	size    := bytes | binary | decimal
#	bytes   := digits                          (a plain count of bytes)
#	binary  := digits ( 'KiB' | 'MiB' | 'GiB' | 'TiB')  (units of 1024)
#	decimal := digits ( 'kB' | 'KB' | 'MB' | 'GB' | 'TB') (units of 1000)
#	digits  := '0' | [1-9][0-9]*                (no sign, no leading zeros)
#
# Anything else — including the percentage form limits.memory also allows,
# fractional numbers, and the PiB/PB/EiB/EB and bare-B spellings — is
# unsupported and fails closed.
incus_size_to_mib() {
    printf '%s\n' "$1" | awk '
        {
            value = $0
            if (match(value, /^[0-9]+$/)) {
                number = value
                unit = ""
            } else if (match(value, /^[0-9]+/)) {
                number = substr(value, 1, RLENGTH)
                unit = substr(value, RLENGTH + 1)
            } else {
                print "host resources: byte size is not numeric: " value > "/dev/stderr"
                exit 1
            }
            # A leading zero (04GiB) is not in the shared grammar: the Go
            # parser refuses it, and the shell admission arithmetic
            # would read it as octal.
            if (number != "0" && substr(number, 1, 1) == "0") {
                print "host resources: byte size has a leading zero: " value > "/dev/stderr"
                exit 1
            }
            multiplier = 1
            if (unit == "") multiplier = 1
            else if (unit == "KiB") multiplier = 1024
            else if (unit == "MiB") multiplier = 1048576
            else if (unit == "GiB") multiplier = 1073741824
            else if (unit == "TiB") multiplier = 1099511627776
            else if (unit == "kB" || unit == "KB") multiplier = 1000
            else if (unit == "MB") multiplier = 1000000
            else if (unit == "GB") multiplier = 1000000000
            else if (unit == "TB") multiplier = 1000000000000
            else {
                print "host resources: byte size has an unsupported unit: " value > "/dev/stderr"
                exit 1
            }
            bytes = number * multiplier
            mib = int(bytes / 1048576)
            if (bytes % 1048576 != 0) mib = mib + 1
            printf "%d\n", mib
        }'
}

# incus_profile_memory_mib PROFILE: print the limits.memory of the Incus
# profile the bounded builder VM launches with, in MiB, or fail. The live
# profile is the authority (it is what the builder actually runs under), not
# the repository's preseed template.
incus_profile_memory_mib() {
    hr_profile=$1
    hr_limits=$(incus profile get "$hr_profile" limits.memory 2>&1) || {
        echo "host resources: incus profile get $hr_profile limits.memory failed: $hr_limits" >&2
        return 1
    }
    case "$hr_limits" in
        '')
            echo "host resources: profile $hr_profile does not set limits.memory" >&2
            return 1
            ;;
    esac
    hr_mib=$(incus_size_to_mib "$hr_limits" 2>&1) || {
        echo "host resources: profile $hr_profile limits.memory '$hr_limits' is not a parseable byte size: $hr_mib" >&2
        return 1
    }
    printf '%s\n' "$hr_mib"
}

# running_sandbox_budgets_mib RESOURCE_PREFIX: print the sum of the memory
# budgets (MiB) the executor's start admission would count for every RUNNING
# or FROZEN managed sandbox of this site, or fail closed. Managed sandboxes
# are identified the same way the executor's start admission identifies them
# (internal/execserver/handlers.go): an Incus instance named sandbox-<logical>
# whose expanded config carries user.<prefix>.managed=true, with the budget
# declared in user.<prefix>.budget-memory-mib inside the protocol floor 512
# and the arithmetic-safety bound 1048576 MiB (there is no fixed product cap;
# the host-derived cap is host memory minus host_reserve_mib, idiom 4).
# The counted budget is the larger of the declared key and the enforced
# expanded limits.memory (idiom 1: a sandbox changed behind chasm's back is
# in drift, and admission counts the side that commits more host memory),
# exactly as the executor's admissibleMemoryBudget computes it. The inventory
# is one `incus list -c n,s,<managed key>,<budget key>,limits.memory
# --format csv` call: Incus reads the -c config columns from the EXPANDED
# config (an instance with no limits.memory of its own reports the profile's
# value), the instance-name and status columns carry no commas, and the flat
# CSV rows are read with a plain IFS=, read — no JSON scanner and no jq on
# the host. The enforced limits.memory is parsed by
# incus_size_to_mib, whose grammar is the one contract shared with Go's
# parseMemoryLimitBytes; a value Go would refuse (for example the percentage
# form, a sign, or a leading-zero spelling) fails closed here too, with the
# offending value named — its memory cannot be accounted, so the builder
# must not start beside it. A parsed enforced value is validated against the
# same bound the Go admission applies — a positive integer of at most
# 1048576 MiB, so the comparison arithmetic cannot overflow — and anything
# outside that bound fails closed. A declared
# key that is absent or empty counts as the protocol default
# (HR_DEFAULT_SANDBOX_BUDGET_MIB) only when the enforced limit is absent as
# well, exactly as the executor defaults the same keyless legacy sandboxes;
# the defaulted budget is named on stderr. A present-but-unreadable or
# out-of-range declared budget fails closed — its memory cannot be accounted,
# so the builder must not start beside it. Any other RUNNING or FROZEN
# instance (a foreign VM this site does not manage) also fails closed: like
# the executor's start gate, the admission refuses to reason past an
# inventory it cannot account for, and an instance in any other state (e.g.
# ERROR) is refused, never skipped — listInstanceStatuses fails closed the
# same way. An `incus list` that fails, or a CSV inventory whose rows the
# admission cannot account for, fails closed (the inventory is captured with
# its own status before any parsing, so a failed list is never read as an
# empty, admitting inventory).
running_sandbox_budgets_mib() {
    hr_prefix=$1
    hr_managed_key=$(printf 'user.%s.managed' "$hr_prefix")
    hr_budget_key=$(printf 'user.%s.budget-memory-mib' "$hr_prefix")
    # One CSV inventory row per instance: name, status, the two expanded
    # user keys, and the enforced limits.memory.
    hr_rows=$(incus list -c "n,s,$hr_managed_key,$hr_budget_key,limits.memory" --format csv 2>/dev/null) || {
        printf '%s\n' "the running sandbox inventory is unavailable; the builder memory admission fails closed"
        return 1
    }
    hr_status=0
    hr_out=$(printf '%s\n' "$hr_rows" | {
        hr_sum=0
        while IFS=, read -r hr_name hr_state hr_managed hr_budget hr_limit; do
            [ -n "$hr_name" ] || continue
            case "$hr_state" in
                RUNNING|FROZEN|STOPPED|Running|Frozen|Stopped) ;;
                *)
                    printf '%s\n' "the Incus instance $hr_name is in state $hr_state, which the builder memory admission cannot account for (only RUNNING, FROZEN and STOPPED are supported); the builder memory admission fails closed"
                    exit 1
                    ;;
            esac
            case "$hr_state" in
                RUNNING|FROZEN|Running|Frozen) ;;
                *) continue ;;
            esac
            printf '%s' "$hr_name" | grep -Eq '^sandbox-[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$' || {
                printf '%s\n' "the running Incus instance $hr_name is not a managed sandbox of this site; the builder memory admission cannot account for it"
                exit 1
            }
            if [ "$hr_managed" != true ]; then
                printf '%s\n' "the running Incus instance $hr_name is not marked managed (${hr_managed_key} is not true); the builder memory admission cannot account for it"
                exit 1
            fi
            # The counted budget: the larger of the declared key and the
            # enforced expanded limits.memory (idiom 1), with the protocol
            # default only when neither side is present.
            hr_enforced_mib=
            if [ -n "$hr_limit" ]; then
                hr_enforced_mib=$(incus_size_to_mib "$hr_limit" 2>/dev/null) || {
                    printf '%s\n' "the running managed sandbox $hr_name has an unsupported enforced limits.memory (got '$hr_limit'); the builder memory admission fails closed"
                    exit 1
                }
                if ! hr_is_mib "$hr_enforced_mib" || [ "$hr_enforced_mib" -lt 1 ] || [ "$hr_enforced_mib" -gt 1048576 ]; then
                    printf '%s\n' "the running managed sandbox $hr_name has an unsupported enforced limits.memory (got '$hr_limit', parsed ${hr_enforced_mib}MiB, outside the supported 1..1048576MiB range); the builder memory admission fails closed"
                    exit 1
                fi
            fi
            if [ -z "$hr_budget" ] && [ -z "$hr_enforced_mib" ]; then
                hr_budget=$HR_DEFAULT_SANDBOX_BUDGET_MIB
                printf '%s\n' "host resources: the running managed sandbox $hr_name carries no ${hr_budget_key} and no enforced limits.memory; counting the protocol default budget ${hr_budget}MiB for it, exactly as sandbox starts do" >&2
            else
                if [ -n "$hr_budget" ]; then
                    case "$hr_budget" in
                        0|[1-9]|[1-9][0-9]*) ;;
                        *)
                            printf '%s\n' "the running managed sandbox $hr_name has an unreadable ${hr_budget_key} (got '$hr_budget'); the builder memory admission fails closed"
                            exit 1
                            ;;
                    esac
                    if [ "$hr_budget" -lt 512 ] || [ "$hr_budget" -gt 1048576 ]; then
                        printf '%s\n' "the running managed sandbox $hr_name declares ${hr_budget_key}=${hr_budget}MiB, outside the supported 512..1048576MiB floor-and-safety-bound range; the builder memory admission fails closed"
                        exit 1
                    fi
                else
                    hr_budget=0
                fi
                if [ -n "$hr_enforced_mib" ] && [ "$hr_enforced_mib" -gt "$hr_budget" ]; then
                    hr_budget=$hr_enforced_mib
                fi
            fi
            hr_sum=$(hr_add_mib "$hr_sum" "$hr_budget") || {
                printf '%s\n' "the running sandbox budgets overflow the admission arithmetic"
                exit 1
            }
        done
        printf '%s\n' "$hr_sum"
    }) || hr_status=$?
    if [ "$hr_status" -ne 0 ]; then
        printf '%s\n' "$hr_out"
        return 1
    fi
    printf '%s\n' "$hr_out"
}

# builder_memory_admission PROFILE RESERVE_MIB MARGIN_MIB RESOURCE_PREFIX:
# the admission rule for running the bounded template builder on a host whose
# sandboxes may be RUNNING (they are never stopped, frozen, or reconfigured
# by the build). Two guards, both naming every number:
#
#   capacity:    sum(running sandbox budgets) + builder limits.memory +
#                host_reserve_mib + margin <= MemTotal — the same structural
#                rule the executor's start admission applies, so running
#                sandboxes cannot grow into their configured budgets on top
#                of the builder even when their current usage is small;
#   instantaneous: MemAvailable >= builder limits.memory + host_reserve_mib
#                + margin — the free-memory snapshot at admission time.
#
# Prints one verdict line; returns 1 with the refusal when either guard
# fails.
builder_memory_admission() {
    hr_profile=$1
    hr_reserve_mib=$2
    hr_margin_mib=$3
    hr_resource_prefix=$4
    for hr_number in "$hr_reserve_mib" "$hr_margin_mib"; do
        case "$hr_number" in
            ''|*[!0-9]*)
                printf '%s\n' "builder memory admission needs numeric MiB arguments, got reserve=$hr_reserve_mib margin=$hr_margin_mib"
                return 1
                ;;
        esac
    done
    [ -n "$hr_resource_prefix" ] || {
        printf '%s\n' "builder memory admission needs the site's resource prefix"
        return 1
    }
    hr_builder_mib=$(incus_profile_memory_mib "$hr_profile" 2>&1) || {
        printf '%s\n' "the builder VM memory limit is unreadable: $hr_builder_mib"
        return 1
    }
    hr_is_mib "$hr_builder_mib" || {
        printf '%s\n' "the builder VM memory limit is not a bounded MiB value: $hr_builder_mib"
        return 1
    }
    # stdout is the verdict or the captured refusal; stderr carries the
    # protocol-default note for keyless sandboxes and must reach the operator
    # unmerged, so the substitution captures stdout only.
    hr_running_mib=$(running_sandbox_budgets_mib "$hr_resource_prefix") || {
        printf '%s\n' "$hr_running_mib"
        return 1
    }
    hr_is_mib "$hr_running_mib" || {
        printf '%s\n' "the running sandbox budget sum is not a bounded MiB value: $hr_running_mib"
        return 1
    }
    hr_total_mib=$(host_mem_total_mib 2>&1) || {
        printf '%s\n' "host memory inventory is unavailable: $hr_total_mib"
        return 1
    }
    hr_available_mib=$(host_mem_available_mib 2>&1) || {
        printf '%s\n' "host memory inventory is unavailable: $hr_available_mib"
        return 1
    }
    hr_need_mib=$(hr_add_mib "$hr_builder_mib" "$hr_reserve_mib") || {
        printf '%s\n' "the builder admission need (builder limits.memory + host_reserve_mib + margin) overflows the admission arithmetic"
        return 1
    }
    hr_need_mib=$(hr_add_mib "$hr_need_mib" "$hr_margin_mib") || {
        printf '%s\n' "the builder admission need (builder limits.memory + host_reserve_mib + margin) overflows the admission arithmetic"
        return 1
    }
    hr_capacity_mib=$(hr_add_mib "$hr_running_mib" "$hr_need_mib") || {
        printf '%s\n' "the builder admission capacity (running budgets + builder limits.memory + host_reserve_mib + margin) overflows the admission arithmetic"
        return 1
    }
    if [ "$hr_capacity_mib" -gt "$hr_total_mib" ]; then
        printf 'insufficient host memory for the bounded builder beside running sandboxes: running sandbox budgets=%dMiB + builder limits.memory=%dMiB + host_reserve_mib=%dMiB + margin=%dMiB = %dMiB exceeds MemTotal=%dMiB; stop sandboxes, lower their budgets or the profile limits.memory, or add host memory and retry\n' \
            "$hr_running_mib" "$hr_builder_mib" "$hr_reserve_mib" "$hr_margin_mib" "$hr_capacity_mib" "$hr_total_mib"
        return 1
    fi
    if [ "$hr_available_mib" -ge "$hr_need_mib" ]; then
        printf 'builder memory fits: MemTotal=%dMiB covers running sandbox budgets=%dMiB + builder limits.memory=%dMiB + host_reserve_mib=%dMiB + margin=%dMiB (need %dMiB) and MemAvailable=%dMiB covers the need; other instances are left running untouched\n' \
            "$hr_total_mib" "$hr_running_mib" "$hr_builder_mib" "$hr_reserve_mib" "$hr_margin_mib" "$hr_need_mib" "$hr_available_mib"
        return 0
    fi
    printf 'insufficient host memory for the bounded builder beside running instances: MemAvailable=%dMiB is below the needed %dMiB (builder limits.memory=%dMiB + host_reserve_mib=%dMiB + margin=%dMiB); stop sandboxes, lower the profile limits.memory, or add host memory and retry\n' \
        "$hr_available_mib" "$hr_need_mib" "$hr_builder_mib" "$hr_reserve_mib" "$hr_margin_mib"
    return 1
}

# disk_free_kib DIR: print the free KiB df reports for DIR (POSIX -P output),
# or nothing when df fails.
disk_free_kib() {
    df -Pk "$1" 2>/dev/null | awk 'NR == 2 {print $4}'
}

# incus_pool_loop_file POOL: print the loop backing file of a loop-backed
# storage pool (its `source` config names an existing regular file), or print
# nothing when the pool is missing, unreadable, or not loop-backed.
incus_pool_loop_file() {
    hr_pool=$1
    hr_source=$(incus storage show "$hr_pool" 2>/dev/null |
        sed -n 's/^  source: //p')
    case "$hr_source" in
        /*) ;;
        *) return 1 ;;
    esac
    [ -f "$hr_source" ] && [ ! -L "$hr_source" ] || return 1
    printf '%s\n' "$hr_source"
}

# loop_file_growth_kib FILE: print how many KiB a sparse loop backing file can
# still grow on its filesystem: the configured (apparent) size minus the
# blocks actually allocated so far. A fully allocated pool cannot grow and
# reports 0.
loop_file_growth_kib() {
    hr_loop_file=$1
    [ -f "$hr_loop_file" ] && [ ! -L "$hr_loop_file" ] || {
        echo "host resources: loop file is missing or unsafe: $hr_loop_file" >&2
        return 1
    }
    hr_apparent=$(stat -c %s "$hr_loop_file" 2>/dev/null) || {
        echo "host resources: loop file size is unreadable: $hr_loop_file" >&2
        return 1
    }
    hr_allocated_blocks=$(stat -c %b "$hr_loop_file" 2>/dev/null) || {
        echo "host resources: loop file allocation is unreadable: $hr_loop_file" >&2
        return 1
    }
    awk -v apparent="$hr_apparent" -v blocks="$hr_allocated_blocks" 'BEGIN {
        growth = int(apparent - blocks * 512)
        if (growth < 0) growth = 0
        printf "%d\n", int((growth + 1023) / 1024)
    }'
}

# disk_headroom_verdict POOL POOL_SIZE_GIB MARGIN_GIB DIR: the preflight disk
# headroom rule. A pool that does not exist yet must fit whole (configured
# size plus margin). A pool that already exists — the normal re-run — only
# needs the margin plus the room its loop file can still grow into; a pool
# that exists but is not loop-backed falls back to the full rule. Prints one
# verdict line naming the rule that applied and its numbers; returns 1 with
# the refusal when the free space does not cover it.
disk_headroom_verdict() {
    hr_pool=$1
    hr_pool_size_gib=$2
    hr_margin_gib=$3
    hr_dir=$4
    hr_avail_kib=$(disk_free_kib "$hr_dir")
    case "$hr_avail_kib" in
        ''|*[!0-9]*) hr_avail_kib=-1 ;;
    esac
    hr_pool_present=0
    if command -v incus >/dev/null 2>&1 && incus storage show "$hr_pool" >/dev/null 2>&1; then
        hr_pool_present=1
    fi
    if [ "$hr_pool_present" -eq 1 ]; then
        hr_loop_file=$(incus_pool_loop_file "$hr_pool" 2>/dev/null || true)
        if [ -n "$hr_loop_file" ]; then
            hr_growth_kib=$(loop_file_growth_kib "$hr_loop_file" 2>&1) || {
                printf '%s\n' "$hr_growth_kib"
                return 1
            }
            hr_need_kib=$((hr_margin_gib * 1048576 + hr_growth_kib))
            hr_rule="existing pool $hr_pool (loop file $hr_loop_file): ${hr_margin_gib}GiB margin + ${hr_growth_kib}KiB the pool can still grow into"
        else
            hr_need_kib=$(( (hr_pool_size_gib + hr_margin_gib) * 1048576 ))
            hr_rule="existing pool $hr_pool is not loop-backed, so the full rule applies: pool ${hr_pool_size_gib}GiB + ${hr_margin_gib}GiB margin"
        fi
    else
        hr_need_kib=$(( (hr_pool_size_gib + hr_margin_gib) * 1048576 ))
        hr_rule="pool ${hr_pool_size_gib}GiB + ${hr_margin_gib}GiB margin (pool does not exist yet)"
    fi
    if [ "$hr_avail_kib" -ge "$hr_need_kib" ]; then
        printf 'disk headroom on %s: %sKiB free covers the needed %sKiB — rule: %s\n' \
            "$hr_dir" "$hr_avail_kib" "$hr_need_kib" "$hr_rule"
        return 0
    fi
    if [ "$hr_avail_kib" -eq -1 ]; then
        printf 'disk headroom on %s is unreadable (df output was not numeric) — rule: %s\n' \
            "$hr_dir" "$hr_rule"
        return 1
    fi
    printf 'disk headroom on %s is insufficient: %sKiB free is below the needed %sKiB — rule: %s\n' \
        "$hr_dir" "$hr_avail_kib" "$hr_need_kib" "$hr_rule"
    return 1
}

# prefix_route_verdict BRIDGE_NAME BRIDGE_IPV4 PREFIX: the preflight
# bridge-prefix collision rule. The connected route implied by bridge_ipv4 —
# the /24's network address with its prefix length, e.g. 10.77.0.0/24 for
# bridge_ipv4=10.77.0.1/24 — is accepted only on the site's own bridge, and
# only when bridge_name exists as an Incus-managed bridge carrying the
# descriptor's bridge_ipv4 (the expected shape on a re-run with the site up).
# Any other overlapping destination is refused, even on the site bridge
# device itself (10.77.0.128/25 dev <bridge> points half the subnet somewhere
# else), as is any overlapping route on another interface, the same name on a
# non-Incus interface, or a bridge whose subnet no longer matches the
# descriptor. Check the main IPv4 table, including covering supernets,
# while ignoring the default route.
prefix_route_verdict() {
    hr_bridge=$1
    hr_bridge_ipv4=$2
    hr_prefix=$3
    hr_site_bridge_ok=0
    if command -v incus >/dev/null 2>&1 && incus network show "$hr_bridge" >/dev/null 2>&1; then
        hr_bridge_addr=$(incus network get "$hr_bridge" ipv4.address 2>/dev/null || true)
        if [ "$hr_bridge_addr" = "$hr_bridge_ipv4" ]; then
            hr_site_bridge_ok=1
        fi
    fi
    # The exact connected route the site bridge implies: the /24 network
    # address (host bits zeroed) with its mask. Only this destination on the
    # site bridge is the site's own route.
    hr_site_net="${hr_bridge_ipv4%%/*}"
    hr_site_net="${hr_site_net%.*}.0/24"
    hr_site_bridge_route=0
    hr_foreign_route=
    hr_routes=$(ip -4 route show 2>/dev/null) || {
        printf 'could not read the main IPv4 route table\n'
        return 1
    }
    hr_overlapping_routes=$(printf '%s\n' "$hr_routes" | awk -v site="${hr_site_net%/*}" '
        function ipv4_number(address,    octet) {
            split(address, octet, /[.]/)
            return ((octet[1] * 256 + octet[2]) * 256 + octet[3]) * 256 + octet[4]
        }
        BEGIN { site_address = ipv4_number(site) }
        {
            net = ""
            if ($1 ~ /^[0-9]+\./) net = $1
            else if ($2 ~ /^[0-9]+\./) net = $2
            if (net == "") next
            count = split(net, cidr, "/")
            mask = count == 2 ? cidr[2] + 0 : 32
            if (mask == 0) next
            common = (mask < 24) ? mask : 24
            block = 2 ^ (32 - common)
            if (int(ipv4_number(cidr[1]) / block) != int(site_address / block)) next
            dev = ""
            for (i = 2; i <= NF; i++) if ($i == "dev") { dev = $(i + 1); break }
            print net ":" dev
        }') || {
        printf 'could not compare the main IPv4 route table with the sandbox prefix\n'
        return 1
    }
    for hr_route_pair in $hr_overlapping_routes; do
        hr_route_net=${hr_route_pair%%:*}
        hr_route_dev=${hr_route_pair#*:}
        if [ "$hr_route_dev" = "$hr_bridge" ] && [ "$hr_route_net" = "$hr_site_net" ] && [ "$hr_site_bridge_ok" -eq 1 ]; then
            hr_site_bridge_route=1
        else
            hr_foreign_route="$hr_route_net${hr_route_dev:+ dev $hr_route_dev}"
        fi
    done
    if [ -n "$hr_foreign_route" ]; then
        printf 'an existing host route overlaps the sandbox prefix %s: %s; only the site bridge %s (%s) may hold the connected route %s — choose another prefix or fix the descriptor\n' \
            "$hr_prefix" "$hr_foreign_route" "$hr_bridge" "$hr_bridge_ipv4" "$hr_site_net"
        return 1
    fi
    if [ "$hr_site_bridge_route" -eq 1 ]; then
        printf 'the only host route into the sandbox prefix %s is the existing site bridge %s (%s)\n' \
            "$hr_prefix" "$hr_bridge" "$hr_bridge_ipv4"
        return 0
    fi
    printf 'no host route overlaps the sandbox prefix %s\n' "$hr_prefix"
    return 0
}

# --- pool growth (idiom 3) ----------------------------------------------------
#
# The supported path for growing the storage pool is editing pool_size_gib in
# the site descriptor and rerunning install-chasm.sh --provision. These
# helpers produce the plan (nothing to do / grow / shrink refused) and the
# host-backing verdict for the grow; the installer applies the grow with
# `incus storage set` and proves it by reading the size back.

# pool_configured_size_mib POOL: print the pool's configured size in MiB, or
# fail closed when the pool is unreadable or the value is unparseable. A pool
# with no configured size (an operator-precreated, non-loop pool that chasm
# never sized) returns 4: the caller decides whether that is a plan-level
# "chasm does not manage its size" note or a broken read-back proof.
pool_configured_size_mib() {
    hr_pool=$1
    hr_show=$(incus storage show "$hr_pool" 2>&1) || {
        echo "host resources: storage pool $hr_pool is unreadable: $hr_show" >&2
        return 1
    }
    hr_size=$(printf '%s\n' "$hr_show" | awk '
        /^config:$/ { in_config = 1; next }
        /^[^ ]/ { in_config = 0 }
        in_config && $1 == "size:" { sub(/^[ ]*size:[ ]*/, ""); print; exit }')
    [ -n "$hr_size" ] || {
        echo "host resources: storage pool $hr_pool has no configured size; chasm does not manage its size; pool_size_gib is not applied" >&2
        return 4
    }
    hr_mib=$(incus_size_to_mib "$hr_size" 2>&1) || {
        echo "host resources: storage pool $hr_pool size '$hr_size' is unparseable: $hr_mib" >&2
        return 1
    }
    echo "$hr_mib"
}

# loop_file_allocated_kib FILE: print the KiB actually allocated to FILE
# (blocks times 512, rounded up), or fail when the allocation is unreadable.
loop_file_allocated_kib() {
    hr_file=$1
    hr_blocks=$(stat -c %b "$hr_file" 2>/dev/null) || {
        echo "host resources: loop file allocation is unreadable: $hr_file" >&2
        return 1
    }
    awk -v blocks="$hr_blocks" 'BEGIN { printf "%d\n", int((blocks * 512 + 1023) / 1024) }'
}

# pool_resize_plan POOL SITE_SIZE_GIB: compare the pool's configured size
# with the descriptor's pool_size_gib. Prints one verdict line and returns:
#   0  equal — nothing to do;
#   4  the pool exists but has no configured size (an operator-precreated,
#      non-loop pool): chasm does not manage its size, pool_size_gib is not
#      applied, and re-provision continues — reported, never failed;
#   2  grow approved — the caller checks the host backing, applies the grow,
#      and proves it by read-back;
#   3  shrink — never supported; the caller must stop provision;
#   1  the plan itself failed closed (unreadable or unparseable pool size).
pool_resize_plan() {
    hr_pool=$1
    hr_site_gib=$2
    hr_current_mib=
    hr_plan_status=0
    hr_current_mib=$(pool_configured_size_mib "$hr_pool") || hr_plan_status=$?
    if [ "$hr_plan_status" -eq 4 ]; then
        printf 'storage pool %s has no configured size; chasm does not manage its size; pool_size_gib is not applied\n' "$hr_pool"
        return 0
    fi
    [ "$hr_plan_status" -eq 0 ] || return 1
    hr_is_mib "$hr_current_mib" || {
        echo "host resources: storage pool $hr_pool size is not a safe MiB integer" >&2
        return 1
    }
    hr_site_mib=$((hr_site_gib * 1024))
    if [ "$hr_current_mib" -eq "$hr_site_mib" ]; then
        printf 'storage pool %s is already %sGiB; nothing to do\n' \
            "$hr_pool" "$hr_site_gib"
        return 0
    fi
    if [ "$hr_current_mib" -gt "$hr_site_mib" ]; then
        printf 'shrinking a storage pool is not supported; pool is %sGiB, site asks for %sGiB\n' \
            "$((hr_current_mib / 1024))" "$hr_site_gib"
        return 3
    fi
    printf 'storage pool %s will grow from %sGiB to %sGiB\n' \
        "$hr_pool" "$((hr_current_mib / 1024))" "$hr_site_gib"
    return 2
}

# pool_grow_verdict POOL NEW_SIZE_GIB MARGIN_GIB DIR: the host-backing rule
# for an approved pool grow. The added ceiling — the new size minus the loop
# file's currently allocated bytes — plus the margin must fit in the free
# space of the filesystem holding the loop file, and the verdict names those
# numbers. There is deliberately no non-loop fallback: chasm creates only
# loop-backed pools, so any pool reaching this verdict without a loop file
# was precreated by an operator (or lost its backing); its host backing
# cannot be proven, and applying a larger size to unbacked storage would be
# a guess, so the grow is refused with the pool named (the earlier `||
# return 1` made this branch unreachable — incus_pool_loop_file already
# returns 1 for a non-loop pool — so the refusal is now explicit and
# reachable). Returns 1 with the refusal when the free space does not cover
# the grow, the pool is unreadable, or the pool is not loop-backed.
pool_grow_verdict() {
    hr_pool=$1
    hr_new_gib=$2
    hr_margin_gib=$3
    hr_dir=$4
    hr_loop_file=$(incus_pool_loop_file "$hr_pool") || {
        if incus storage show "$hr_pool" >/dev/null 2>&1; then
            printf 'cannot grow pool %s: it is not loop-backed, so the host backing for the added size cannot be proven; growing a pool chasm did not create is refused\n' "$hr_pool"
        else
            printf 'cannot grow pool %s: the pool is unreadable, so its backing cannot be verified\n' "$hr_pool"
        fi
        return 1
    }
    hr_allocated_kib=$(loop_file_allocated_kib "$hr_loop_file") || return 1
    hr_added_kib=$((hr_new_gib * 1048576 - hr_allocated_kib))
    hr_need_kib=$((hr_added_kib + hr_margin_gib * 1048576))
    hr_dir=$(dirname -- "$hr_loop_file")
    hr_rule="grow pool $hr_pool to ${hr_new_gib}GiB on loop file $hr_loop_file: ${hr_added_kib}KiB added above the ${hr_allocated_kib}KiB already allocated + ${hr_margin_gib}GiB margin"
    hr_avail_kib=$(disk_free_kib "$hr_dir")
    case "$hr_avail_kib" in
        ''|*[!0-9]*) hr_avail_kib=-1 ;;
    esac
    if [ "$hr_avail_kib" -ge "$hr_need_kib" ]; then
        printf 'pool growth backing on %s: %sKiB free covers the needed %sKiB — rule: %s\n' \
            "$hr_dir" "$hr_avail_kib" "$hr_need_kib" "$hr_rule"
        return 0
    fi
    if [ "$hr_avail_kib" -eq -1 ]; then
        printf 'pool growth backing on %s is unreadable (df output was not numeric) — rule: %s\n' \
            "$hr_dir" "$hr_rule"
        return 1
    fi
    printf 'pool growth backing on %s is insufficient: %sKiB free is below the needed %sKiB — rule: %s\n' \
        "$hr_dir" "$hr_avail_kib" "$hr_need_kib" "$hr_rule"
    return 1
}
