# chasm bootstrap profile: minimal (core)
#
# A bootstrap profile seals a reproducible guest template by naming the
# Debian packages to install. The installed form of this manifest is
# /etc/chasm/profiles/minimal.profile; the repository keeps this
# source form so scripts/check-site-profiles.sh can hold the sealed guest
# layer to it.
#
# This is the CORE manifest: it carries only what core itself depends on —
# ca-certificates for secure network access, zsh for the shell contract,
# and systemd-timesyncd for clock recovery after pause/resume. The optional
# developer tool set moved to the developer-tools plugin
# (plugins/developer-tools/developer-tools.profile; see docs/plugins.md).

packages=ca-certificates,systemd-timesyncd,zsh
