#!/bin/sh
umask 022
set -eu

# Repo-local regression harness for host-control/chasm-firewall.
#
# Runs the installed-shape helper against a fake iptables (stateful rule
# files) and a fake nft (table-state file plus a captured ruleset) on PATH,
# with a fake site library supplied through the helper's documented
# CHASM_SITE_HOST_LIB override, so the coexistence accepts can be tested
# without root or nftables:
#
#   1. apply inserts one accept per chain for the pinned bridge, and the
#      rendered ruleset bytes carry the bridge token;
#   2. re-apply is idempotent (no duplicate inserts);
#   3. apply after a bridge_name change sweeps the orphaned accepts for the
#      previous interface before inserting the new ones, loudly;
#   4. remove drains both chains (every duplicate), sweeps leftovers, and
#      deletes the vetting table only after the iptables teardown;
#   5. a failing drain or absence check aborts loudly and keeps the table;
#   6. the sweep matches comments as exact tokens (a foreign rule whose
#      comment merely extends the token survives) and handles the modern
#      "! -i" negation shape and substring-distinct interface names.
#
# Usage: scripts/test-firewall-helper.sh   (repository root; no arguments)

[ "$#" -eq 0 ] || { echo "usage: $0" >&2; exit 2; }

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
HELPER=$ROOT/host-control/chasm-firewall
TEMPLATE=$ROOT/host-config/firewall.nft.template

WORK=$(mktemp -d)
trap 'rm -rf -- "$WORK"' EXIT HUP INT TERM
mkdir -p "$WORK/bin" "$WORK/state"

# --- fake iptables -------------------------------------------------------
# Stateful rule store: one "-A <CHAIN> <spec...>" line per rule in
# $FW_STATE/<CHAIN>. Every invocation is logged with its sequence so the
# remove ordering (iptables teardown before nft table delete) is assertable.
# FW_FAIL_OP + FW_FAIL_MATCH inject a failure (exit 9) into one operation.
cat >"$WORK/bin/iptables" <<'STUB'
#!/bin/sh
log() {
    seq=$(wc -l <"$FW_CALLLOG" 2>/dev/null || echo 0)
    printf '%s %s\n' "$((seq + 1))" "$*" >>"$FW_CALLLOG"
}
log "iptables $*"
op=$1
chain=$2
shift 2
spec="$*"
if [ -n "${FW_FAIL_OP:-}" ] && [ "$op" = "$FW_FAIL_OP" ] && case "$spec" in *"$FW_FAIL_MATCH"*) true ;; *) false ;; esac; then
    exit 9
fi
state=$FW_STATE/$chain
case "$op" in
    -S)
        [ -f "$state" ] && cat "$state"
        exit 0
        ;;
    -C)
        if [ -f "$state" ] && grep -Fqx -- "-A $chain $spec" "$state"; then
            exit 0
        fi
        exit 1
        ;;
    -I)
        printf -- "-A %s %s\n" "$chain" "$spec" >>"$state"
        exit 0
        ;;
    -D)
        if [ -f "$state" ] && grep -Fqx -- "-A $chain $spec" "$state"; then
            grep -Fvx -- "-A $chain $spec" "$state" >"$state.next" || true
            mv "$state.next" "$state"
            exit 0
        fi
        exit 1
        ;;
    *)
        echo "fake iptables: unsupported invocation: $*" >&2
        exit 2
        ;;
esac
STUB

# --- fake nft ------------------------------------------------------------
cat >"$WORK/bin/nft" <<'STUB'
#!/bin/sh
log() {
    seq=$(wc -l <"$FW_CALLLOG" 2>/dev/null || echo 0)
    printf '%s %s\n' "$((seq + 1))" "$*" >>"$FW_CALLLOG"
}
log "nft $*"
if [ "$1" = "-f" ]; then
    [ "${FW_FAIL_NFT_LOAD:-0}" = 0 ] || exit 9
    # Capture the rendered ruleset so tests can pin the exact bytes.
    cp -- "$2" "$FW_LAST_RULESET"
    touch "$FW_TABLE"
    exit 0
