package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/gfx-labs/acphost/pkg/acphost"
	"github.com/gfx-labs/acphost/pkg/proc"
	"github.com/urfave/cli/v3"
	"golang.org/x/sys/unix"
)

// Detached mode (#200, #211): start, status, stop and wait on a command
// run by proc.StartDetached, or with start --host an ACP host run by
// acphost.StartDetached, with its state in --dir. See
// docs/notes/200-cli-detached.md and docs/notes/211-cli-host-detached.md.

// cmdDetached is status, stop and wait. The dir is --dir, or a named
// session's (-s), whose host keeps the same files.
func (a *app) cmdDetached(_ context.Context, cmd *cli.Command) error {
	if cmd.NArg() > 0 || a.dashed {
		return fmt.Errorf("%s takes no arguments", cmd.Name)
	}
	ref, err := sessionRef(cmd)
	if err != nil {
		return err
	}
	d, err := proc.Open(ref.dir)
	if err != nil {
		return a.fail(1, err)
	}
	switch cmd.Name {
	case "status":
		return exit(status(d, a.stdout))
	case "stop":
		return exit(stop(d, cmd.Duration("grace"), a.stdout, a.stderr))
	}
	ex, err := d.Wait(context.Background())
	if err != nil {
		return a.fail(1, err)
	}
	return exit(exitCode(ex, a.stderr))
}

// cmdStart is start. It takes the same flags as run; --dir is the state
// dir and the command runs in the cwd, as with host.
func (a *app) cmdStart(_ context.Context, cmd *cli.Command) error {
	o, err := a.opts(cmd)
	if err != nil {
		return err
	}
	if o.dir == "" || o.print || o.check || o.verbose {
		return errors.New("start needs --dir and takes no --print, --check or -v")
	}
	dir := o.dir
	if o.detachHost {
		return exit(startHost(o, a.stdin, a.stdout, a.stderr))
	}
	o.dir = ""
	s, err := build(o, a.stdin)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return a.fail(1, err)
	}
	ps := procSpec(s)
	ps.Name = fmt.Sprintf("acphost-start-%d", os.Getpid())
	d, err := proc.StartDetached(dir, ps, o.grace)
	if err != nil {
		return a.fail(125, err)
	}
	say(a.stdout, d.Pid)
	return nil
}

// startHost is start --host: write spec.json into --dir as host does,
// then run the host detached. The agent runs in the cwd.
func startHost(o opts, stdin io.Reader, stdout, stderr io.Writer) int {
	o.mode = "host" // build names the session after --dir and runs the agent in the cwd
	s, err := build(o, stdin)
	if err != nil {
		say(stderr, "acphost:", err)
		return 2
	}
	dir := o.dir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		say(stderr, "acphost:", err)
		return 1
	}
	if acphost.Alive(dir) {
		// Checked before WriteSpec, which would replace a running host's spec.
		say(stderr, "acphost: a host already running in", dir)
		return 125
	}
	if err := acphost.CheckSpawn(s, false, acphost.WithHostDir(dir)); err != nil {
		say(stderr, "acphost:", err)
		return 125
	}
	if err := acphost.WriteSpec(dir, s); err != nil {
		say(stderr, "acphost:", err)
		return 1
	}
	d, err := acphost.StartDetached(dir, nil)
	if err != nil {
		say(stderr, "acphost:", err)
		return 125
	}
	say(stderr, "acphost: socket", acphost.SocketPath(dir))
	if s.Listen != "" {
		// The host wrote listen.json before the agent started, which is
		// when StartDetached returned.
		if l, ok := acphost.ReadListen(dir); ok {
			sayListen(stderr, l)
		}
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
	ex, err := stopWait(d, grace)
	if errors.Is(err, context.DeadlineExceeded) {
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

// stopWait stops d and waits for its exit. With a grace it SIGKILLs a
// supervisor that has not finished by then and returns
// context.DeadlineExceeded. Killing it kills the command through its
// parent-death signal; no exit.json is left.
func stopWait(d *proc.Detached, grace time.Duration) (proc.Exit, error) {
	if err := d.Stop(); err != nil {
		return proc.Exit{}, err
	}
	ctx := context.Background()
	if grace > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, grace)
		defer cancel()
	}
	ex, err := d.Wait(ctx)
	if errors.Is(err, context.DeadlineExceeded) {
		_ = unix.Kill(d.Pid, unix.SIGKILL)
	}
	return ex, err
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
