# developer-tools plugin (v1)

Optional guest developer tool set as a data-only chasm bootstrap profile.
See `docs/plugins.md` for the plugin contract; core never requires this
directory.

## Interface

- Shape: guest package profile (v1), in the validated manifest grammar of
  `scripts/lib/require-site-profile.sh`. Data only: the manifest is parsed
  line by line, never sourced or evaluated, and its bytes are digest-bound
  into the sealed template identity.
- Content: `packages=curl,git,jq,python3,python3-pip,python3-venv,sqlite3,tmux,universal-ctags,vim`
  — the optional developer tool set removed from the core `minimal`
  manifest. Core's `minimal` keeps `ca-certificates,zsh,systemd-timesyncd`
  (certificates, shell contract, clock recovery).

## Prerequisites and effects

- Prerequisite: a provisioned chasm site and a working Debian apt mirror
  inside the builder VM. Installing a profile seals it into a NEW template;
  existing sandboxes are unchanged.
- Effects: the named packages are installed inside the guest template by
  the ordinary guest package layer, with post-install verification of every
  package. No host system is touched.

## Selection / install

- Install: copy this manifest into the production profile root, e.g.
  `install -o root -g root -m 0644 developer-tools.profile /etc/chasm/profiles/`
  (create the directory first on a fresh host), then either set
  `bootstrap_profile=developer-tools` in `/etc/chasm/site.conf` or pass the
  profile name to the template builder. For development and tests set
  `CHASM_PROFILE_ROOT` to a directory containing the manifest. To combine
  with the core set, install both templates and select per sandbox.
- The reader applies the same fail-closed rules to this manifest as to any
  core profile: safe name, regular non-symlink file, not group- or
  world-writable, owned by root or the invoking user, plain tokens only.

## Check / remove

- Check: `scripts/lib/require-site-profile.sh --verify developer-tools`
  (with the profile root resolved as above) re-validates the installed
  manifest.
- Remove: delete the manifest from the profile root and any sealed template
  built from it (`chasm` refuses nothing else). Core upgrades never prune
  or rewrite plugin manifests.

## Failure behavior

Any validation failure (unsafe name, writable or symlinked manifest,
unknown key, malformed or duplicate package name) fails closed before any
guest mutation; the guest package layer fails closed if any listed package
cannot be verified post-install. A missing zsh in a selected profile is
refused by the guest layer (core's shell contract).
