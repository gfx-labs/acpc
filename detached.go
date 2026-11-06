package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/elee1766/acphost/pkg/proc"
	"golang.org/x/sys/unix"
)

// Detached mode (#200): start, status, stop and wait on a command run by
// proc.StartDetached, with its state in --dir. See
// docs/notes/200-cli-detached.md.

// isDetached reports whether cmd is one of the detached subcommands.
func isDetached(cmd string) bool {
	switch cmd {
	case "start", "status", "stop", "wait":
		return true
	}
	return false
}

// detached runs a detached subcommand. start takes the same flags as run.
func detached(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if args[0] == "start" {
		return start(args, stdin, stdout, stderr)
	}
	fs := flag.NewFlagSet("acphost "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "state dir given to start")
	grace := fs.Duration("grace", 0, "stop: SIGKILL the supervisor if it still runs after this long (0 waits)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *dir == "" || fs.NArg() > 0 {
		say(stderr, "acphost:", args[0], "needs --dir and no arguments")
		return 2
	}
	d, err := proc.Open(*dir)
	if err != nil {
		say(stderr, "acphost:", err)
		return 1
	}
	switch args[0] {
	case "status":
		return status(d, stdout)
	case "stop":
		return stop(d, *grace, stdout, stderr)
	}
	ex, err := d.Wait(context.Background())
	if err != nil {
		say(stderr, "acphost:", err)
		return 1
	}
	return exitCode(ex, stderr)
}

func start(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	// Parse as run, so start takes the same flags; --dir is the state
	// dir and the command runs in the cwd, as with host.
	o, err := parse(append([]string{"run"}, args[1:]...), stderr)
	if err != nil {
		if errors.Is(err, errUsage) {
			sayf(stderr, "%s", usage)
		} else if !errors.Is(err, flag.ErrHelp) {
			say(stderr, "acphost:", err)
		}
		return 2
	}
	if o.dir == "" || o.print || o.check || o.verbose {
		say(stderr, "acphost: start needs --dir and takes no --print, --check or -v")
		return 2
	}
	dir := o.dir
	o.dir = ""
	s, err := build(o, stdin)
	if err != nil {
		say(stderr, "acphost:", err)
		return 2
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		say(stderr, "acphost:", err)
		return 1
	}
	ps := procSpec(s)
	ps.Name = fmt.Sprintf("acphost-start-%d", os.Getpid())
	d, err := proc.StartDetached(dir, ps, o.grace)
	if err != nil {
		say(stderr, "acphost:", err)
		return 125
	}
	say(stdout, d.Pid)
	return 0
}

// statusOut is what status prints.
type statusOut struct {
	State string     `json:"state"` // running, exited or lost
	Pid   int        `json:"pid"`
	Exit  *proc.Exit `json:"exit,omitempty"`
}

func status(d *proc.Detached, stdout io.Writer) int {
	st := statusOut{State: "running", Pid: d.Pid}
	if !d.Running() {
		st.State = "lost" // supervisor died without exit.json
		if ex, ok := d.Exit(); ok {
			st.State, st.Exit = "exited", &ex
		}
	}
	writeJSON(stdout, st)
	return 0
}

func stop(d *proc.Detached, grace time.Duration, stdout, stderr io.Writer) int {
	if err := d.Stop(); err != nil {
		say(stderr, "acphost:", err)
		return 1
	}
	ctx := context.Background()
	if grace > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, grace)
		defer cancel()
	}
	ex, err := d.Wait(ctx)
	if errors.Is(err, context.DeadlineExceeded) {
		// The supervisor did not finish in time. Killing it kills the
		// command through its parent-death signal; no exit.json is left.
		_ = unix.Kill(d.Pid, unix.SIGKILL)
		say(stderr, "acphost: killed supervisor", d.Pid)
		return 1
	}
	if err != nil {
		say(stderr, "acphost:", err)
		return 1
	}
	writeJSON(stdout, ex)
	return 0
}

// exitCode maps an Exit to the CLI status, the same way run does.
func exitCode(ex proc.Exit, stderr io.Writer) int {
	if ex.SetupError != "" {
		say(stderr, "acphost: setup failed:", ex.SetupError)
		return 125
	}
	if ex.Signal != "" {
		return 128 + signalNumber(ex.Signal)
	}
	return ex.ExitCode
}
