# Drive a sandbox from an agent

Run these commands as the configured Chasm operator on a provisioned
host. Your agent can use the ordinary CLI and the sandbox's unprivileged
guest account.

## Start a sandbox and run a command

```sh
chasm create research
chasm start research
chasm exec research --cwd /workspace -- /usr/bin/id
```

`start` waits for the guest agent to respond. Use `--` to end Chasm
options. Pass the guest command and arguments as separate elements if
your agent starts processes directly.

## Capture streams and status

```sh
printf 'sample input\n' |
  chasm exec research --status-fd 3 -- /usr/bin/cat \
    3>exec-status.json >exec.stdout 2>exec.stderr
```

Use a pipe or file for descriptor 3 when your agent needs a definitive
result. Read the one JSON object from that descriptor. A `completed`
result's `guest_status` is the guest command's exit code. An
`interrupted` result can also carry a confirmed guest status.
Check `output_code` for incomplete stream delivery even if the command
exited successfully.
`outcome_unknown` means Chasm could not confirm completion. Check the
sandbox or the command's intended output before retrying an operation
with side effects. A missing status frame also leaves completion
unconfirmed.

## Stop when you finish

```sh
chasm stop research
```

Files on the sandbox's root disk and `/workspace` survive a stop. Use
`chasm delete research` after stopping only if you want to remove the
sandbox and its workspace. The site's `/shared` volume remains available
to other sandboxes.
