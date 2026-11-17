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
go install github.com/gfx-labs/acpc/cmd/acpc@latest
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
- `new --protocol 2` and `exec --protocol 2` (hidden, experimental) speak
  the ACP v2 draft instead of version 1. See
  `docs/notes/2026-11-16-acp-v2.md`.
- A prompt for a session in use waits its turn: prompts run one after
  another in the order they came. With `--no-wait` a detached acpc waits
  for it, logging to `queue.log` in the session directory.
  `cancel --queued` also drops the waiting prompts.
- `-f FILE` (repeatable) sends a file with the prompt: an image as an
  image block, another file embedded in the prompt, or as a link to the
  file when the agent does not take embedded context. An agent that does
  not take images refuses one.
- Exit status: 0 `end_turn`, 130 cancelled, 1 another stop reason or an
  error, 125 the agent could not start, 2 a bad command line.

## A2A

```sh
acpc a2a --listen 127.0.0.1:8080 --cwd ~/work -c agent.yaml -- towa serve
```

serves the agent over [A2A](https://a2a-protocol.org) v1.0 (JSON-RPC,
with streaming), with the agent card at `/.well-known/agent-card.json`.

- An A2A context is an acpc session, so it has a host of its own, keeps
  its conversation across tasks and restarts, and waits in the same queue
  as `acpc prompt`. A task is one turn: the agent's text streams as an
  artifact, each tool call's title as a working status, and the stop
  reason ends the task. Canceling a task cancels the turn.
- Every context runs in `--cwd` under the isolation of `-c` and the
  isolation flags.
- It listens only on loopback or `unix:PATH` and has no authentication.
  Put a proxy in front for anything else. `--card FILE` (YAML or JSON) is
  merged into the agent card: give the proxy's URL in
  `supportedInterfaces` and its auth in `securitySchemes` and
  `securityRequirements`.
- `--ask` puts each permission request to the A2A client: the task stops
  at `input-required` with the options, and a reply on that task naming
  one answers it. The permission flags work as for `prompt`.

Details: [docs/notes/290-cli-sessions.md](docs/notes/290-cli-sessions.md),
[docs/notes/2026-11-13-prompt-queue.md](docs/notes/2026-11-13-prompt-queue.md).

## Library

`pkg/acpc` is the client without the command line: `acpc.Open` takes a
session's lock, and `Session` creates it (`Create`), runs turns
(`Prompt`, `Submit`), follows another client's turn (`AwaitTurn`) and
cancels one. Output goes to an `acpc.Output` that gets journal frames.

## Not done

- The agent's `fs/*` and `terminal/*` client methods. The agent does its
  file and command work itself, inside its sandbox.
- MCP servers in `session/new`. acpc sends none: the agent reads its own
  MCP config.

## Check

```sh
go build ./... && go vet ./... && go test ./... && golangci-lint run ./...
```

## Releases

Master is squash-only and PR titles are conventional commits.
release-please keeps a release PR open; merging it tags `vX.Y.Z`.
