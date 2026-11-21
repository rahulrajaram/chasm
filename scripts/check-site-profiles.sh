#!/bin/sh
umask 022
set -eu

# Site bootstrap-profile gate: the shipped profiles must be acceptable to and
# resolved exactly by the shell reader, the profile corpus must match the
# recorded verdicts, and a manifest whose final line has no trailing newline
# must still be read (so an unterminated final key cannot be silently skipped).
#
# Runs read-only from the repository root, unprivileged. Nothing here touches
# /etc or any guest: the reader is pointed at the repository forms with
# CHASM_PROFILE_ROOT, and the corpus and synthetic cases are checked inline
# against the recorded verdicts.

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(CDPATH='' cd -- "$SCRIPT_DIR/.." && pwd)
cd "$REPO_ROOT"

PROFILES_DIR=$REPO_ROOT/guest-config/profiles
CASES_DIR=$REPO_ROOT/testdata/fixtures/profile-cases
MANIFEST=$CASES_DIR/MANIFEST

fail() {
    echo "site profiles: $*" >&2
    exit 1
}

[ -d "$PROFILES_DIR" ] || fail "shipped profiles directory is missing: $PROFILES_DIR"
[ -d "$CASES_DIR" ] || fail "case corpus is missing: $CASES_DIR"
[ -f "$MANIFEST" ] || fail "case manifest is missing: $MANIFEST"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT HUP INT TERM
mkdir "$work/shipped" "$work/plugins" "$work/cases"
cp "$PROFILES_DIR"/*.profile "$work/shipped/"
cp "$CASES_DIR"/*.profile "$work/cases/"
chmod 0644 "$work/shipped"/*.profile "$work/cases"/*.profile

# --- verdict helper ----------------------------------------------------------
# reader verdicts are printed as a single line: "ACCEPT" or "REJECT: <message>".
profile_verdict() {
    root=$1
    name=$2
    set +e
    output=$(CHASM_PROFILE_ROOT="$root" sh -c \
        'set -eu; . ./scripts/lib/require-site-profile.sh; require_site_profile "$1"' sh "$name" 2>&1)
    rc=$?
    set -e
    if [ "$rc" -eq 0 ]; then
        echo "ACCEPT"
    else
        echo "REJECT: $output"
    fi
}

verdict_word() {
    case "$1" in
        ACCEPT) echo accept ;;
        REJECT:*) echo reject ;;
        *) echo unknown ;;
    esac
}

shipped=0
checked=0
failures=0

# --- Part 1: shipped profiles resolve under the reader ----------------------
for profile_path in "$PROFILES_DIR"/*.profile; do
    [ -f "$profile_path" ] || continue
    shipped=$((shipped + 1))
    name=$(basename "$profile_path" .profile)

    # Resolve the pins in a sourced reader pointed at copies in the work
    # directory; a rejection is reported per profile rather than aborting the gate.
    if ! CHASM_PROFILE_ROOT="$work/shipped" sh -c \
        'set -eu; . ./scripts/lib/require-site-profile.sh; require_site_profile "$1"' \
        sh "$name" 2>"$work/shipped.err"; then
        echo "FAIL $name.profile (shipped): reader rejected it" >&2
        sed 's/^/     /' "$work/shipped.err" >&2
        failures=$((failures + 1))
    else
        echo "ok   $name.profile (shipped)"
    fi
done
[ "$shipped" -ge 1 ] || fail "no shipped profiles found in $PROFILES_DIR"

# --- Part 1b: plugin profiles resolve under the reader -----------------------
# Guest package profiles vended as plugins (plugins/*/README.md,
# docs/plugins.md) are data-only manifests in the same grammar and must
# satisfy the same reader. They are NOT part of the shipped core set: the
# core must build, install, and verify with the whole plugins/ directory
# absent, so this loop simply contributes failures when a present plugin
# profile is invalid.
for plugin_profile in "$REPO_ROOT"/plugins/*/*.profile; do
    [ -f "$plugin_profile" ] || continue
    shipped=$((shipped + 1))
    name=$(basename "$plugin_profile" .profile)
    mkdir -p "$work/plugins/$name"
    cp "$plugin_profile" "$work/plugins/$name/$name.profile"
    chmod 0644 "$work/plugins/$name/$name.profile"
    if ! CHASM_PROFILE_ROOT="$work/plugins/$name" sh -c \
        'set -eu; . ./scripts/lib/require-site-profile.sh; require_site_profile "$1"' \
        sh "$name" 2>"$work/plugin.err"; then
        echo "FAIL $plugin_profile (plugin): reader rejected it" >&2
        sed 's/^/     /' "$work/plugin.err" >&2
        failures=$((failures + 1))
    else
        echo "ok   $name.profile (plugin)"
    fi
done

# --- Part 2: corpus verdicts ------------------------------------------------
# Every manifest entry must name a real case file, and every case file must
# have an entry, so a forgotten case cannot silently vanish from the gate.
manifest_names=$(
    sed -n 's/^[[:space:]]*\([a-z0-9-]*\)[[:space:]]\+\(accept\|reject\)[[:space:]]\+\(.\+\)$/\1/p' "$MANIFEST"
)
if [ -z "$manifest_names" ]; then
    fail "the case manifest has no parseable entries"
fi
for case_name in $manifest_names; do
    [ -f "$CASES_DIR/$case_name.profile" ] ||
        fail "manifest entry $case_name has no case file"
done
for case_file in "$CASES_DIR"/*.profile; do
    case_name=$(basename "$case_file" .profile)
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
    case_file=$CASES_DIR/$case_name.profile
    [ -f "$case_file" ] || continue
    checked=$((checked + 1))

    out=$(profile_verdict "$work/cases" "$case_name")
    word=$(verdict_word "$out")

    problem=
    [ "$word" = "$expect" ] || problem="reader $word, want $expect"
    if [ "$expect" = "reject" ]; then
        [ "$word" = reject ] && [ -n "$hint" ] && [ "$hint" != "-" ] &&
            case "$out" in *"$hint"*) ;; *) problem="${problem:+$problem; }message does not name '$hint'" ;; esac
    fi

    if [ -n "$problem" ]; then
        echo "FAIL $case_name: $problem" >&2
        echo "     reader: $out" >&2
        failures=$((failures + 1))
    else
        echo "ok   $case_name ($expect)"
    fi
done < "$MANIFEST"

# --- Part 3: synthetic no-trailing-newline ----------------------------------
# The corpus files all end with a newline, which would hide a reader that
# silently skips a final line without one. Generate the variant at run time
# from a valid case and append an unknown key with no terminator: the reader
# must still read and refuse it.
synthetic_case() {
    label=$1
    source_case=$2
    expect=$3
    hint=$4
    case_file=$work/synthetic-$label.profile
    tr -d '\n' <"$work/cases/$source_case.profile" >"$case_file"
    printf '\nmystery_key=1' >>"$case_file"

    checked=$((checked + 1))
    out=$(profile_verdict "$work" "synthetic-$label")
    word=$(verdict_word "$out")
    problem=
    [ "$word" = "$expect" ] || problem="reader $word, want $expect"
    if [ "$expect" = "reject" ]; then
        [ -n "$hint" ] && [ "$hint" != "-" ] &&
            case "$out" in *"$hint"*) ;; *) problem="${problem:+$problem; }message does not name '$hint'" ;; esac
    fi
    if [ -n "$problem" ]; then
        echo "FAIL $label (synthetic): $problem" >&2
        echo "     reader: $out" >&2
        failures=$((failures + 1))
    else
        echo "ok   $label (synthetic $expect)"
    fi
}

# An unknown key as the unterminated final line must be refused.
# (keep the label within the 32-char profile-name grammar so the generated
# case itself parses far enough to reach the unterminated final key)
synthetic_case synt-unknown valid-minimal-shaped reject mystery_key

# --- Part 4: namespace invariant -------------------------------------------
# The builder and launcher assign PROFILE_NAME to the Incus profile (a site
# key) before they source the profile reader, and the identity helper runs
# `incus profile show "$PROFILE_NAME"` after resolving a bootstrap profile.
# The reader must therefore never set PROFILE_NAME, or a resolved bootstrap
# profile would silently clobber the Incus profile out from under every
# later use (builder resume checks, receipts, `incus init --profile`). A stub
# incus cannot catch this class of bug: any profile name answers.
set +e
namespace_out=$(CHASM_PROFILE_ROOT="$work/shipped" sh -c '
    set -eu
    PROFILE_NAME=incus-profile-from-caller
    . ./scripts/lib/require-site-profile.sh
    require_site_profile minimal
    test -z "${PROFILE_NAME:-}" && exit 1
    test "$PROFILE_NAME" = "incus-profile-from-caller"
' 2>&1)
namespace_rc=$?
set -e
if [ "$namespace_rc" -eq 0 ]; then
    echo "ok   synt-namespace (reader preserves PROFILE_NAME)"
    checked=$((checked + 1))
else
    echo "FAIL synt-namespace: the reader sets PROFILE_NAME; it must not (it collides with the Incus profile in the builder and launcher)" >&2
    echo "     $namespace_out" >&2
    failures=$((failures + 1))
fi

echo ""
if [ "$failures" -eq 0 ]; then
    echo "site profiles: $shipped shipped profiles and $checked corpus/synthetic cases verified"
    exit 0
fi
echo "site profiles: $failures of $checked corpus/synthetic cases (and $shipped shipped profiles) failed" >&2
exit 1