fi
if [ "$1 $2" = "list tables" ]; then
    [ ! -f "$FW_TABLE" ] || echo 'table inet chasm'
    exit 0
fi
if [ "$1 $2" = "list ruleset" ]; then
    echo "${FW_RULESET:-}"
    exit 0
fi
if [ "$1 $2" = "list table" ]; then
    [ -f "$FW_TABLE" ] && exit 0
    exit 1
fi
if [ "$1 $2" = "delete table" ]; then
    rm -f "$FW_TABLE"
    exit 0
fi
echo "fake nft: unsupported invocation: $*" >&2
exit 2
STUB

# --- fake site library ---------------------------------------------------
cat >"$WORK/site-host-lib.sh" <<STUB
. '$ROOT/scripts/lib/require-site-host.sh'
SITE_BRIDGE_NAME=\${FW_TEST_BRIDGE:-chasmbr0}
require_site_host() { return 0; }
STUB

chmod +x "$WORK/bin/iptables" "$WORK/bin/nft"

CALLLOG=$WORK/call.log
TABLE=$WORK/table.present
LAST_RULESET=$WORK/last-ruleset.nft
: >"$CALLLOG"

# run_helper BRIDGE [MODE] — run the helper with the named pinned bridge.
run_helper() {
    FW_STATE=$WORK/state FW_CALLLOG=$CALLLOG FW_TABLE=$TABLE \
    FW_LAST_RULESET=$LAST_RULESET FW_TEST_BRIDGE=$1 \
    PATH=$WORK/bin:$PATH \
    CHASM_SITE_HOST_LIB=$WORK/site-host-lib.sh \
    CHASM_FIREWALL_TEMPLATE=$TEMPLATE \
    "$HELPER" "${2:-apply}"
}

# run_helper_failing BRIDGE MODE FAIL_OP FAIL_MATCH — run with an injected
# iptables failure (exit 9) on FAIL_OP invocations whose spec contains
# FAIL_MATCH.
run_helper_failing() {
    FW_STATE=$WORK/state FW_CALLLOG=$CALLLOG FW_TABLE=$TABLE \
    FW_LAST_RULESET=$LAST_RULESET FW_TEST_BRIDGE=$1 \
    FW_FAIL_OP=$3 FW_FAIL_MATCH=$4 \
    PATH=$WORK/bin:$PATH \
    CHASM_SITE_HOST_LIB=$WORK/site-host-lib.sh \
    CHASM_FIREWALL_TEMPLATE=$TEMPLATE \
    "$HELPER" "$2"
}

seed_rule() {
    printf -- "-A %s %s\n" "$1" "$2" >>"$WORK/state/$1"
}

chain_rules() { cat "$WORK/state/$1" 2>/dev/null || true; }

our_accept_count() { chain_rules "$1" | grep -Ec -- '--comment[[:space:]]+chasm_bridge([[:space:]]|$)' || true; }

fail() {
    echo "test-firewall-helper: $*" >&2
    exit 1
}

# 1. apply inserts one accept per chain and renders the expected bytes.
run_helper chasmbr0 apply
[ "$(our_accept_count INPUT)" = "1" ] || fail "apply did not insert exactly one INPUT accept"
[ "$(our_accept_count FORWARD)" = "1" ] || fail "apply did not insert exactly one FORWARD accept"
[ -f "$LAST_RULESET" ] || fail "apply never rendered a ruleset"
grep -Fq 'iifname "chasmbr0"' "$LAST_RULESET" || fail "rendered ruleset lost the bridge name"
grep -Fq 'tcp dport 53 accept' "$LAST_RULESET" || fail "rendered ruleset lost the DNS accept"
for hook in input forward; do
    awk -v hook="$hook" '
        $1 == "chain" { in_chain = ($2 == hook) }
        in_chain && $1 == "type" && $3 == "hook" && $4 == hook { attached = 1 }
        in_chain && $1 == "iifname" {
            if (attached && $0 ~ /iifname "chasmbr0" meta nfproto ipv6 counter drop/) passed = 1
            exit
        }
        END { exit !passed }
    ' "$LAST_RULESET" || fail "missing leading IPv6 drop in the $hook hook"
