# Site configuration

`/etc/chasm/site.conf` configures one Chasm installation. The installer
copies [the example](../host-config/site.conf.example) on first provision
and resolves the site name and operator. Later provisioning runs use the
installed descriptor. Changes to the descriptor alone cannot migrate Incus
objects or change already-rendered systemd and firewall configuration.

Use one `key=value` per line, with no quotes or variable expansion.
Blank lines and comments are accepted. Duplicate and unknown keys are
errors. Keep the installed file root-owned with mode `0644`. Store
credentials outside this file.

| Key | Shipped value | Purpose and change considerations |
| --- | --- | --- |
| `site_name` | Hostname | Site label. Omission and an empty value resolve to the hostname during provisioning. |
| `bridge_name` | `chasmbr0` | Incus bridge name and interface selected by the host firewall. Choose before provisioning. |
| `bridge_ipv4` | `10.77.0.1/24` | Bridge address and CIDR. Pick a subnet with no overlapping destination in the host's main IPv4 routing table. The allocator excludes the gateway. Addresses ending in `.0`, `.10`, `.11` and `.255` cannot be used as the gateway. |
| `sandbox_ipv4_prefix` | `10.77.0.` | First three octets of the bridge subnet, followed by a dot. Must match `bridge_ipv4`. The profile and builder reserve `.10` and `.11`. |
| `storage_pool` | `chasm` | Incus pool backing the template, guest root disks and custom volumes. Choose before provisioning. |
| `pool_size_gib` | `64` | Loop-backed pool ceiling, from 1 to 65536 GiB. Provisioning can grow an existing supported pool after a headroom check. See [pool growth](install.md#growing-the-storage-pool). |
| `profile_name` | `chasm` | Incus profile containing the default NIC and root disk. Choose before provisioning. |
| `profile_root_disk_gib` | `60` | Optional root disk default in GiB for a freshly provisioned profile, from 1 to 4096. It must fit within a new loop-backed pool. Choose this before creating the pool and profile. |
| `base_image_alias` | `chasm-debian-13` | Local alias for the image used when sealing templates. An existing alias takes precedence over an import. |
| `base_image_remote` | `images:debian/13/cloud` | Incus `remote:image` used when the local alias is absent. Editing this field alone leaves an existing local alias in place. |
| `resource_prefix` | `chasm` | Ownership metadata keys, volume names and sealed template names. Omission and an empty value use the default. Changing it on a live site makes its existing objects appear foreign to Chasm. |
| `operator_user` | Provisioning user | Required key. An empty value resolves to the invoking user. Use an existing unprivileged host account distinct from the supervisor. |
| `guest_uid` | `1001` | UID and GID of the sandbox account. Valid range is 1 to 2147483647. Choose before building templates and creating data, since file ownership is numeric. |
| `supervisor_user` | `chasm-supervisor` | Dedicated unprivileged service account and socket group. Choose before installing the control plane. |
| `host_reserve_mib` | `1024` | Host memory reserved by sandbox admission, from 0 to 1048576 MiB. It reduces the budget available to sandboxes. |
| `default_sandbox` | Empty | Optional logical name for commands that accept an omitted sandbox argument. Supply the name without the `sandbox-` prefix. |
| `bootstrap_profile` | `minimal` | Bootstrap manifest used to build the sealed template. Omission and an empty value use the default. See [profiles](plugins.md). |

All fields are required except `site_name`, `resource_prefix`,
`default_sandbox`, `bootstrap_profile` and `profile_root_disk_gib`. The stock bridge is configured
for public IPv4 egress through NAT and has Incus IPv6 addressing disabled.
The firewall drops IPv6 input and forwarding from the sandbox bridge.
Installer verification checks the live IPv4 address against the
descriptor and expects Incus IPv6 addressing to remain disabled. Peers
on that bridge can communicate over IPv4, and local IPv6 switching is
outside the routed firewall policy. Treat bridge, profile,
identity and pool names as deployment choices and plan migrations before
changing them on a populated site.

The stock Incus profile uses two CPUs, 3 GiB memory and a 60 GiB root disk.
Changing `profile_root_disk_gib` after provisioning does not resize the
existing profile or guest disks. Existing sites can use the Incus profile
and individual sandbox resize workflow.
Use the [CLI resource options](cli.md) for an individual sandbox. Changing
a profile default alone leaves explicit instance settings in effect.

## Installation boundaries

Chasm manages the local Incus daemon through the standard
`/var/lib/incus/unix.socket` in the default project. Changing the resource
prefix affects object ownership and naming. Each deployment still needs
its own control services, state and firewall if it is to function as an
independent site.

A fresh pool created by Chasm uses a Btrfs loopback file under
`/var/lib/incus/disks/`. The shipped preseed assigns the builder and guest
NIC a default IPv4 policy through the managed bridge. New sandbox clones
match their guest interface using the MAC reported for the Incus `eth0`
device.
