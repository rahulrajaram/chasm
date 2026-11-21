#!/bin/sh
# incus-install.sh: the Incus install step's release logic.
#
# The host must be Debian itself (ID=debian in /etc/os-release); Debian
# derivatives — including every ID_LIKE=debian system — are refused before
# any approval, because chasm 0.1.0 supports Debian 12 and 13 only and a
# derivative's VERSION_ID must never be read as Debian's. On Debian:
#
#   trixie (13) and newer
#       apt-get install incus virtiofsd
#       (the release's own repositories)
#   bookworm (12)
#       apt-get install -t bookworm-backports incus virtiofsd
#       (refused up front when no enabled bookworm-backports apt source is
#       configured; the refusal message explains how to add one)
#   any other Debian release
#       refused with a clear message (install incus and virtiofsd yourself)
#
# The caller provides approve, fail, and INCUS_INSTALL_NEEDED (the flag
# preflight sets when incus is absent); apt-get is expected on PATH.
#
# OS_RELEASE_FILE and APT_SOURCES_ROOT are harness overrides used only by
# the repo-local tests (scripts/test-incus-install.sh); production always
# reads /etc/os-release and searches /etc/apt.

: "${OS_RELEASE_FILE:=/etc/os-release}"
: "${APT_SOURCES_ROOT:=/etc/apt}"

# os_release_value KEY: print the value of KEY= from the os-release file
# with any shell quoting stripped, or nothing when the file or key is
# absent.
os_release_value() {
    [ -r "$OS_RELEASE_FILE" ] || return 0
    _orv_value=$(sed -n "s/^$1=//p" "$OS_RELEASE_FILE" | head -n 1)
    case "$_orv_value" in
        \"*\") _orv_value=${_orv_value#\"}; _orv_value=${_orv_value%\"} ;;
        \'*\') _orv_value=${_orv_value#\'}; _orv_value=${_orv_value%\'} ;;
    esac
    printf '%s\n' "$_orv_value"
}

# incus_release_class: print the install class for this host: "native" for
# Debian 13 (trixie) and newer, "bookworm-backports" for Debian 12
# (bookworm), "non-debian" when the host is not Debian itself (ID=debian;
# ID_LIKE=debian derivatives do not qualify — their VERSION_ID is not
# Debian's and 0.1.0 supports Debian 12 and 13 only), or "unsupported" for
# any other Debian release. The release number decides when present (it
# stays right for future releases whose codenames this script does not
# know); the codename is the fallback.
incus_release_class() {
    case "$(os_release_value ID)" in
        debian) ;;
        *) echo non-debian; return 0 ;;
    esac
    _irc_version=$(os_release_value VERSION_ID)
    case "$_irc_version" in
        '' | *[!0-9]*)
            case "$(os_release_value VERSION_CODENAME)" in
                trixie) echo native ;;
                bookworm) echo bookworm-backports ;;
                *) echo unsupported ;;
            esac
            return 0
            ;;
    esac
    if [ "$_irc_version" -ge 13 ]; then
        echo native
    elif [ "$_irc_version" -eq 12 ]; then
        echo bookworm-backports
    else
        echo unsupported
    fi
}

# incus_backports_enabled_list_entries: succeed when a .list file under the
# apt root carries an enabled (uncommented) `deb` line for the
# bookworm-backports suite. Commented-out lines do not count, and neither do
# deb-src-only entries: apt would have no enabled binary source for the
# suite, so the pinned install would fail after the operator approved it.
incus_backports_enabled_list_entries() {
    [ -d "$APT_SOURCES_ROOT" ] || return 1
    [ -n "$(
        find "$APT_SOURCES_ROOT" -type f -name '*.list' 2>/dev/null | {
            while IFS= read -r _ibs_list; do
                if awk '
                    /^[ \t]*deb[ \t]/ && /(^|[ \t])bookworm-backports([ \t]|$)/ { found = 1; exit }
                    END { exit found ? 0 : 1 }
                ' "$_ibs_list" 2>/dev/null; then
                    printf 'yes\n'
                    break
                fi
            done
        }
    )" ]
}

