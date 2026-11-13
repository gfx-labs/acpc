# acpc: its own repository, on go-acp

2026-11-13

The session commands (`new`, `prompt`, `exec`, `attach`, `cancel`, `ls`,
`rm`, and `status`/`stop`/`wait` on a session) moved out of
`github.com/gfx-labs/acphost/cmd/acphost` into this repository, with
their history. acphost keeps its library and the commands for running a
command or host by hand (`run`, `host`, `start`, `status`, `stop`,
`wait` on a dir).

Why: acphost is a library for running an agent in a detached, isolated
host. An ACP client is a different thing with its own dependencies and
release cycle. Here it uses acphost only through its public API.

## Changes from the acphost commands

- The binary is `acpc`. Named sessions are under
  `$XDG_STATE_HOME/acpc/sessions`, not `acphost/sessions`. Sessions made
  by the old commands are not found.
- The hand-written JSON-RPC (request ids, a read loop, message structs)
  is replaced by `github.com/gfx-labs/go-acp`: typed requests on a
  `ClientSideConnection`, and an `acp.Client` that answers permission
  requests. The host's `turn_end` notification is registered as an
  extension notification.
- Output still comes from the journal, as before. go-acp's
  `SessionUpdate` callback only says that the journal grew.
- `prompt --no-wait` calls `Prompt` with a context that never ends and
  returns once the journal has the prompt. go-acp sends `session/cancel`
  when a `Prompt` context ends, which cancelled the turn on the first try
  (caught by `TestSessionDetachedTurn`).
- `status`, `stop` and `wait` default to the `default` session, like the
  other commands.

## Dependencies

- `github.com/gfx-labs/acphost`: the host, journal and isolation.
- `github.com/gfx-labs/go-acp`: ACP types and JSON-RPC connection.
  Its own dependency is `valyala/bytebufferpool`.
- `github.com/urfave/cli/v3`: command parsing, as in acphost
  (`290-cli-sessions.md`).
- `go.yaml.in/yaml/v3`: the `-c` config file, as in acphost.
- `golang.org/x/sys`: flock, pidfd and signals.
- `github.com/stretchr/testify` (tests).

## Checks

The session tests moved with the code and run against a real detached
host and the fake agent in `agent_test.go`. `go test -race -count=20`
passed, and the commands were run by hand against `jcode acp`.
