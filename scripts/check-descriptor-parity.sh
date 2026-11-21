#!/bin/sh
umask 022
set -eu

# Descriptor parity gate: the shell reader (scripts/lib/require-site-host.sh)
# and the Go reader (internal/siteconfig) must accept and reject exactly the
# same site descriptors.
#
# Every case under testdata/fixtures/descriptor-cases/ is a complete
# descriptor plus a recorded verdict in the MANIFEST (<name> <expect> <hint>).
# This script runs each case through both readers and fails if either reader
# disagrees with the recorded verdict, with the other reader, or (for rejects)
# with the rule or key the manifest says must be named.
#
# The Go reader is exercised through a throwaway harness generated in a temp
# directory outside the repository: the harness copies the siteconfig package
# source (the file that defines the expected behavior), so the gate never adds
# a permanent Go program and never imports internal/ from outside the module.
#
# Runs unprivileged from the repository root. Nothing here touches /etc or
# any installed path; the shell reader is pointed at the case files with
# CHASM_SITE_CONF. The corpus itself is allowed to contain host-network
# literals (testdata is the reviewed allowlist); this script must not.

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(CDPATH='' cd -- "$SCRIPT_DIR/.." && pwd)
cd "$REPO_ROOT"

CASES_DIR=$REPO_ROOT/testdata/fixtures/descriptor-cases
MANIFEST=$CASES_DIR/MANIFEST

fail() {
    echo "descriptor parity: $*" >&2
    exit 1
}

[ -d "$CASES_DIR" ] || fail "case corpus is missing: $CASES_DIR"
[ -f "$MANIFEST" ] || fail "case manifest is missing: $MANIFEST"
[ -f "$REPO_ROOT/internal/siteconfig/siteconfig.go" ] ||
    fail "the Go reader source is missing: internal/siteconfig/siteconfig.go"
command -v go >/dev/null 2>&1 || fail "go is required to run the Go reader side of the parity gate"

# --- build the throwaway Go reader harness in a temp dir outside the repo ---
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT HUP INT TERM
mkdir "$work/cases"
cp "$CASES_DIR"/*.conf "$work/cases/"
chmod 0644 "$work/cases"/*.conf
mkdir -p "$work/reader"
cp "$REPO_ROOT/internal/siteconfig/siteconfig.go" "$work/reader/siteconfig.go"
go_version=$(sed -n 's/^go[[:space:]]*//p' "$REPO_ROOT/go.mod" | head -n 1)
[ -n "$go_version" ] || go_version=1.23
{
    echo "module descriptorparity"
    echo ""
    echo "go $go_version"
} > "$work/go.mod"
cat > "$work/main.go" <<'EOF'
package main

import (
	"fmt"
	"os"

	siteconfig "descriptorparity/reader"
)

func main() {
	f, err := os.Open(os.Args[1])
	if err != nil {
		fmt.Printf("REJECT: %v\n", err)
		return
	}
	defer f.Close()
	site, err := siteconfig.Parse(f)
	if err != nil {
		fmt.Printf("REJECT: %s\n", err)
		return
	}
	fmt.Printf("ACCEPT root_disk_gib=%d\n", site.ProfileRootDiskGiB)
}
EOF
(
    cd "$work"
    go build -buildvcs=false -o parity-reader . 2>"$work/build.err"
) || { echo "descriptor parity: could not compile the Go reader harness:" >&2; sed 's/^/  /' "$work/build.err" >&2; exit 1; }

# --- verdict helpers ---------------------------------------------------------
# reader verdicts are printed as a single line: "ACCEPT" or "REJECT: <message>".
shell_verdict() {
    case_file=$1
    set +e
    output=$(CHASM_SITE_CONF="$case_file" sh -c \
        '. ./scripts/lib/require-site-host.sh; site_require_descriptor && printf "ACCEPT root_disk_gib=%s\n" "${SITE_PROFILE_ROOT_DISK_GIB:-0}"' 2>&1)
    rc=$?
    set -e
    if [ "$rc" -eq 0 ]; then
        printf '%s\n' "$output"
    else
        echo "REJECT: $output"
    fi
}

go_verdict() {
    case_file=$1
    set +e
    output=$("$work/parity-reader" "$case_file" 2>&1)
    set -e
    printf '%s\n' "$output"
}

verdict_word() {
    case "$1" in
        "ACCEPT root_disk_gib="*) echo accept ;;
        REJECT:*) echo reject ;;
        *) echo unknown ;;
    esac
}

checked=0
failures=0

# File-mode rejection belongs to the shell reader; the Go parser receives
# an already-open file and intentionally has no filesystem-mode contract.
cp "$work/cases/valid-base.conf" "$work/writable.conf"
chmod 0664 "$work/writable.conf"
case "$(shell_verdict "$work/writable.conf")" in
    *'group- or world-writable'*) ;;
    *) fail "the shell reader accepted a 664 descriptor" ;;
esac

# Every manifest entry must name a real case file, and every case file must
# have an entry, so a forgotten case cannot silently vanish from the gate.
manifest_names=$(
    sed -n 's/^[[:space:]]*\([a-z0-9-]*\)[[:space:]]\+\(accept\|reject\)[[:space:]]\+\(.\+\)$/\1/p' "$MANIFEST"
)
if [ -z "$manifest_names" ]; then
    fail "the case manifest has no parseable entries"
