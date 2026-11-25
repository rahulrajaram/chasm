---
title: "Nesting: Chasm inside a VM, Docker inside Chasm"
permalink: /nesting.html
---
# Nesting: Chasm inside a VM, Docker inside Chasm

Chasm can run inside a virtual machine, and a Chasm sandbox can run
containers. Both were tested on 2026-11-25; the limits of that testing are
listed with each setup.

## Run Chasm inside a QEMU/KVM virtual machine

Every distribution trial ran Chasm inside a QEMU VM: Debian 13,
Ubuntu 26.04, Ubuntu 24.04, Fedora 45 (SELinux enforcing) and Arch Linux.
Each VM completed a fresh install, the sandbox lifecycle, `chasm exec`,
`chasm doctor` and persistence across stop and start.

The VM becomes the Chasm host, so it needs hardware virtualization of its own:

- **On the outer host**, enable nested virtualization in KVM
  (`/sys/module/kvm_intel/parameters/nested` reads `Y`) and pass the CPU's
  virtualization extension to the VM. The trials used
  `-accel kvm -cpu host,+vmx`.
- **Inside the VM**, Chasm's usual rule applies: nested virtualization must
  be **disabled** for the sandboxes Chasm creates. Install the drop-in from
  [Disabling nested virtualization](install.md#disabling-nested-virtualization)
  inside the VM, not on the outer host.
- Give the VM enough memory and disk for the sandboxes. The trials used
  4 vCPUs, 8 GiB of RAM, UEFI firmware (q35 with OVMF), and the
  [compact install](install.md#compact-install) with an 8 GiB pool.

Expect slower lifecycle operations than on bare metal. Under nested KVM,
`chasm start` took 26 to 66 seconds, almost all of it waiting for the guest
agent; the sandbox VM itself booted in about one second.

The trials ran on an Intel host. AMD hosts (`kvm_amd`, `+svm`) and cloud VMs
were not tested; a cloud VM works only if the provider offers nested
virtualization.

### Firecracker

Chasm cannot run inside a Firecracker microVM. Chasm needs KVM inside its
host to start sandboxes, and Firecracker does not provide nested
virtualization to its guests. This was not tested.

## Run Docker inside a Chasm sandbox

A sandbox is a full Linux VM, so Docker runs in it like on any Debian
machine. The containers share the sandbox's kernel, not your host's.

The guest account has no `sudo`, so install Docker as an administrator from
the host. The commands assume a sandbox named `research` and the default
guest account `agent`. Check the account name with
`chasm exec research -- id -un`.

```sh
sudo incus --force-local --project default exec sandbox-research -- apt-get update
sudo incus --force-local --project default exec sandbox-research -- apt-get install -y docker.io
sudo incus --force-local --project default exec sandbox-research -- usermod -aG docker agent
```

`chasm exec` runs commands with the guest account's primary group only, so
wrap Docker commands in `sg docker` to use the new group membership:

```sh
chasm exec research -- sg docker -c 'docker run --rm hello-world'
```

What the trial showed, on a Debian 13 sandbox with Docker 26.1.5 from
Debian's packages:

- Without `sg docker`, the command exits 126 with "permission denied" on the
  Docker socket.
- With `sg docker`, `hello-world` pulls and runs.
- Pulled images survive `chasm stop` and `chasm start`, because they live on
  the sandbox's root disk. Size the root disk for your images: the compact
  install's 4 GiB root disk is small for real workloads.

Keep in mind:

- Membership in the `docker` group is equivalent to root inside the guest.
  The VM is still the isolation boundary between the agent and your host.
- Container traffic leaves through the sandbox, so Chasm's network policy
  applies to it as well.
- Docker was installed by hand inside one sandbox. It is not part of the
  sealed template, so a new sandbox needs the same steps.