done
case "$(cat "$LAST_RULESET")" in
    *'@'*) fail "rendered ruleset retains an unreplaced @TOKEN@" ;;
esac

# 2. re-apply is idempotent.
run_helper chasmbr0 apply
[ "$(our_accept_count INPUT)" = "1" ] || fail "re-apply duplicated the INPUT accept"

# 3. a bridge_name change sweeps the orphaned accepts for the old interface.
run_helper fwtestbr1 apply
if chain_rules INPUT | grep -Fq -- '-i chasmbr0'; then
    fail "apply with a new bridge left the old interface's INPUT accept orphaned"
fi
[ "$(our_accept_count INPUT)" = "1" ] || fail "apply with a new bridge did not converge on one INPUT accept"

# 3b. the sweep is loud: a failure while sweeping an orphan must abort apply.
: >"$WORK/state/INPUT"; : >"$WORK/state/FORWARD"
seed_rule INPUT '-i oldbr0 -m comment --comment chasm_bridge -j ACCEPT'
before=$(cksum "$LAST_RULESET")
if run_helper_failing fwtestbr3 apply -D oldbr0 2>/dev/null; then
    fail "apply succeeded despite a failing orphan sweep"
fi

[ "$(cksum "$LAST_RULESET")" = "$before" ] || fail "orphan sweep failure replaced the old vetting table"

# A rejected nft transaction must not grant the new bridge a rescue rule.
if FW_FAIL_NFT_LOAD=1 run_helper fwtestbr3 apply; then
    fail "apply ignored a failed nft transaction"
fi
[ "$(cksum "$LAST_RULESET")" = "$before" ] || fail "failed transaction replaced the ruleset"
[ -z "$(chain_rules INPUT)" ] && [ -z "$(chain_rules FORWARD)" ] ||
    fail "failed nft load left rescue rules for an unvetted bridge"

if run_helper_failing fwtestbr3 apply -C fwtestbr3; then
    fail "apply treated an iptables read error as an absent rule"
fi
[ -z "$(chain_rules INPUT)" ] || fail "apply inserted a rescue rule after a read error"

# 4. remove drains every duplicate, sweeps leftovers, then deletes the table.
seed_rule INPUT '-i fwtestbr1 -m comment --comment chasm_bridge -j ACCEPT'
seed_rule INPUT '-i fwtestbr1 -m comment --comment chasm_bridge -j ACCEPT'
seed_rule INPUT '-i chasmbr0 -m comment --comment chasm_bridge -j ACCEPT'
seed_rule FORWARD '-i fwtestbr1 -m comment --comment chasm_bridge -j ACCEPT'
: >"$CALLLOG"
run_helper fwtestbr1 remove
[ -z "$(chain_rules INPUT)" ] && [ -z "$(chain_rules FORWARD)" ] ||
    fail "remove left rescue rules behind: $(chain_rules INPUT) $(chain_rules FORWARD)"
last_iptables=$(awk '$2 == "iptables" { print $1 }' "$CALLLOG" | tail -1)
nft_delete=$(awk '$2 == "nft" && $3 == "delete" { print $1 }' "$CALLLOG" | tail -1)
[ -n "$nft_delete" ] || fail "remove never deleted the vetting table"
if [ "$last_iptables" -ge "$nft_delete" ]; then
    fail "remove deleted the vetting table before finishing the iptables teardown"
fi
[ ! -f "$TABLE" ] || fail "fake table state survived remove"

