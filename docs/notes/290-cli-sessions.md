# 290: CLI sessions and urfave/cli

2026-11-11

The CLI can hold a conversation with an ACP agent from a shell, the way
acpx does, with the agent under acphost's isolation.

```
acphost new    [-s NAME] [-c FILE] [flags] [--lease 8h] -- CMD ARGS...
acphost prompt [-s NAME] [-c FILE] [flags] [--no-wait] TEXT...
acphost exec   [-c FILE] [flags] TEXT... -- CMD ARGS...
acphost attach [-s NAME]
acphost cancel [-s NAME]
acphost ls     [--all] [--format text|json]
acphost rm     -s NAME
acphost status|stop|wait -s NAME
```

There are no agent names: the agent is the command after `--`
(`-- jcode acp`, `-- opencode acp`). `-s` defaults to `default`. Every
command that takes `-s` also takes `--dir DIR` for a session directory
kept somewhere else.

## What a session is

A session is a host's session directory (`spec.json`, the socket, the
journal) plus two files of the CLI:

- `client.json`: the agent's ACP session id, the pid of the host whose
  agent has it open, and the seq of the last journal frame shown.
- `client.lock`: flocked by the one command using the session, holding its
  pid. The host replaces a connection when a second one arrives, so two
  clients on one session would cut each other off. A second `prompt` or
  `attach` is refused with the holder's pid.

Named sessions are in `$XDG_STATE_HOME/acphost/sessions/<key>/<name>`
(`~/.local/state` without the variable). `<key>` is 12 hex digits of the
SHA-256 of the working directory, so a name is a different session in each
directory. It is a hash and not the path because the socket below it has
to fit in 107 bytes. Names are up to 32 letters, digits, `.`, `_` or `-`.

## Commands

- `new` writes the spec, starts the host detached (`StartDetached`), sends
  `initialize` and `session/new`, and prints the session directory. If any
  of that fails the session is removed again.
- `prompt` starts the host if it is not running, sends one
  `session/prompt` and shows the turn. The text is the arguments joined by
  spaces, or stdin with no arguments or `-`. `--no-wait` returns once the
  journal has the prompt.
- `exec` is `new`, one `prompt` and `rm` on a temporary directory. With
  `--format text` it writes each handshake step (spawn, `initialize`,
  `session/new`) as it happens, as acpx does (2026-11-13).
- `attach` shows what the journal gained since this client last looked,
  and if a turn is running waits for its end. Ctrl-C ends `attach`, not
  the turn.
- `cancel` stops the running turn. If a `prompt` in another terminal is
  waiting on it, `cancel` sends that process SIGINT, the same as Ctrl-C
  there: it sends `session/cancel` and waits for the agent to end the
  turn. Otherwise `cancel` connects and sends `session/cancel` itself.
- `ls` lists the working directory's sessions, `--all` every directory's.
- `rm` stops the host and deletes the directory. It takes a name only.

Output comes from the journal, not the socket. A journal frame has a seq,
so the client knows what it has shown, and a turn nobody watched is shown
by the next `attach` or `prompt`. A message on the socket only tells the
client that the journal grew. `--format` is `text` (the library's
`Renderer`), `json` (journal frames, one per line) or `quiet` (the agent's
message text only).

## Host restarts

The host stops when no client was connected for the lease (`--lease`, 8h
for `new`), on `stop`, or with the machine. The next `prompt` starts it
again. The new agent process does not have the conversation open, so the
client sends `session/load`; what the agent replays while loading is not
shown. If the agent has no `loadSession` capability or the load fails, the
client opens a new ACP session and puts `Summarize(journal)` and
`SummaryEnd` in front of the prompt, as jobshop does.

`-c` and the isolation flags on a later `prompt` replace the session's
isolation. If it differs from the spec, an idle host is stopped and
restarted with it. While a turn runs it is refused.

## Permissions

The client offers the agent no file or terminal capability; the agent
works inside its sandbox. It answers `session/request_permission`:

- `--approve-reads` (default): allow tool kinds `read` and `search`,
  refuse the rest and say so on stderr.
- `--approve-all`, `--deny-all`.

It picks the one-time option when the agent offers one, and answers
`cancelled` once a cancel was sent. The sandbox is the boundary, not this
policy: an agent that does not ask is not held back by it. `jcode acp`
ran a shell command that wrote a file without asking.

## Exit status

0 for `end_turn`, 130 for a cancelled turn or an interrupted command, 1 for
any other stop reason or error, 125 when the host or agent cannot start, 2
for a bad command line.

## urfave/cli

The commands are parsed by `github.com/urfave/cli/v3` (3.14.0) instead of
the `flag` package: seven more commands with their own flag sets and help.

`run`, `host`, `start`, `status`, `stop` and `wait` keep their flags and
behaviour. Compared on 55 command lines against the v0.24.0 binary: exit
status and stdout are the same on 50. The other five are `-h`, `--help`
and `help`, which now print help and exit 0 instead of printing usage and
exiting 2. Error text on stderr differs: one line, without the flag dump.

What had to be worked around, in `app.go`:

- urfave drops the first `--`, and `run sh -c x` and `run echo a -- b`
  have to keep working. The arguments are split at the first `--` before
  parsing, and `StopOnNthArg` ends flag parsing at the first word.
- A repeated flag is one value per use (`DisableSliceFlagSeparator`): a
  path or a URI SAN can hold a comma.
- No `help` subcommand under a command, or `run -- help` would be it.

## Dependency

`github.com/urfave/cli/v3` is new and direct. It has no dependencies of
its own outside tests. A module that imports only `pkg/acphost` does not
get it: checked with a scratch module, whose `go.mod`, `go.sum`, build
list and binary had dbus, compress and x/sys and no urfave. Go's module
graph pruning leaves out what the imported packages do not need. So the
CLI stays in the root module. The CLI binary grows from 11.8 MB to
14.8 MB, session code included.

## Tests

`session_test.go` runs the commands against a real detached host whose
agent is the test binary (`agent_test.go`): a conversation across prompts
and a host restart, the summary fallback, the permission policies, cancel
of a turn another process waits on, a detached turn with `attach` and
`cancel`, a config change, and `exec` with its exit statuses. A turn that
has to be running blocks in the agent on a FIFO or a cancel, so nothing
sleeps.

Checked by hand with `jcode acp`: `exec`, a three-prompt conversation with
a host stop in the middle (the agent loaded the session and still knew the
number from the first prompt), `--no-wait` with `cancel` and `attach`, and
a session under a bwrap sandbox from `-c`.

## Not done

- Queueing a prompt behind a running turn. It is refused.
- Images or files in a prompt, and the agent's `fs/*` and `terminal/*`
  client methods.
- Setting the agent's mode or model.
