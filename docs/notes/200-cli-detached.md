# 200: acphost CLI detached mode

2026-11-06. Item 3 of #197.

```
acphost start  -c FILE --dir STATEDIR [--grace 10s] [flags] -- CMD ARGS...
acphost status --dir STATEDIR
acphost stop   --dir STATEDIR [--grace 30s]
acphost wait   --dir STATEDIR
```

Thin wrappers over `proc.StartDetached` and `proc.Open` (note 159).

- `start` takes `run`'s flags and config. `--dir` is the state dir
  (supervisor.pid, supervisor.lock, output.log, isolation.json,
  exit.json); the command runs in the cwd, as with `host`. Its `--grace`
  is the SIGTERM to SIGKILL delay the supervisor uses on stop. It prints
  the supervisor pid and returns once the command started; a start or
  setup error exits 125. `--print`, `--check` and `-v` are refused.
- `status` prints `{"state","pid","exit"}`: `running`, `exited` (with
  exit.json) or `lost` (the supervisor died without writing exit.json).
- `stop` sends SIGTERM to the supervisor, waits, and prints the exit
  JSON. With `--grace` it SIGKILLs the supervisor if it is still there
  after that long (the command dies with it through the parent-death
  signal) and exits 1.
- `wait` blocks on the lock (no polling) and exits with the command's
  status, mapped like `run`: 128+n for a signal, 125 for a setup error.
