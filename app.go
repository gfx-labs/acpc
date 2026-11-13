package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime/debug"
	"slices"
	"strconv"
	"time"

	"github.com/urfave/cli/v3"
)

// sessionLease is the default lease of a named session. It is only
// connected during a turn, so its lease is the idle time between prompts.
const sessionLease = 8 * time.Hour

// app is one command line: the streams the commands use and what followed
// the first "--".
type app struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	root           *cli.Command
	// rest is everything after the first "--" and dashed whether there
	// was one. run splits it off before urfave/cli parses, because
	// urfave/cli drops the "--" and the commands need to know where it was.
	rest   []string
	dashed bool
}

// exitStatus is a command's exit status, returned through urfave/cli as
// an error. The command has already printed what went wrong.
type exitStatus int

func (e exitStatus) Error() string { return "exit status " + strconv.Itoa(int(e)) }

// exit turns a status into an action's return value.
func exit(code int) error {
	if code == 0 {
		return nil
	}
	return exitStatus(code)
}

// fail prints err and returns code as the action's result.
func (a *app) fail(code int, err error) error {
	say(a.stderr, "acpc:", err)
	return exitStatus(code)
}

// run executes one command line and returns the exit status. An error
// that is not an exit status is a bad command line: 2.
func (a *app) run(args []string) int {
	err := a.exec(args)
	var st exitStatus
	switch {
	case err == nil:
		return 0
	case errors.As(err, &st):
		return int(st)
	}
	say(a.stderr, "acpc:", err)
	return 2
}

// exec parses args and runs the command's action.
func (a *app) exec(args []string) error {
	if i := slices.Index(args, "--"); i >= 0 {
		a.dashed, a.rest = true, args[i+1:]
		args = args[:i]
	}
	return a.root.Run(context.Background(), append([]string{"acpc"}, args...))
}

// argv is the agent command of new: the arguments from the
// first one that is not a flag, as the flag package split them before.
func (a *app) argv(cmd *cli.Command) []string {
	pos := cmd.Args().Slice()
	switch {
	case !a.dashed:
		return pos
	case len(pos) == 0:
		return a.rest
	}
	// The command began before the "--", so the "--" is one of its arguments.
	return append(append(pos, "--"), a.rest...)
}

// newApp builds the command tree. Every command reads and writes through
// the given streams, so tests run it in-process.
func newApp(stdin io.Reader, stdout, stderr io.Writer) *app {
	a := &app{stdin: stdin, stdout: stdout, stderr: stderr}
	commands := []*cli.Command{
		{
			Name: "status", Usage: "print whether a session's host is running, exited or lost, as JSON",
			Flags: sessionFlags(), Action: a.cmdDetached,
		},
		{
			Name: "stop", Usage: "stop a session's host and print its exit; the next prompt starts it again",
			Flags: append(sessionFlags(), graceFlag()), Action: a.cmdDetached,
		},
		{
			Name: "wait", Usage: "wait for a session's host to exit and exit with the agent's status",
			Flags: sessionFlags(), Action: a.cmdDetached,
		},
		{
			Name: "new", Usage: "create a session: start its host detached and open an ACP session with the agent",
			ArgsUsage: "-- CMD ARGS...", Action: a.cmdNew, StopOnNthArg: new(1),
			Flags: join(sessionFlags(), isolationFlags(), []cli.Flag{
				&cli.DurationFlag{Name: "lease", Value: sessionLease, Usage: "the host exits after no client for this long; the next prompt restarts it"},
			}),
		},
		{
			Name: "prompt", Usage: "send a prompt to a session and show the turn; queued if the session is busy",
			ArgsUsage: "TEXT... (no TEXT, or -, reads stdin)", Action: a.cmdPrompt,
			Flags: join(sessionFlags(), turnFlags(), fileFlags(), isolationFlags(), []cli.Flag{
				&cli.BoolFlag{Name: "no-wait", Usage: "return once the host has the prompt, or once it is queued behind a running turn; attach shows the turn"},
				&cli.StringFlag{Name: "queued", Hidden: true, Usage: "internal: wait for and send this queued prompt"},
			}),
		},
		{
			Name: "exec", Usage: "one prompt in a temporary session that is removed afterwards",
			ArgsUsage: "TEXT... -- CMD ARGS... (no TEXT, or -, reads stdin)", Action: a.cmdExec,
			Flags: join(turnFlags(), fileFlags(), isolationFlags()),
		},
		{
			Name: "attach", Usage: "show what a session did since this client last looked, and the running turn",
			Flags: join(sessionFlags(), turnFlags()), Action: a.cmdAttach,
		},
		{
			Name: "cancel", Usage: "cancel a session's running turn",
			Action: a.cmdCancel,
			Flags: join(sessionFlags(), []cli.Flag{
				&cli.BoolFlag{Name: "queued", Usage: "also drop the prompts waiting in the queue"},
			}),
		},
		{
			Name: "ls", Usage: "list the sessions of the working directory",
			Action: a.cmdLs,
			Flags: []cli.Flag{
				&cli.BoolFlag{Name: "all", Usage: "sessions of every working directory"},
				&cli.StringFlag{Name: "format", Value: formatText, Usage: "text or json"},
			},
		},
		{
			Name: "rm", Usage: "stop a named session and delete its directory",
			Flags:  []cli.Flag{&cli.StringFlag{Name: "session", Aliases: []string{"s"}, Usage: "session `NAME`"}},
			Action: a.cmdRm,
		},
	}
	a.root = &cli.Command{
		Name:    "acpc",
		Usage:   "an ACP client: prompt an agent from a shell, with the conversation kept under a name",
		Version: version(),
		Reader:  stdin, Writer: stdout, ErrWriter: stderr,
		// Actions return their exit status as an error; run maps it.
		ExitErrHandler: func(context.Context, *cli.Command, error) {},
		OnUsageError:   usageError,
		Commands:       commands,
		Action: func(_ context.Context, cmd *cli.Command) error {
			if cmd.NArg() > 0 {
				return fmt.Errorf("unknown command %q (see acpc --help)", cmd.Args().First())
			}
			return errors.New("no command (see acpc --help)")
		},
	}
	for _, c := range commands {
		c.OnUsageError = usageError
		// A repeated flag is one value per use: a path or a URI SAN may
		// hold a comma.
		c.DisableSliceFlagSeparator = true
		// No help subcommand, or a command to run named help would be it.
		c.HideHelpCommand = true
	}
	return a
}

