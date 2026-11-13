package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/gfx-labs/acpc/pkg/acpc"

	acp "github.com/gfx-labs/go-acp"

	"github.com/gfx-labs/acphost/pkg/acphost"
	"github.com/gfx-labs/acphost/pkg/proc"
	"github.com/urfave/cli/v3"
)

// Session commands: new, prompt, exec, attach, cancel, ls and rm. See
// docs/notes/290-cli-sessions.md.

const (
	formatText  = "text"
	formatJSON  = "json"
	formatQuiet = "quiet"

	// execLease only matters if exec is killed: it is connected until it
	// stops its host.
	execLease = time.Minute
)

// output shows journal frames in one of the formats.
type output struct {
	w      io.Writer
	format string
	text   acphost.Renderer
	open   bool // quiet: the agent's text has not ended its line
}

func (o *output) Frame(fr acphost.Frame) {
	switch o.format {
	case formatJSON:
		_, _ = o.w.Write(acphost.RawFrame(fr))
	case formatQuiet:
		if s := agentText(fr); s != "" {
			_, _ = io.WriteString(o.w, s)
			o.open = !strings.HasSuffix(s, "\n")
		}
	default:
		o.text.Render(fr)
	}
}

// End finishes the last line of quiet output.
func (o *output) End() {
	if o != nil && o.open {
		_, _ = io.WriteString(o.w, "\n")
		o.open = false
	}
}

