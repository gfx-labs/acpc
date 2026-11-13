// Command acpc is an ACP client: it keeps a conversation with an ACP agent
// under a name and prompts it from a shell. The agent runs under an
// acphost host (github.com/gfx-labs/acphost), detached and with its
// isolation, so the conversation outlives the command and the journal
// shows what happened while nobody watched.
//
//	acpc new    -s NAME [-c sandbox.yaml] -- CMD ARGS...
//	acpc prompt -s NAME TEXT...
//	acpc exec   [-c sandbox.yaml] TEXT -- CMD ARGS...
//	acpc attach|cancel|rm -s NAME
//	acpc status|stop|wait -s NAME
//	acpc ls
//
// See docs/notes/290-cli-sessions.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/gfx-labs/acphost/pkg/acphost"
	"github.com/gfx-labs/acphost/pkg/proc"
	"github.com/gfx-labs/acphost/pkg/sandbox"
	"github.com/urfave/cli/v3"
	"golang.org/x/sys/unix"
)

func main() {
	// The detached host is this binary re-exec'd.
	acphost.MaybeChild()
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return newApp(stdin, stdout, stderr).run(args)
}

// isolation is the config file and the flags that override it.
type isolation struct {
	config, network string
	ro, rw, join    []string
}

func (i isolation) set() bool {
	return i.config != "" || i.network != "" || len(i.ro) > 0 || len(i.rw) > 0 || len(i.join) > 0
}

func isolationFrom(cmd *cli.Command) isolation {
	return isolation{
		config: cmd.String("config"), network: cmd.String("network"),
		ro: cmd.StringSlice("ro"), rw: cmd.StringSlice("rw"), join: cmd.StringSlice("join"),
	}
}

// load reads the config file and applies the flag overrides.
func (i isolation) load(stdin io.Reader) (Config, error) {
	c, err := readConfig(i.config, stdin)
	if err != nil {
		return c, err
	}
	if i.network != "" || len(i.ro) > 0 || len(i.rw) > 0 {
		if c.Sandbox == nil {
			c.Sandbox = &sandbox.Sandbox{}
		}
		if i.network != "" {
			c.Sandbox.Network = i.network
		}
		for _, m := range i.ro {
			c.Sandbox.RO = append(c.Sandbox.RO, parseMount(m))
		}
		for _, m := range i.rw {
			c.Sandbox.RW = append(c.Sandbox.RW, parseMount(m))
		}
	}
	for _, j := range i.join {
		t, p, ok := strings.Cut(j, "=")
		if !ok {
			return c, fmt.Errorf("--join %q: want TYPE=PATH", j)
		}
		if c.Join == nil {
			c.Join = map[string]string{}
		}
		c.Join[t] = p
	}
	return c, nil
}

// cmdDetached is status, stop and wait on a session's host.
func (a *app) cmdDetached(_ context.Context, cmd *cli.Command) error {
	if cmd.NArg() > 0 || a.dashed {
		return fmt.Errorf("%s takes no arguments", cmd.Name)
	}
	t, err := resolve(cmd.String("session"), cmd.String("dir"), defaultSession)
	if err != nil {
		return err
	}
	d, err := proc.Open(t.dir)
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

// statusOut is what status prints.
type statusOut struct {
	State string     `json:"state"` // running, exited or lost
	Pid   int        `json:"pid"`
	Exit  *proc.Exit `json:"exit,omitempty"`
}

func status(d *proc.Detached, stdout io.Writer) int {
	st := statusOut{State: "running", Pid: d.Pid}
	if !d.Running() {
		st.State = "lost" // the host died without exit.json
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
		say(stderr, "acpc: killed host", d.Pid)
		return 1
	}
	if err != nil {
		say(stderr, "acpc:", err)
		return 1
	}
	writeJSON(stdout, ex)
	return 0
}

// stopWait stops d and waits for its exit. With a grace it SIGKILLs a host
// that has not finished by then and returns context.DeadlineExceeded.
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

// exitCode maps an Exit to a status: 125 for a setup failure, 128+n for a
// signal, else the agent's exit code.
func exitCode(ex proc.Exit, stderr io.Writer) int {
	if ex.SetupError != "" {
		say(stderr, "acpc: setup failed:", ex.SetupError)
		return 125
	}
	if ex.Signal != "" {
		return 128 + signalNumber(ex.Signal)
	}
	return ex.ExitCode
}

func writeJSON(w io.Writer, v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	_, _ = w.Write(append(b, '\n'))
}

// say and sayf write CLI output; a failed write to the terminal has
// nowhere better to be reported.
func say(w io.Writer, a ...any) { _, _ = fmt.Fprintln(w, a...) }

func sayf(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
