---
title: "Upgrading and uninstalling"
permalink: /upgrade-and-uninstall.html
---
# Upgrading and uninstalling

Upgrade Chasm by re-running the installer from a trusted checkout of the
intended release. Removal is manual and affects both the host
installation and any site data you choose to delete.

## Upgrading

1. Update the checkout and re-run provisioning from it:

   ```sh
   cd chasm && git pull
   sudo scripts/install-chasm.sh --provision
   ```

   Provisioning is resumable. Completed steps such as the descriptor write,
   the identity pin, Incus initialization and a current template build are
   detected and skipped. The payload step always runs again.
   The re-run rebuilds the Go binaries, refreshes the provision tree
   under `/usr/local/lib/chasm/provision/`, re-renders the units, and rewrites the
   firewall template. It reloads the active firewall to apply the
   updated rules. Preflight on the live
   site applies the re-run rules: an existing loop pool only needs its
   margin plus the room the pool can still grow into, and the site
   bridge's own route into the sandbox prefix is accepted.

   Current upgrades install the operator CLIs in `/usr/local/bin/` and the
   service executables in `/usr/local/sbin/`. They replace payloads at both
   `/usr/local/lib/chasm/go/` and `/usr/local/libexec/chasm/` with
   compatibility symlinks to those installed files. Verification checks
   both the new systemd executable paths and the compatibility links.
   A persisted Incus socket override remains in `/etc/chasm/incus-socket`
   (root:root 0600).

2. Watch the drift report's template probe. The sealed template carries a
   versioned identity digest over its inputs: the builder and identity scripts,
   the package layer and profile reader, the guest `zshrc`, the base image
   fingerprint, the Incus profile, the descriptor's `guest_uid` and
   `resource_prefix`, and the installed bootstrap profile manifest. When an
   upgrade changes any of these, the old template is reported **stale** even
   though its name prefix still matches, and provisioning offers to rebuild
   it. Other tree changes do not make it stale. An upgrade keeps an installed
   core manifest under `/etc/chasm/profiles/` that differs from the release's,
   so a manifest change in the release alone does not trigger a rebuild (see
   [plugins.md](plugins.md)).

3. Rebuild the template when offered. A rebuild runs beside running
   sandboxes (the builder never stops or touches them): it is admitted
   when `MemTotal` covers the builder profile's `limits.memory` plus
   `host_reserve_mib` plus a margin plus the configured budgets of every
   running sandbox (the same rule sandbox starts use: the per-sandbox
   budget cap is the host's memory minus `host_reserve_mib`, named in
   every refusal), and `MemAvailable` still covers the need — and it
   refuses with the numbers when it does not — `chasm stop` a few
   sandboxes and retry in that case.
   While the builder VM runs, Chasm refuses start operations because it
   cannot account for that VM as a managed sandbox. Existing sandboxes
   keep running on the old template's cloned disks until you delete
   them; only new `chasm create` calls start from
   the rebuilt template.

4. Verify:

   ```sh
   sudo scripts/install-chasm.sh --verify
   ```

   Zero drift is the target state after any upgrade.

A re-run keeps the installed site descriptor at `/etc/chasm/site.conf` instead
of copying the example again; it only rewrites the `site_name` and
`operator_user` lines with their resolved values. When a release changes the
descriptor schema (reported in the
[changelog](https://github.com/rahulrajaram/chasm/blob/main/CHANGELOG.md)), edit the file by hand — both readers fail
closed on unknown keys, so stale keys must be removed for the site to
come up.

## Uninstalling

Back up the data you want to preserve. A sandbox root disk, its custom
workspace volume, and the custom volume shared by the site are distinct
Incus objects. Export each required object separately. Keep any Chasm
state and descriptor you need for recovery.

As the Chasm operator, stop and delete each sandbox you intend to remove.
Replace `NAME` with its logical name. Deletion removes its root disk,
workspace volume and catalog entry.

```sh
chasm stop NAME
chasm delete NAME
```

Afterward, use an administrator account for the remaining teardown.
Confirm ownership against your installed descriptor and Incus inventory.
The following names apply to a dedicated installation with shipped
defaults. Keep shared Incus objects in place if other workloads use them.

```sh
# stop and disable the control plane
systemctl disable --now chasm-executor.socket \
    chasm-executor.service chasm-supervisor.socket \
    chasm-supervisor.service chasm-firewall.service

# inspect the remaining site resources before deleting them
incus --force-local list --project default
incus --force-local storage volume list chasm --project default

# list every sealed template this site owns, including older identities
incus --force-local list --project default --format csv -c n | grep '^chasm-template-'
# for each one confirmed to belong to this site and safe to discard, replace
# TEMPLATE with its exact name, remove its delete protection, and delete it
incus --force-local config set TEMPLATE security.protection.delete=false --project default
incus --force-local delete TEMPLATE --project default

# remove only site-owned objects after confirming their data can be discarded
incus --force-local storage volume delete chasm chasm-shared --project default
incus --force-local profile delete chasm --project default
incus --force-local network delete chasmbr0 --project default
# the imported base image (base_image_alias); keep it if you supplied the alias yourself or other workloads use it
incus --force-local image delete chasm-debian-13 --project default
incus --force-local storage delete chasm --project default

# remove host files
rm -f  /etc/systemd/system/chasm-{executor,supervisor}.service \
       /etc/systemd/system/chasm-{executor,supervisor}.socket \
       /etc/systemd/system/chasm-firewall.service
systemctl daemon-reload
rm -f  /etc/sudoers.d/chasm
rm -f  /usr/lib/sysusers.d/chasm.conf \
       /usr/lib/tmpfiles.d/chasm.conf
rm -f  /usr/local/bin/chasm /usr/local/bin/chasmctl
# optional: a chasm-clipboard helper installed by an earlier release or
# built from plugins/clipboard (it is no longer part of the core payload)
rm -f  /usr/local/bin/chasm-clipboard
rm -f  /usr/local/sbin/chasm-firewall
rm -f  /usr/local/sbin/chasm-executor-server \
       /usr/local/sbin/chasm-supervisor-server
rm -rf /usr/local/lib/chasm
rm -rf /usr/local/libexec/chasm     # interim compatibility links
rm -rf /var/lib/chasm               # state, catalog, audit log, build receipts
rm -rf /etc/chasm                   # descriptor, identity marker, firewall template

# remove the supervisor system user and its group
userdel chasm-supervisor
groupdel chasm-supervisor

# optional: the sshd hardening drop-ins (installed only with --harden-ssh).
# Removing 00-chasm.conf also stops sshd reading ~/.ssh/authorized_keys-chasm;
# move any keys you rely on into authorized_keys first.
rm -f /etc/ssh/sshd_config.d/00-chasm.conf /etc/ssh/sshd_config.d/99-chasm.conf
sshd -t && for unit in ssh.service sshd.service; do
    systemctl is-active --quiet "$unit" && { systemctl reload "$unit"; break; }
done   # with socket activation only, new connections use the new configuration
```

Leaving the nftables `chasm` table behind is harmless once the bridge is
gone; `nft delete table inet chasm` removes it immediately. The modprobe
nested-virtualization drop-in under `/etc/modprobe.d/` is a host
hardening measure, not chasm state; keep or remove it as you prefer.

The `storage_pool`, `profile_name`, `bridge_name`, `resource_prefix`
and `supervisor_user` in your descriptor identify site objects. Check for
old site-owned templates, custom volumes, and images that still reference
the pool before deleting it. An Incus dependency error is a reason to
inspect the remaining inventory before retrying.
