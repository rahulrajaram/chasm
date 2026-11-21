#!/bin/sh

# Shared IPv4 claim inventory for the site's sandbox bridge network.
#
# Source this file (it defines functions only and never runs anything on its
# own) to get the read-only address-claim check used by
# scripts/launch-sandbox-vm.sh (a sandbox's eth0 address before boot) and
# scripts/build-sandbox-template.sh (the fixed builder address before any
# builder instance is created or resumed). Nothing here mutates Incus, the
# firewall, or any instance: only instance inventory and config are read.
#
# An address is claimed by another instance through any of three sources:
#   - its eth0 ipv4.address device override (in any state: a stopped
#     instance reclaims the address on its next boot), read exactly as the
#     launcher has always read it;
#   - the site's user.<prefix>.instance-ipv4 metadata (written by the
#     launcher after guest initialization; instances created by older
#     builds keep whatever they recorded);
#   - a live IPv4 of a RUNNING or FROZEN instance reported by incus list.
#
# The live-address source is parsed as real CSV. Incus renders the IPv4
# column of an instance with several addresses (a guest Docker bridge beside
# the site interface, for example) as newline-joined "addr (ifname)" entries
# inside ONE quoted field, so a record spans physical lines and a line-based
# reader silently drops every address after the first. The awk reader below
# handles quoted multi-line fields; jq is deliberately not required on the
# host, so the inventory stays `incus list --format csv -c ns4` and no JSON
# parser is involved.

# ipv4_claim_refusal RESOURCE_PREFIX CANDIDATE SELF: print a refusal message
# naming the claiming instance (and which of the three sources claims the
# address) when any instance other than SELF claims CANDIDATE, or when the
# address inventory cannot be read or parsed, and return 1; print nothing and
# return 0 when CANDIDATE is free. An empty SELF skips nothing. Callers fail
# closed on every nonzero return.
ipv4_claim_refusal() {
    ic_prefix=$1
    ic_candidate=$2
    ic_self=${3:-}
    [ -n "$ic_candidate" ] || {
        echo "eth0 IPv4 claim is missing"
        return 1
    }
    ic_inventory=$(incus list --format csv -c ns4 2>/dev/null) || {
        echo "IPv4 address inventory is unavailable"
        return 1
    }
    ic_metadata_key=$(incus_metadata_key "$ic_prefix" instance-ipv4)
    # Parse the NAME,STATUS,IPv4-addresses CSV into one record per instance.
    # The reader accumulates physical lines until the quote count is even, so
    # a quoted field carrying embedded newlines stays one record, then emits
    # NAME<TAB>STATE<TAB>addr1 addr2 ... with the " (ifname)" annotations
    # dropped; the comparison against the candidate stays exact. An
    # unterminated quoted field makes awk exit nonzero and the check fails
    # closed: a malformed inventory is never treated as a free address.
    ic_records=$(printf '%s\n' "$ic_inventory" | awk '
        function csv_fields(record, fields,    position, length_of, character, value, quoted, count) {
            split("", fields)
            value = ""
            quoted = 0
            count = 0
            length_of = length(record)
            for (position = 1; position <= length_of; position++) {
                character = substr(record, position, 1)
                if (quoted) {
                    if (character == "\"") {
                        if (substr(record, position + 1, 1) == "\"") {
                            value = value "\""
                            position++
                        } else {
                            quoted = 0
                        }
                    } else {
                        value = value character
                    }
                } else if (character == "\"") {
                    quoted = 1
                } else if (character == ",") {
                    fields[++count] = value
                    value = ""
                } else {
                    value = value character
                }
            }
            fields[++count] = value
            return count
        }
        function emit_record(record,    count, field, name, state, live, addresses, position, line) {
            count = csv_fields(record, field)
            name = field[1]
            state = toupper(field[2])
            live = (count >= 3) ? field[3] : ""
            gsub(/\([^)]*\)/, " ", live)
            addresses = split(live, field, /[ \t\n\r,;]+/)
            line = name "\t" state
            for (position = 1; position <= addresses; position++) {
                if (field[position] != "") {
                    line = line "\t" field[position]
                }
            }
            print line
        }
        {
            count = gsub(/"/, "&", $0)
            if (record == "") {
                record = $0
            } else {
                record = record "\n" $0
            }
            balance = (balance + count) % 2
            if (balance == 1) {
                next
            }
            emit_record(record)
            record = ""
        }
        END {
            if (balance != 0 || record != "") {
                print "unterminated quoted field in the instance inventory" > "/dev/stderr"
                exit 2
            }
        }
    ') || {
        echo "IPv4 address inventory is unparseable"
        return 1
    }
    # One record per instance: NAME, STATE, then one live address per tab
    # field. Live addresses only exist for RUNNING/FROZEN instances; a
    # stopped instance may still claim through its override or metadata,
    # which are read for every state below.
    ic_tab=$(printf '\t')
    while IFS="$ic_tab" read -r ic_name ic_state ic_live; do
        [ -n "$ic_name" ] || continue
        [ "$ic_name" = "$ic_self" ] && continue
        ic_override=$(incus config device get "$ic_name" eth0 ipv4.address 2>/dev/null || true)
        if [ "$ic_override" = "$ic_candidate" ]; then
            echo "IPv4 address $ic_candidate is already claimed by $ic_name (eth0 ipv4.address override); renumber or free the address"
            return 1
        fi
        ic_metadata=$(incus config get "$ic_name" "$ic_metadata_key" 2>/dev/null || true)
        if [ "$ic_metadata" = "$ic_candidate" ]; then
            echo "IPv4 address $ic_candidate is already claimed by $ic_name ($ic_metadata_key metadata); renumber or free the address"
            return 1
        fi
        case "$ic_state" in
            RUNNING|FROZEN)
                # shellcheck disable=SC2086  # the addresses are tab-joined tokens
                for ic_address in $ic_live; do
                    if [ "$ic_address" = "$ic_candidate" ]; then
                        echo "IPv4 address $ic_candidate is already claimed by $ic_name (live address of a $ic_state instance); renumber or free the address"
                        return 1
                    fi
                done
                ;;
        esac
    done <<EOF
$ic_records
EOF
    return 0
}