# incus_backports_enabled_deb822_stanzas: succeed when a Deb822 .sources
# file under the apt root carries a stanza whose Suites name
# bookworm-backports, that provides binary packages (Types includes deb;
# an absent Types defaults to deb deb-src, so it counts), and that is not
# disabled (Enabled: no/false/0). A disabled stanza counts for nothing even
# though its text still names the suite; comment lines inside a stanza are
# ignored like apt ignores them.
incus_backports_enabled_deb822_stanzas() {
    [ -d "$APT_SOURCES_ROOT" ] || return 1
    [ -n "$(
        find "$APT_SOURCES_ROOT" -type f -name '*.sources' 2>/dev/null | {
            while IFS= read -r _ibs_sources; do
                if awk '
                    function reset() { backports = 0; enabled = 1; deb = 1; field = "" }
                    BEGIN { reset() }
                    function configured() { return backports && enabled && deb }
                    {
                        line = $0
                        sub(/\r$/, "", line)
                        if (line ~ /^[ \t]*#/) next
                        if (line ~ /^[ \t]*$/) {
                            if (configured()) { found = 1; exit }
                            reset()
                            next
                        }
                        if (line ~ /^[ \t]/) {
                            if (field == "Suites" && line ~ /(^|[ \t])bookworm-backports([ \t]|$)/) backports = 1
                            else if (field == "Types") {
                                if (line ~ /(^|[ \t])deb([ \t]|$)/) deb = 1
                                else deb = 0
                            }
                            next
                        }
                        colon = index(line, ":")
                        if (colon == 0) { field = ""; next }
                        field = substr(line, 1, colon - 1)
                        value = tolower(substr(line, colon + 1))
                        if (field == "Suites" && value ~ /(^|[ \t])bookworm-backports([ \t]|$)/) backports = 1
                        else if (field == "Types") {
                            if (value ~ /(^|[ \t])deb([ \t]|$)/) deb = 1
                            else deb = 0
                        }
                        else if (field == "Enabled") {
                            if (value ~ /^[ \t]*(no|false|0)([ \t]|$)/) enabled = 0
                            else enabled = 1
                        }
                    }
                    END { exit (found || configured()) ? 0 : 1 }
                ' "$_ibs_sources" 2>/dev/null; then
                    printf 'yes\n'
                    break
                fi
            done
        }
    )" ]
}

# incus_backports_source_configured: succeed when a bookworm-backports apt
# source apt would actually use exists. Only enabled entries count: an
# active `deb` line in a .list file, or a live Deb822 stanza in a .sources
# file (see the two detectors). When apt-cache is available, its policy
# output is preferred as the live confirmation — it lists the suites apt has
# actually loaded, which is what the pinned install reads — and the file
# scan decides otherwise (a just-added source the not-yet-run apt-get update
# cannot confirm still counts).
incus_backports_source_configured() {
    if command -v apt-cache >/dev/null 2>&1 &&
        apt-cache policy 2>/dev/null | grep -qs bookworm-backports; then
        return 0
    fi
    incus_backports_enabled_list_entries && return 0
    incus_backports_enabled_deb822_stanzas && return 0
    return 1
}

# incus_backports_hint: the how-to-add-it sentence shared by the preflight
# note and the install-step refusal.
incus_backports_hint() {
    echo "add one (for example: echo 'deb http://deb.debian.org/debian bookworm-backports main' > /etc/apt/sources.list.d/bookworm-backports.list), then re-run --provision, or install incus and virtiofsd yourself"
}

# install_incus: the approved provision step. The release class decides the
# apt command, and every refusal happens before the approval prompt so the
# operator is never asked to approve a step that cannot run.
install_incus() {
    [ "${INCUS_INSTALL_NEEDED:-0}" -eq 1 ] || return 0
    _ii_class=$(incus_release_class)
    case "$_ii_class" in
        non-debian)
            fail "chasm 0.1.0 supports Debian 12 (bookworm) and 13 (trixie) only; this host is not Debian (ID=$(os_release_value ID) ID_LIKE=$(os_release_value ID_LIKE)), so its release cannot choose an Incus source; install incus and virtiofsd yourself and re-run --provision"
            ;;
        native)
            approve "install Incus from this Debian release's own repositories (apt-get update && apt-get install incus virtiofsd)"
            apt-get update
            DEBIAN_FRONTEND=noninteractive apt-get install -y incus virtiofsd
            ;;
        bookworm-backports)
            incus_backports_source_configured ||
                fail "installing Incus on Debian 12 (bookworm) needs the bookworm-backports apt source, and none is configured under $APT_SOURCES_ROOT; $(incus_backports_hint)"
            approve "install Incus from Debian bookworm-backports (apt-get update && apt-get install -t bookworm-backports incus virtiofsd)"
            apt-get update
            DEBIAN_FRONTEND=noninteractive apt-get install -y -t bookworm-backports incus virtiofsd
            ;;
        *)
            fail "no supported Incus source for this Debian release (VERSION_ID=$(os_release_value VERSION_ID) VERSION_CODENAME=$(os_release_value VERSION_CODENAME); Debian 13/trixie and newer install from the release's own repositories, Debian 12/bookworm from bookworm-backports); install incus and virtiofsd yourself and re-run --provision"
            ;;
    esac
}