# 5. a failing drain aborts loudly and keeps the vetting table.
: >"$CALLLOG"
rm -f "$WORK/state/INPUT" "$WORK/state/FORWARD"
run_helper fwtestbr2 apply
if run_helper_failing fwtestbr2 remove -D fwtestbr2 2>/dev/null; then
    fail "remove succeeded despite a failing drain"
fi
[ -f "$TABLE" ] || fail "a failed drain must not reach the vetting-table delete"
chain_rules INPUT | grep -Fq -- '-i fwtestbr2' ||
    fail "the injected drain failure did not hold the rule in place"

# 5b. a failing absence check (-C error, not rule-absent) is also fatal.
if run_helper_failing fwtestbr2 remove -C fwtestbr2 2>/dev/null; then
    fail "remove succeeded despite a failing absence check"
fi
[ -f "$TABLE" ] || fail "a failed absence check must not reach the vetting-table delete"

# 6. token-exact comment matching: a foreign rule whose comment extends the
# token is NOT swept, a modern negated-interface orphan IS.
: >"$CALLLOG"
rm -f "$WORK/state/INPUT" "$WORK/state/FORWARD"
seed_rule INPUT '-i eth9 -m comment --comment chasm_bridge_ext -j ACCEPT'
seed_rule INPUT '! -i br-old -m comment --comment chasm_bridge -j ACCEPT'
run_helper fwtestbr4 apply
chain_rules INPUT | grep -Fq -- 'chasm_bridge_ext' ||
    fail "the sweep deleted a foreign rule whose comment merely extends the token"
if chain_rules INPUT | grep -Fq -- 'br-old'; then
    fail "the sweep ignored a modern negated-interface orphan"
fi
[ "$(our_accept_count INPUT)" = "1" ] || fail "apply did not converge after negated-orphan sweep"

# 6b. substring-distinct interface names: bridge incusbr must sweep incusbr0.
: >"$WORK/state/INPUT"; : >"$WORK/state/FORWARD"
seed_rule INPUT '-i incusbr0 -m comment --comment chasm_bridge -j ACCEPT'
run_helper incusbr apply
if chain_rules INPUT | grep -Fq -- '-i incusbr0'; then
    fail "a bridge name that is a substring of the orphan interface did not sweep"
fi
chain_rules INPUT | grep -Fq -- '-i incusbr ' || chain_rules INPUT | grep -Fq -- '-i incusbr -' ||
    fail "the exact bridge accept is missing after the substring sweep"

# Use an isolated PATH and a fixture proc file to model nft-only hosts
# independently of the test runner's kernel modules.
mkdir "$WORK/nft-only"
for utility in basename dirname sed cat grep mktemp rm cp touch wc; do
    ln -s "$(command -v "$utility")" "$WORK/nft-only/$utility"
done
ln -s "$WORK/bin/nft" "$WORK/nft-only/nft"
sed "s|/proc/net/ip_tables_names|$WORK/legacy-tables|g" "$HELPER" >"$WORK/helper"
run_nft_only() {
    FW_STATE=$WORK/state FW_CALLLOG=$CALLLOG FW_TABLE=$TABLE FW_LAST_RULESET=$LAST_RULESET \
    PATH=$WORK/nft-only CHASM_SITE_HOST_LIB=$WORK/site-host-lib.sh CHASM_FIREWALL_TEMPLATE=$TEMPLATE \
        /bin/sh "$WORK/helper" "$1"
}
for mode in apply apply remove; do
    run_nft_only "$mode" || fail "nft-only $mode failed"
done
run_helper chasmbr0 apply
for mode in remove apply; do
    if FW_RULESET='iifname "oldbr0" accept comment "chasm_bridge"' run_nft_only "$mode"; then
        fail "$mode permitted unverified nft rescue rules"
    fi
    echo filter >"$WORK/legacy-tables"
    if run_nft_only "$mode"; then
        fail "$mode permitted unverified legacy filter rules"
    fi
    rm "$WORK/legacy-tables"
    [ -f "$TABLE" ] || fail "$mode removed the existing table"
done

echo "test-firewall-helper: all scenarios passed"
