# Audit a disposable Chasm guest with Lynis

Use this diagnostic on a dedicated qualification site provisioned from the candidate you are testing. Lynis inventories the guest OS and its configuration. The score alone cannot establish whether host isolation or network policy works.

As the Chasm operator, create a fresh guest and start it. Pick an unused name and stop if creation fails. The commands below assume the name `lynis-audit`.

```sh
chasm create lynis-audit && chasm start lynis-audit
```

On that same site, run the following from the trusted checkout as an administrator with sudo access. Use the local Incus daemon and the default project used by the release. Verify the source revision against the candidate deployed on this site. The package installation changes only the selected guest. Use its configured Debian repositories.

```sh
sudo incus --force-local --project default exec sandbox-lynis-audit -- apt-get update
sudo incus --force-local --project default exec sandbox-lynis-audit -- apt-get install -y lynis
scripts/diagnostics/audit-sandbox.sh lynis-audit
```

The harness checks site ownership, VM identity, boot ID and expected mounts. It prints the completed report path and keeps evidence in a private directory under the current directory. Record the deployed release revision alongside the harness revision. Review `warning[]`, `suggestion[]`, and `vulnerable_package[]` entries in `report.dat`. Compare runs using the same template, package state, Lynis version, and options. Keep the report and console log private because they can identify services, packages and network configuration. After preserving the evidence, stop the disposable guest with `chasm stop lynis-audit` and remove it with `chasm delete lynis-audit`.