// agentText is the text of an agent message chunk, or "".
func agentText(fr acphost.Frame) string {
	var m struct {
		Method string `json:"method"`
		Params struct {
			Update struct {
				Kind    string `json:"sessionUpdate"`
				Content struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"update"`
		} `json:"params"`
	}
	if fr.From != "agent" || json.Unmarshal(fr.Msg, &m) != nil || m.Method != "session/update" || m.Params.Update.Kind != "agent_message_chunk" {
		return ""
	}
	return m.Params.Update.Content.Text
}

// turnOptions reads --format and the permission flags.
func turnOptions(cmd *cli.Command, w io.Writer) (*output, string, error) {
	format := cmd.String("format")
	if format != formatText && format != formatJSON && format != formatQuiet {
		return nil, "", fmt.Errorf("--format %q: want text, json or quiet", format)
	}
	policy, n := acpc.ApproveReads, 0
	for _, p := range []string{acpc.ApproveAll, acpc.ApproveReads, acpc.DenyAll} {
		if cmd.Bool(p) {
			policy = p
			n++
		}
	}
	if n > 1 {
		return nil, "", errors.New("give one of --approve-all, --approve-reads and --deny-all")
	}
	return &output{w: w, format: format, text: acphost.Renderer{W: w}}, policy, nil
}

// promptText is the prompt: the arguments joined by spaces, or stdin when
// there are none or the only one is "-".
func (a *app) promptText(args []string, stdinTaken bool) (string, error) {
	text := strings.Join(args, " ")
	if len(args) == 0 || (len(args) == 1 && args[0] == "-") {
		if stdinTaken || a.stdin == nil {
			return "", errors.New("no prompt text (stdin is not available for it)")
		}
		b, err := io.ReadAll(a.stdin)
		if err != nil {
			return "", err
		}
		text = string(b)
	}
	if strings.TrimSpace(text) == "" {
		return "", errors.New("empty prompt")
	}
	return text, nil
}

// sessionSpec is the spec of a session's host: the agent argv runs in the
// working directory under the given isolation.
func (a *app) sessionSpec(name string, iso isolation, lease time.Duration, argv []string) (acphost.Spec, error) {
	c, err := iso.load(a.stdin)
	if err != nil {
		return acphost.Spec{}, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return acphost.Spec{}, err
	}
	return acphost.Spec{
		Protocol: acphost.Protocol, Session: name, Command: argv[0], Args: argv[1:],
		Env: c.Env, Dir: cwd, LeaseMS: lease.Milliseconds(),
		Sandbox: c.Sandbox, User: c.User, Cgroup: c.Cgroup, Join: c.Join,
		Listen: c.Listen, TLS: c.TLS,
	}, nil
}

// hostName is Spec.Session for t: one path element (a cgroup and a GC
// root are named after it) that differs between working directories.
func hostName(t acpc.Target) string {
	if t.Name == "" {
		return filepath.Base(t.Dir)
	}
	return filepath.Base(filepath.Dir(t.Dir)) + "-" + t.Name
}

// options are the acpc.Options for a command's session.
func (a *app) options(out *output, policy string) acpc.Options {
	o := acpc.Options{Policy: policy, Log: prefixed{a.stderr}, ClientInfo: acp.Implementation{Name: "acpc", Version: version()}}
	if out != nil {
		o.Output = out
	}
	return o
}

// prefixed writes the session's notices with an "acpc: " prefix.
type prefixed struct{ w io.Writer }

func (p prefixed) Write(b []byte) (int, error) {
	if _, err := io.WriteString(p.w, "acpc: "); err != nil {
		return 0, err
	}
	return p.w.Write(b)
}

// report prints err and returns the exit status for it: 130 for an
// interrupt, 125 for a host that could not start, else 1. For a host that
// stopped it adds the end of the host's log, where the agent's stderr is.
func (a *app) report(dir string, err error) int {
	if errors.Is(err, acpc.ErrInterrupted) {
		return 130
	}
	say(a.stderr, "acpc:", err)
	var gone *acpc.HostGoneError
	if errors.As(err, &gone) {
		if tail := acpc.HostLogTail(dir); tail != "" {
			sayf(a.stderr, "acpc: end of %s:\n%s\n", filepath.Join(dir, acphost.LogFile), tail)
		}
		// bwrap reports a command it cannot run after the host is up.
		if gone.Exit.Reason == acphost.ReasonSpawnFailed {
			return 125
		}
	}
	var spawn *acpc.SpawnError
	if errors.As(err, &spawn) {
		return 125
	}
	return 1
}

// turnStatus is the exit status for a finished turn: 0 for end_turn, 130
// for a cancelled one, 1 for any other stop reason or an error.
func (a *app) turnStatus(dir string, stop acp.StopReason, err error) int {
	if err != nil {
		return a.report(dir, err)
	}
	switch stop {
	case acp.StopReasonEndTurn:
		return 0
	case acp.StopReasonCancelled:
		return 130
	}
	say(a.stderr, "acpc: the turn stopped:", stop)
	return 1
}

// turn sends in on s and returns the exit status once the turn ended.
func (a *app) turn(s *acpc.Session, dir string, out *output, in acpc.PromptInput) int {
	stop, err := s.Prompt(in)
	code := a.turnStatus(dir, stop, err)
	if out != nil {
		out.End()
	}
	return code
}

func (a *app) cmdNew(_ context.Context, cmd *cli.Command) error {
	argv := a.argv(cmd)
	if len(argv) == 0 {
		return errors.New("new: no agent command (give it after --)")
	}
	t, err := acpc.Resolve(cmd.String("session"), cmd.String("dir"), acpc.DefaultSession)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(t.Dir, acphost.SpecFile)); err == nil {
		return a.fail(1, fmt.Errorf("%s already exists", t))
	}
	spec, err := a.sessionSpec(hostName(t), isolationFrom(cmd), cmd.Duration("lease"), argv)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(t.Dir, 0o700); err != nil {
		return a.fail(1, err)
	}
	// Nothing of new is shown, but the journal is read: the first prompt
	// then starts after the handshake.
	s, err := acpc.Open(t, spec, a.options(&output{w: io.Discard, format: formatQuiet}, acpc.DenyAll))
	if err != nil {
		return a.fail(1, err)
	}
	err = s.Create()
	s.Close()
	if err != nil {
		code := a.report(t.Dir, err)
		if err := acpc.Discard(t); err != nil {
			say(a.stderr, "acpc:", err)
		}
		return exit(code)
	}
	say(a.stdout, t.Dir)
	return nil
}

func (a *app) cmdPrompt(_ context.Context, cmd *cli.Command) error {
	t, err := acpc.Resolve(cmd.String("session"), cmd.String("dir"), acpc.DefaultSession)
	if err != nil {
		return err
	}
	if path := cmd.String("queued"); path != "" {
		return exit(a.runWaiter(t, path))
	}
	out, policy, err := turnOptions(cmd, a.stdout)
	if err != nil {
		return err
	}
	iso := isolationFrom(cmd)
	text, err := a.promptText(append(cmd.Args().Slice(), a.rest...), iso.config == "-")
	if err != nil {
		return err
	}
	files, err := acpc.ReadAttachments(cmd.StringSlice("file"))
	if err != nil {
		return err
	}
	q := acpc.Queued{Input: acpc.PromptInput{Text: text, Files: files}, Policy: policy, NoWait: cmd.Bool("no-wait")}
	if iso.set() {
		c, err := iso.load(a.stdin)
		if err != nil {
			return err
		}
		q.Config = &c
	}
	spec, err := acpc.ReadSessionSpec(t)
	if err != nil {
		return a.fail(1, err)
	}
	e, err := acpc.Enqueue(t.Dir, q)
	if err != nil {
		return a.fail(1, err)
	}
	handedOff := false
	defer func() {
		if !handedOff {
			e.Done()
		}
	}()
	ahead := len(acpc.QueueAhead(t.Dir, filepath.Base(e.Path)))
	_, inUse := acpc.LockHolder(t.Dir)
	if q.NoWait && (ahead > 0 || inUse) {
		handedOff = true
		return exit(a.handOff(t, e, ahead))
	}
	if ahead > 0 || inUse {
		sayf(a.stderr, "acpc: queued behind %d prompt(s) in %s\n", ahead+1, t)
	}
	s, err := acpc.OpenQueued(t, spec, a.options(out, policy), e)
	if errors.Is(err, acpc.ErrInterrupted) {
		return exit(130)
	}
	if err != nil {
		return a.fail(1, err)
	}
	defer s.Close()
	if err := s.Reach(q); err != nil {
		return exit(a.report(t.Dir, err))
	}
	if q.NoWait && s.Busy() {
		// A turn started with --no-wait runs; a waiter sends this one after it.
		s.Close()
		handedOff = true
		return exit(a.handOff(t, e, 0))
	}
	if err := s.AwaitRunning(); err != nil {
		return exit(a.report(t.Dir, err))
	}
	if q.NoWait {
		if err := s.Submit(q.Input); err != nil {
			return exit(a.report(t.Dir, err))
		}
		return nil
	}
	return exit(a.turn(s, t.Dir, out, q.Input))
}

// handOff starts a detached waiter for the queued --no-wait prompt e.
func (a *app) handOff(t acpc.Target, e *acpc.Entry, ahead int) int {
	exe, err := os.Executable()
	if err == nil {
		err = acpc.StartWaiter(t, e, []string{exe, "prompt", "--dir", t.Dir, "--queued", e.Path})
	}
	_ = e.File.Close() // the waiter holds the lock now
	if err != nil {
		_ = os.Remove(e.Path)
		say(a.stderr, "acpc:", err)
		return 1
	}
	sayf(a.stderr, "acpc: queued behind %d prompt(s) or a running turn in %s\n", ahead+1, t)
	return 0
}

// runWaiter is a detached acpc for one queued --no-wait prompt: it waits
// for its turn and sends the prompt. Its output is in queue.log; the turn
// is shown by the next attach.
func (a *app) runWaiter(t acpc.Target, path string) int {
	e, q, err := acpc.Adopt(path, acpc.QueuedFD)
	if err != nil {
		say(a.stderr, "acpc:", err)
		return 1
	}
	defer e.Done()
	spec, err := acpc.ReadSessionSpec(t)
	if err != nil {
		say(a.stderr, "acpc:", err)
		return 1
	}
	s, err := acpc.OpenQueued(t, spec, a.options(nil, q.Policy), e)
	if err != nil {
		say(a.stderr, "acpc:", err)
		return 1
	}
	defer s.Close()
	if err := s.Reach(q); err != nil {
		return a.report(t.Dir, err)
	}
	if err := s.AwaitRunning(); err != nil {
		return a.report(t.Dir, err)
	}
	if err := s.Submit(q.Input); err != nil {
		return a.report(t.Dir, err)
	}
	return 0
}

func (a *app) cmdExec(_ context.Context, cmd *cli.Command) error {
	if len(a.rest) == 0 {
		return errors.New("exec: no agent command (give it after --)")
	}
	out, policy, err := turnOptions(cmd, a.stdout)
	if err != nil {
		return err
	}
	iso := isolationFrom(cmd)
	text, err := a.promptText(cmd.Args().Slice(), iso.config == "-")
	if err != nil {
		return err
	}
	files, err := acpc.ReadAttachments(cmd.StringSlice("file"))
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "acpc-exec-")
	if err != nil {
		return a.fail(1, err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	t := acpc.Target{Dir: dir}
	spec, err := a.sessionSpec(hostName(t), iso, execLease, a.rest)
	if err != nil {
		return err
	}
	opts := a.options(out, policy)
	// As acpx does, exec shows the handshake as it happens.
	if out.format == formatText {
		opts.Trace = out.w
	}
	s, err := acpc.Open(t, spec, opts)
	if err != nil {
		return a.fail(1, err)
	}
	code := 0
	if err := s.Create(); err != nil {
		code = a.report(dir, err)
	} else {
		code = a.turn(s, dir, out, acpc.PromptInput{Text: text, Files: files})
	}
	s.Close()
	if err := acpc.StopHost(dir); err != nil {
		say(a.stderr, "acpc:", err)
	}
	return exit(code)
}

func (a *app) cmdAttach(_ context.Context, cmd *cli.Command) error {
	if cmd.NArg() > 0 || len(a.rest) > 0 {
		return errors.New("attach takes no arguments")
	}
	t, err := acpc.Resolve(cmd.String("session"), cmd.String("dir"), acpc.DefaultSession)
	if err != nil {
		return err
	}
	out, policy, err := turnOptions(cmd, a.stdout)
	if err != nil {
		return err
	}
	s, err := acpc.OpenExisting(t, a.options(out, policy))
	if err != nil {
		return a.fail(1, err)
	}
	defer s.Close()
	defer out.End()
	if !acphost.Alive(t.Dir) {
		s.CatchUp()
		say(a.stderr, "acpc: the host is not running (the next prompt starts it)")
		return nil
	}
	if err := s.Connect(); err != nil {
		return exit(a.report(t.Dir, err))
	}
	s.CatchUp()
	if !s.Busy() {
		return nil
	}
	// A signal here ends attach, not the turn: cancel does that.
	stop, err := s.AwaitTurn()
	return exit(a.turnStatus(t.Dir, stop, err))
}

func (a *app) cmdCancel(_ context.Context, cmd *cli.Command) error {
	if cmd.NArg() > 0 || len(a.rest) > 0 {
		return errors.New("cancel takes no arguments")
	}
	t, err := acpc.Resolve(cmd.String("session"), cmd.String("dir"), acpc.DefaultSession)
	if err != nil {
		return err
	}
	if _, err := acpc.ReadSessionSpec(t); err != nil {
		return a.fail(1, err)
	}
	if cmd.Bool("queued") {
		if n := acpc.ClearQueue(t.Dir); n > 0 {
			sayf(a.stderr, "acpc: dropped %d queued prompt(s)\n", n)
		}
	}
	// Queued prompts wait for the client lock. cancel takes a place at the
	// front of the queue first, so none of them starts a turn between the
	// interrupted client letting go and cancel connecting.
	front, err := acpc.EnqueueFront(t.Dir)
	if err != nil {
		return a.fail(1, err)
	}
	defer front.Done()
	// A client using the session owns the connection: a second one would
	// replace it. Interrupting it makes a prompt cancel its turn and wait
	// for the agent to end it, and an attach let go.
	interrupted := false
	if pid, held := acpc.LockHolder(t.Dir); held {
		if err := acpc.InterruptClient(t, pid); err != nil {
			return a.fail(1, err)
		}
		<-acpc.LockReleased(t.Dir)
		interrupted = true
	}
	s, err := acpc.OpenExisting(t, a.options(nil, acpc.DenyAll))
	if err != nil {
		return a.fail(1, err)
	}
	defer s.Close()
	busy := false
	if acphost.Alive(t.Dir) {
		if err := s.Connect(); err != nil {
			return exit(a.report(t.Dir, err))
		}
		busy = s.Busy()
	}
	if !busy {
		if !interrupted {
			say(a.stderr, "acpc: no turn is running")
		}
		return nil
	}
	if err := s.CancelTurn(); err != nil {
		return exit(a.report(t.Dir, err))
	}
	if _, err := s.AwaitTurn(); err != nil {
		return exit(a.report(t.Dir, err))
	}
	return nil
}

// listed is one session in ls.
type listed struct {
	Name      string     `json:"name"`
	Cwd       string     `json:"cwd"`
	Dir       string     `json:"dir"`
	State     string     `json:"state"` // running or stopped: the host
	HostPid   int        `json:"host_pid,omitempty"`
	ClientPid int        `json:"client_pid,omitempty"` // a prompt or attach using it
	Command   []string   `json:"command"`
	Last      *time.Time `json:"last,omitempty"` // newest journal frame
}

func (a *app) cmdLs(_ context.Context, cmd *cli.Command) error {
	format := cmd.String("format")
	if format != formatText && format != formatJSON {
		return fmt.Errorf("--format %q: want text or json", format)
	}
	root, err := acpc.SessionsRoot()
	if err != nil {
		return a.fail(1, err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return a.fail(1, err)
	}
	keys := []string{acpc.CwdKey(cwd)}
	all := cmd.Bool("all")
	if all {
		keys = dirNames(root)
	}
	list := []listed{}
	for _, key := range keys {
		for _, name := range dirNames(filepath.Join(root, key)) {
			dir := filepath.Join(root, key, name)
			spec, err := acphost.ReadSpec(dir)
			if err != nil {
				continue
			}
			l := listed{Name: name, Cwd: spec.Dir, Dir: dir, State: "stopped", Command: append([]string{spec.Command}, spec.Args...)}
			if d, err := proc.Open(dir); err == nil && d.Running() {
				l.State, l.HostPid = "running", d.Pid
			}
			l.ClientPid, _ = acpc.LockHolder(dir)
			if seq, at := acphost.JournalTail(dir); seq > 0 {
				l.Last = &at
			}
			list = append(list, l)
		}
	}
	if format == formatJSON {
		writeJSON(a.stdout, list)
		return nil
	}
	tw := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
	head := "NAME\tHOST\tCLIENT\tLAST\tCOMMAND"
	if all {
		head += "\tCWD"
	}
	sayf(tw, "%s\n", head)
	for _, l := range list {
		client, last := "-", "-"
		if l.ClientPid > 0 {
			client = fmt.Sprint(l.ClientPid)
		}
		if l.Last != nil {
			last = l.Last.Local().Format("2006-01-02 15:04")
		}
		sayf(tw, "%s\t%s\t%s\t%s\t%s", l.Name, l.State, client, last, strings.Join(l.Command, " "))
		if all {
			sayf(tw, "\t%s", l.Cwd)
		}
		sayf(tw, "\n")
	}
	_ = tw.Flush()
	return nil
}

// dirNames lists the directories in dir by name. A missing dir has none.
func dirNames(dir string) []string {
	ents, _ := os.ReadDir(dir)
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

func (a *app) cmdRm(_ context.Context, cmd *cli.Command) error {
	name := cmd.String("session")
	if name == "" || cmd.NArg() > 0 || len(a.rest) > 0 {
		return errors.New("rm needs -s NAME and no arguments")
	}
	t, err := acpc.Resolve(name, "", "")
	if err != nil {
		return err
	}
	if _, err := os.Stat(t.Dir); err != nil {
		return a.fail(1, fmt.Errorf("no %s", t))
	}
	if err := acpc.Remove(t); err != nil {
		return a.fail(1, err)
	}
	return nil
}
