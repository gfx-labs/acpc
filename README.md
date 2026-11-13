# acpc

An [ACP](https://agentclientprotocol.com) client for the shell, like
[acpx](https://github.com/openclaw/acpx): it keeps a conversation with an
ACP agent under a name and prompts it turn by turn. The agent runs in a
detached [acphost](https://github.com/gfx-labs/acphost) host, so the
conversation survives the command, a host restart and a reboot, and can
run under a bubblewrap sandbox, a user namespace and a cgroup. ACP is
spoken with [go-acp](https://github.com/gfx-labs/go-acp).

Linux only.

```sh
go install github.com/gfx-labs/acpc@latest
```

## Use

```sh
$ acpc exec 'Reply with 42 only' -- jcode acp
[acpc] spawning agent: jcode acp
[acpc] host pid 1557661
[client] initialize (running)
[client] initialized protocol version 1, agent jcode 0.99.1, loadSession true
[client] session/new cwd /home/me/work (running)
[client] session session_U8_...
=== turn 02:04:26: Reply with 42 only
42
=== turn end 02:04:28: end_turn
```

A named session keeps the conversation between commands:

```sh
acpc new -s work -- jcode acp
acpc prompt -s work 'Remember the word PELICAN.'
acpc stop -s work                       # the host exits
acpc prompt -s work 'Which word?'       # restarted, conversation loaded
acpc prompt -s work --no-wait 'run the tests'
acpc attach -s work                     # show that turn
acpc cancel -s work
acpc ls
acpc rm -s work
```

- The agent is the command after `--`. There is no agent registry; the
  agent picks its model and mode.
- `-s` defaults to `default`. A name belongs to the working directory it
  was created in. `--dir DIR` uses a session directory of your choice.
- `prompt` reads stdin with no text or `-`. Output is streamed as the
  agent sends it.
- `--format text|json|quiet`: the rendered turn, journal frames, or the
  agent's text only.
- `--approve-reads` (default), `--approve-all`, `--deny-all` answer
  permission requests.
- `-c FILE`, `--ro`, `--rw`, `--network`, `--join` put the agent under
  acphost's isolation. The config file has acphost's `sandbox`, `user`,
  `cgroup`, `env` and `join` sections.
- If the host stopped, the next `prompt` starts it and the agent loads
  the conversation (`session/load`), or gets a summary of the journal if
  it cannot.
- Exit status: 0 `end_turn`, 130 cancelled, 1 another stop reason or an
  error, 125 the agent could not start, 2 a bad command line.

Details: [docs/notes/290-cli-sessions.md](docs/notes/290-cli-sessions.md).

## Not done

- The agent's `fs/*` and `terminal/*` client methods. The agent does its
  file and command work itself, inside its sandbox.
- MCP server config, queued prompts, images or files in a prompt.

## Check

```sh
go build ./... && go vet ./... && go test ./... && golangci-lint run ./...
```
