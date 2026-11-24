---
title: "Chasm compared with Incus, Docker, Firecracker, Vagrant and gVisor"
permalink: /comparison.html
---
# Chasm compared with Incus, Docker, Firecracker, Vagrant and gVisor

Both Chasm and Docker can cap CPU and memory and discard a workspace. Choose Chasm when you want unfamiliar code to run behind a VM boundary: each sandbox boots its own Linux kernel. An ordinary Docker Engine container on Linux shares the kernel of its container host. Chasm uses Incus to provide the VM and adds a sealed starting image, memory admission checks and a private workspace with a defined cleanup policy.

## Which isolation model fits the workload?

| Tool | What runs the workload? | When to choose it | Security decision you still own |
| --- | --- | --- | --- |
| Chasm | A full Incus VM per sandbox, with a guest kernel and a private root disk and workspace. | Run unfamiliar code that needs a full Linux system, with a separate guest kernel for each workload. Chasm provisions the VM, checks available memory before start, and removes its private workspace on delete. | All guests can write to the site's shared volume and reach peers on the stock bridge. Use the site within one operator trust domain. [Architecture](architecture.md) |
| Incus | System containers sharing the host kernel, or VMs with their own kernels. | Manage a mix of long-lived or disposable Linux systems and configure their profiles, networks and storage directly. | Select the instance type, device access, network and privileges for each workload. [Incus instance types](https://linuxcontainers.org/incus/docs/main/explanation/instances/) |
| Docker Engine on Linux | Ordinary Linux containers isolate processes with namespaces and account for resources with cgroups. They share the host kernel. | Package an application as an image and run it with the Docker toolchain. | Review daemon access, capabilities, privileged mode and host mounts. Access to the Docker daemon is a privileged boundary. [Docker Engine security](https://docs.docker.com/engine/security/) |
| Firecracker | A KVM microVM with a guest kernel and a deliberately small virtual device model. | Build a service that launches large numbers of lightweight VMs and supplies its own orchestration. | The service operator supplies guest images, networking, storage and the host controls around the VMM. [Firecracker architecture](https://firecracker-microvm.github.io/) |
| Vagrant | A provider creates the environment. A VM provider and the Docker provider use different isolation mechanisms. | Describe a reproducible development environment and choose a provider for each host. | Review the provider, synced folders, forwarded ports and provisioners in the project configuration. [Vagrant providers](https://developer.hashicorp.com/vagrant/docs/providers) |
| gVisor | An application kernel in userspace implements the Linux interfaces used by a sandboxed container. | Keep a container workflow while reducing the host system-call interface exposed to applications. | Check application compatibility and measure filesystem, networking and system-call-heavy paths for your workload. [gVisor architecture](https://gvisor.dev/docs/architecture_guide/intro/) · [Performance guide](https://gvisor.dev/docs/architecture_guide/performance/) |

## How should I compare performance?

Measure the operation your users wait for. For an interactive sandbox, time from the start request until a command can run in the guest. Record template or image preparation separately, along with guest size, host hardware, cache state and concurrency. For a storage-heavy workload, also measure reads and writes on the actual workspace mount.

### Chasm 0.1.0 lifecycle timings

A single Debian 13 qualification site ran Incus 7.0.2 in a nested KVM fixture on an Intel Xeon E5-2680 v4. The site VM had 4 vCPUs and 8 GiB RAM. One sandbox used 1 vCPU and 2 GiB RAM. The site and sealed template were already provisioned. A monotonic clock measured each complete operator CLI invocation, including the local user switch.

| Operation | Wall time in execution order | What completion means |
| --- | --- | --- |
| Create | 86.6 s (one observation) | Create from the local sealed template and return a stopped sandbox with workspace. |
| Start | 28.9, 26.8, 30.9 s (median 28.9 s) | Start the same stopped guest and return `agent_ready=true`. |
| Stop | 4.1, 61.9, 62.0 s (median 61.9 s) | Return from the stop command. Two of three stops took about a minute; the timing trace does not identify the cause. |
| Delete | 0.53 s (one observation) | Return from deletion of the stopped VM and its workspace. |

The sequence was one create, three start/stop cycles, then one delete. Host and guest caches were left in their natural state. These observations describe one serial workload under nested virtualization. Bare-metal latency, concurrent sandbox density, throughput and tail latency remain unmeasured.

Firecracker reports microVM startup below **125 ms** and VMM memory overhead below **5 MiB** in its reference configuration. The memory figure excludes the workload's guest RAM. Chasm's configured guest memory budget and its time to report a ready guest agent measure different things. The release qualification did not run a common cross-product benchmark. [Firecracker figures](https://firecracker-microvm.github.io/)

## What security testing has Chasm undergone?

Chasm 0.1.0 underwent maintainer-run defensive qualification and AI-assisted code review. The release evidence identifies no independent third-party penetration test. The checks below describe what was actually exercised for release commit `9aec85297794cb0bb8a44d01c694a6a847e17c01`.

| Area | What was checked | Scope of the evidence |
| --- | --- | --- |
| Routed network policy | An isolated Linux network fixture established reachability, applied the release rules, then checked that the private IPv4 target and the IPv6 input and forward targets stopped answering. A permitted IPv4 target still answered. | Covers the tested routes and hooks. Guests on the stock bridge can communicate directly. |
| Privileged lifecycle operations | Review found a workspace ownership check that could be skipped and a forced deletion after a stopped-state check. Both were corrected. Regression tests cover ownership refusal and a VM that restarts before deletion. | Tests use controlled Incus responses for failure cases. A disposable Debian 13 site also exercised normal create, start, stop and delete against real Incus. |
| Interrupted operations | Tests cover firewall cleanup across backend changes and failed template publication. On a disposable Incus 7 site, the qualification fixture moved a sealed VM back to its pre-publication builder name and exercised explicit recovery. | The live test verified recovery of that builder and its sealed snapshot. It also exposed a profile-format assumption that the original mocks missed. |
| Guest hardening | The optional Lynis audit ran inside a disposable guest and returned a hardening index of **63**. The collector checked instance and boot identity before publishing the report. | This is a configuration score for that guest and package state. It does not measure hypervisor escape resistance or host isolation. [Run the guest audit](guest-security-audit.md) |
| Release artifact | All 22 local CI checks and the additional Go race checks passed. Gitleaks scanned the selected archive and reachable release history with zero findings. | These checks cover tested behavior, observed races and recognizable secret patterns. The operator and host administrators remain trusted. |

To inspect what the regression cases assert, start with the [deletion tests](https://github.com/rahulrajaram/chasm/blob/9aec85297794cb0bb8a44d01c694a6a847e17c01/internal/execserver/delete_test.go), [firewall tests](https://github.com/rahulrajaram/chasm/blob/9aec85297794cb0bb8a44d01c694a6a847e17c01/scripts/test-firewall-helper.sh), and [builder recovery tests](https://github.com/rahulrajaram/chasm/blob/9aec85297794cb0bb8a44d01c694a6a847e17c01/scripts/test-builder-address-claim.sh).

See the [security policy](https://github.com/rahulrajaram/chasm/blob/9aec85297794cb0bb8a44d01c694a6a847e17c01/SECURITY.md) for the threat model and vulnerability reporting process.