fi
for case_name in $manifest_names; do
    [ -f "$CASES_DIR/$case_name.conf" ] ||
        fail "manifest entry $case_name has no case file"
done
for case_file in "$CASES_DIR"/*.conf; do
    case_name=$(basename "$case_file" .conf)
    found=no
    for manifest_name in $manifest_names; do
        [ "$manifest_name" = "$case_name" ] && found=yes
    done
    [ "$found" = yes ] || fail "case file $case_name has no manifest entry"
done

while read -r case_name expect hint; do
    case "$case_name" in
        ''|'#'*) continue ;;
    esac
    case_file=$work/cases/$case_name.conf
    [ -f "$case_file" ] || continue
    checked=$((checked + 1))

    shell_out=$(shell_verdict "$case_file")
    go_out=$(go_verdict "$case_file")
    shell_word=$(verdict_word "$shell_out")
    go_word=$(verdict_word "$go_out")

    problem=
    [ "$shell_word" = "$expect" ] || problem="shell $shell_word, want $expect"
    [ "$go_word" = "$expect" ] || problem="${problem:+$problem; }go $go_word, want $expect"
    if [ "$expect" = "reject" ]; then
        [ "$shell_word" = reject ] && [ -n "$hint" ] && [ "$hint" != "-" ] &&
            case "$shell_out" in *"$hint"*) ;; *) problem="${problem:+$problem; }shell message does not name '$hint'" ;; esac
        [ "$go_word" = reject ] && [ -n "$hint" ] && [ "$hint" != "-" ] &&
            case "$go_out" in *"$hint"*) ;; *) problem="${problem:+$problem; }go message does not name '$hint'" ;; esac
    fi

    if [ -n "$problem" ]; then
        echo "FAIL $case_name: $problem" >&2
        echo "     shell: $shell_out" >&2
        echo "     go:    $go_out" >&2
        failures=$((failures + 1))
    else
        echo "ok   $case_name ($expect)"
    fi
done < "$MANIFEST"

# The Go reader uses zero for an omitted disk size and the shell uses an
# empty value. Both mean the renderer should apply the 60 GiB default.
for disk_size in omitted 1 16 4096; do
    case_file=$work/root-disk-$disk_size.conf
    sed 's/^pool_size_gib=.*/pool_size_gib=4096/' "$CASES_DIR/valid-base.conf" > "$case_file"
    expected=$disk_size
    rendered=$disk_size
    if [ "$disk_size" = omitted ]; then
        expected=0
        rendered=60
    else
        printf 'profile_root_disk_gib=%s\n' "$disk_size" >> "$case_file"
    fi
    shell_out=$(shell_verdict "$case_file")
    go_out=$(go_verdict "$case_file")
    for result in "$shell_out" "$go_out"; do
        [ "$result" = "ACCEPT root_disk_gib=$expected" ] ||
            fail "disk size $disk_size: shell=$shell_out go=$go_out expected=$expected"
    done
    CHASM_SITE_CONF="$case_file" "$SCRIPT_DIR/render-incus-preseed.sh" > "$work/preseed"
    grep -Eq "^        size: ${rendered}GiB$" "$work/preseed" ||
        fail "disk size $disk_size: preseed omitted the expected root size ${rendered}GiB"
    checked=$((checked + 1))
done

# --- synthetic cases ---------------------------------------------------------
# The corpus files all end with a newline, which would hide a reader that
# silently skips a final line without one. Generate the missing-newline
# variants at run time from case files that already exist, so both readers are
# held to the same parse for a descriptor whose last line has no terminator.
synthetic_case() {
    label=$1
    source_case=$2
    expect=$3
    hint=$4
    case_file=$work/synthetic-$label.conf
    tr -d '\n' <"$CASES_DIR/$source_case.conf" >"$case_file"
    printf '\nmystery_key=1' >>"$case_file"

    checked=$((checked + 1))
    shell_out=$(shell_verdict "$case_file")
    go_out=$(go_verdict "$case_file")
    shell_word=$(verdict_word "$shell_out")
    go_word=$(verdict_word "$go_out")
    problem=
    [ "$shell_word" = "$expect" ] || problem="shell $shell_word, want $expect"
    [ "$go_word" = "$expect" ] || problem="${problem:+$problem; }go $go_word, want $expect"
    [ "$shell_word" = "$go_word" ] || problem="${problem:+$problem; }readers disagree"
    if [ "$expect" = "reject" ]; then
        case "$shell_out" in *"$hint"*) ;; *) problem="${problem:+$problem; }shell message does not name '$hint'" ;; esac
        case "$go_out" in *"$hint"*) ;; *) problem="${problem:+$problem; }go message does not name '$hint'" ;; esac
    fi
    if [ -n "$problem" ]; then
        echo "FAIL $label (synthetic): $problem" >&2
        echo "     shell: $shell_out" >&2
        echo "     go:    $go_out" >&2
        failures=$((failures + 1))
    else
        echo "ok   $label (synthetic $expect)"
    fi
}

# An unknown key as the unterminated final line must be refused by both.
synthetic_case unknown-final-line-no-newline valid-base reject mystery_key

echo ""
if [ "$failures" -eq 0 ]; then
    echo "descriptor parity: $checked cases, both readers agree with the recorded verdicts"
    exit 0
fi
echo "descriptor parity: $failures of $checked cases diverged" >&2
exit 1