// usageError returns a parse error as it is, so run prints it and exits 2
// instead of urfave/cli printing the command's whole help.
func usageError(_ context.Context, _ *cli.Command, err error, _ bool) error { return err }

func join(sets ...[]cli.Flag) []cli.Flag {
	var out []cli.Flag
	for _, s := range sets {
		out = append(out, s...)
	}
	return out
}

// isolationFlags are the config file and its overrides.
func isolationFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "config", Aliases: []string{"c"}, Usage: "config `FILE` (YAML or JSON), - for stdin"},
		&cli.StringFlag{Name: "network", Usage: "override sandbox network: host, none, pasta"},
		&cli.StringSliceFlag{Name: "ro", Usage: "add a read-only bind `SRC[:DST]` (repeatable)"},
		&cli.StringSliceFlag{Name: "rw", Usage: "add a read-write bind `SRC[:DST]` (repeatable)"},
		&cli.StringSliceFlag{Name: "join", Usage: "join a namespace `TYPE=PATH` (repeatable)"},
	}
}

// sessionFlags name a session directory: by name, or directly.
func sessionFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "session", Aliases: []string{"s"}, Usage: "session `NAME`, scoped to the working directory"},
		&cli.StringFlag{Name: "dir", Usage: "session `DIR`, instead of a name"},
	}
}

// graceFlag is stop's --grace.
func graceFlag() cli.Flag {
	return &cli.DurationFlag{Name: "grace", Value: stopGrace, Usage: "SIGKILL the host if it still runs after this long (0 waits)"}
}

// turnFlags are how a turn is shown and how permission requests are answered.
func turnFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "format", Value: formatText, Usage: "text, json (journal frames, one per line) or quiet (agent text only)"},
		&cli.BoolFlag{Name: "approve-all", Usage: "allow every permission request"},
		&cli.BoolFlag{Name: "approve-reads", Usage: "allow read and search requests, refuse the rest (default)"},
		&cli.BoolFlag{Name: "deny-all", Usage: "refuse every permission request"},
	}
}

// fileFlags are the files sent with a prompt.
func fileFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringSliceFlag{Name: "file", Aliases: []string{"f"}, Usage: "send `PATH` with the prompt: an image as an image, another file embedded or linked (repeatable)"},
	}
}

// version is the module version the binary was built from, or dev.
func version() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}
