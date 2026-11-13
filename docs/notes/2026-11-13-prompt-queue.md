# Prompt queue, files in prompts, pkg/acpc

2026-11-13

## Queue

A session has one client at a time (client.lock), because a second
connection replaces the first at the host. Before this, `prompt` on a
session in use failed. Now each prompt waits in `<session>/queue/`:

- One file per waiting prompt, `%020d-<pid>.json` (unix nanoseconds), so
  names sort in arrival order. It holds the prompt text, files, the
  isolation flags and the permission policy.
- The process waiting for it holds a flock on the file. A file nobody
  holds belongs to a waiter that died and is removed by the next reader.
  No pid checks, nothing to clean up after a crash.
- A prompt runs when its file is first and it can take client.lock. It
  then connects and, if the host reports a turn running (one sent with
  `--no-wait`), waits for that turn's end before sending its own.
- `--no-wait` on a busy session starts a detached `acpc prompt --queued
  FILE` (setsid) that inherits the file's lock as fd 3 and logs to
  `queue.log`. The command returns at once.
- Isolation flags given with a queued prompt apply when it runs, after
  the turn before it ended, since the host restarts for them.
- `cancel` takes a slot at the front of the queue (name `0000...`) before
  it interrupts the client, so a waiting prompt cannot start a turn in
  between. `cancel --queued` also removes the waiting files; their
  waiters see the file gone and exit.

Polling (200 ms) is used for the wait: inotify plus flock wakeups would
need more code for a wait that is usually seconds or minutes long.

## Files

`-f PATH` reads the file when the prompt is made, so a queued prompt
sends what the file held then. Kinds follow the agent's
`promptCapabilities`: image block for `image/*` (refused without
`image`), embedded text or blob resource with `embeddedContext`, else a
`resource_link` to the file URI. Total 40 MiB, under the host's 64 MiB
frame limit after base64.

## Layout

The client moved to `pkg/acpc` so other Go programs can run sessions
without the command line. It returns errors; exit codes, output formats
and `acpc: ` prefixes stay in `cmd/acpc`. The detached waiter's argv is
given by the caller (`StartWaiter`), since the library cannot know the
binary. `go install github.com/gfx-labs/acpc/cmd/acpc@latest` is the new
install path.
