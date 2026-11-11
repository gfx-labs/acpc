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

	"github.com/gfx-labs/acphost/pkg/acphost"
	"github.com/gfx-labs/acphost/pkg/proc"
	"github.com/urfave/cli/v3"
	"golang.org/x/sys/unix"
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
	// stopGrace bounds the wait for a host to stop before it is killed.
	stopGrace = 30 * time.Second
)

// output shows journal frames in one of the formats.
type output struct {
	w      io.Writer
	format string
	text   acphost.Renderer
	open   bool // quiet: the agent's text has not ended its line
}

func (o *output) frame(fr acphost.Frame) {
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

// end finishes the last line of quiet output.
func (o *output) end() {
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
	policy, n := approveReads, 0
	for _, p := range []string{approveAll, approveReads, denyAll} {
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
func hostName(t target) string {
	if t.name == "" {
		return filepath.Base(t.dir)
	}
	return filepath.Base(filepath.Dir(t.dir)) + "-" + t.name
}

// create writes the spec, starts the host and opens an ACP session.
func (s *session) create() int {
	if err := acphost.WriteSpec(s.t.dir, s.spec); err != nil {
		return s.report(err)
	}
	if err := s.start(); err != nil {
		return s.report(err)
	}
	if err := s.connect(); err != nil {
		return s.report(err)
	}
	s.catchUp()
	if _, err := s.ensure(); err != nil {
		return s.report(err)
	}
	return 0
}

// turn sends one prompt and returns the exit status once the turn ended.
func (s *session) turn(text string) int {
	text, err := s.ready(text)
	if err != nil {
		return s.report(err)
	}
	s.inTurn = true
	res, err := s.call("session/prompt", s.prompt(text))
	s.inTurn = false
	code := s.turnStatus(res, err)
	s.out.end()
	return code
}

// ready prepares the agent for a prompt and returns the text to send.
func (s *session) ready(text string) (string, error) {
	seed, err := s.ensure()
	if err != nil {
		return "", err
	}
	if seed != "" {
		// Summarize drops everything up to SummaryEnd, so a later
		// summary does not nest this one.
		text = seed + "\n\n" + acphost.SummaryEnd + "\n\n" + text
	}
	return text, nil
}

func (s *session) prompt(text string) promptParams {
	return promptParams{SessionID: s.state.ACPSession, Prompt: []contentText{{Type: "text", Text: text}}}
}

// submit sends one prompt and returns once the host has journaled it. A
// client that disconnected or was replaced sooner could lose the prompt:
// the host closes a replaced connection without reading what is left on it.
func (s *session) submit(text string) int {
	text, err := s.ready(text)
	if err != nil {
		return s.report(err)
	}
	if _, err := s.request("session/prompt", s.prompt(text)); err != nil {
		return s.report(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			select {
			case _, ok := <-s.link.in:
				if !ok {
					cancel()
					return
				}
			case <-s.sigs:
				cancel()
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	taken := false
	err = acphost.FollowJournal(ctx, s.journal(), s.off, func(fr acphost.Frame) {
		if fr.From == "controller" && frameMethod(fr) == "session/prompt" {
			taken = true
			cancel()
		}
	})
	if err != nil {
		return s.report(err)
	}
	if !taken {
		return s.report(errors.New("stopped before the host took the prompt"))
	}
	return 0
}

func (a *app) cmdNew(_ context.Context, cmd *cli.Command) error {
	argv := a.argv(cmd)
	if len(argv) == 0 {
		return errors.New("new: no agent command (give it after --)")
	}
	t, err := resolve(cmd.String("session"), cmd.String("dir"), defaultSession)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(t.dir, acphost.SpecFile)); err == nil {
		return a.fail(1, fmt.Errorf("%s already exists", t))
	}
	spec, err := a.sessionSpec(hostName(t), isolationFrom(cmd), cmd.Duration("lease"), argv)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(t.dir, 0o700); err != nil {
		return a.fail(1, err)
	}
	// Nothing of new is shown, but the journal is read: the first prompt
	// then starts after the handshake.
	s, err := a.use(t, spec, &output{w: io.Discard, format: formatQuiet}, denyAll)
	if err != nil {
		return a.fail(1, err)
	}
	code := s.create()
	s.close()
	if code != 0 {
		a.discard(t)
		return exit(code)
	}
	say(a.stdout, t.dir)
	return nil
}

// discard removes a session that could not be created.
func (a *app) discard(t target) {
	if err := stopHost(t.dir); err != nil {
		say(a.stderr, "acphost:", err)
	}
	if t.name == "" {
		// A directory the user named: only what makes it a session goes.
		for _, f := range []string{acphost.SpecFile, stateFile, lockFile} {
			_ = os.Remove(filepath.Join(t.dir, f))
		}
		return
	}
	removeNamed(t)
}

// removeNamed deletes a named session's directory, and its working
// directory's when that was the last session in it.
func removeNamed(t target) {
	_ = os.RemoveAll(t.dir)
	_ = os.Remove(filepath.Dir(t.dir))
}

func (a *app) cmdPrompt(_ context.Context, cmd *cli.Command) error {
	t, err := resolve(cmd.String("session"), cmd.String("dir"), defaultSession)
	if err != nil {
		return err
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
	var config *Config
	if iso.set() {
		c, err := iso.load(a.stdin)
		if err != nil {
			return err
		}
		config = &c
	}
	s, err := a.open(t, out, policy)
	if err != nil {
		return a.fail(1, err)
	}
	defer s.close()
	if config != nil {
		if err := s.reconfigure(*config); err != nil {
			return exit(s.report(err))
		}
	}
	if err := s.start(); err != nil {
		return exit(s.report(err))
	}
	if err := s.connect(); err != nil {
		return exit(s.report(err))
	}
	if s.welcome.Busy {
		return exit(s.report(fmt.Errorf("a turn is running in %s (attach waits for it, cancel stops it)", t)))
	}
	// What an earlier turn left unseen comes first.
	s.catchUp()
	if cmd.Bool("no-wait") {
		return exit(s.submit(text))
	}
	return exit(s.turn(text))
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
	dir, err := os.MkdirTemp("", "acphost-exec-")
	if err != nil {
		return a.fail(1, err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	t := target{dir: dir}
	spec, err := a.sessionSpec(hostName(t), iso, execLease, a.rest)
	if err != nil {
		return err
	}
	s, err := a.use(t, spec, out, policy)
	if err != nil {
		return a.fail(1, err)
	}
	code := s.create()
	if code == 0 {
		code = s.turn(text)
	}
	s.close()
	if err := stopHost(dir); err != nil {
		say(a.stderr, "acphost:", err)
	}
	return exit(code)
}

func (a *app) cmdAttach(_ context.Context, cmd *cli.Command) error {
	if cmd.NArg() > 0 || len(a.rest) > 0 {
		return errors.New("attach takes no arguments")
	}
	t, err := resolve(cmd.String("session"), cmd.String("dir"), defaultSession)
	if err != nil {
		return err
	}
	out, policy, err := turnOptions(cmd, a.stdout)
	if err != nil {
		return err
	}
	s, err := a.open(t, out, policy)
	if err != nil {
		return a.fail(1, err)
	}
	defer s.close()
	defer out.end()
	if !acphost.Alive(t.dir) {
		s.catchUp()
		say(a.stderr, "acphost: the host is not running (the next prompt starts it)")
		return nil
	}
	if err := s.connect(); err != nil {
		return exit(s.report(err))
	}
	s.catchUp()
	if !s.welcome.Busy {
		return nil
	}
	// A signal here ends attach, not the turn: cancel does that.
	return exit(s.turnStatus(s.awaitTurn()))
}

func (a *app) cmdCancel(_ context.Context, cmd *cli.Command) error {
	if cmd.NArg() > 0 || len(a.rest) > 0 {
		return errors.New("cancel takes no arguments")
	}
	t, err := resolve(cmd.String("session"), cmd.String("dir"), defaultSession)
	if err != nil {
		return err
	}
	if _, err := readSessionSpec(t); err != nil {
		return a.fail(1, err)
	}
	// A client using the session owns the connection: a second one would
	// replace it. Interrupting it makes a prompt cancel its turn and wait
	// for the agent to end it, and an attach let go.
	interrupted := false
	if pid, held := lockHolder(t.dir); held {
		if err := interruptClient(t, pid); err != nil {
			return a.fail(1, err)
		}
		<-lockReleased(t.dir)
		interrupted = true
	}
	s, err := a.open(t, nil, denyAll)
	if err != nil {
		return a.fail(1, err)
	}
	defer s.close()
	busy := false
	if acphost.Alive(t.dir) {
		if err := s.connect(); err != nil {
			return exit(s.report(err))
		}
		busy = s.welcome.Busy
	}
	if !busy {
		if !interrupted {
			say(a.stderr, "acphost: no turn is running")
		}
		return nil
	}
	if err := s.cancelTurn(); err != nil {
		return exit(s.report(err))
	}
	if _, err := s.awaitTurn(); err != nil {
		return exit(s.report(err))
	}
	return nil
}

// interruptClient sends SIGINT to the client holding t's lock. The pid
// comes from a file, so it is only signalled if that process has the lock
// file open, and through a pidfd so the check and the signal are about the
// same process.
func interruptClient(t target, pid int) error {
	if pid <= 0 {
		return fmt.Errorf("%s is in use; try again", t)
	}
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return fmt.Errorf("%s is in use by pid %d: %w", t, pid, err)
	}
	defer func() { _ = unix.Close(fd) }()
	// /proc shows resolved paths.
	lock, err := filepath.EvalSymlinks(filepath.Join(t.dir, lockFile))
	if err != nil {
		return err
	}
	if !hasOpen(pid, lock) {
		return fmt.Errorf("%s is in use, but pid %d is not its client; try again", t, pid)
	}
	return unix.PidfdSendSignal(fd, unix.SIGINT, nil, 0)
}

// hasOpen reports whether process pid has path open.
func hasOpen(pid int, path string) bool {
	fds := fmt.Sprintf("/proc/%d/fd", pid)
	ents, err := os.ReadDir(fds)
	if err != nil {
		return false
	}
	for _, e := range ents {
		if p, err := os.Readlink(filepath.Join(fds, e.Name())); err == nil && p == path {
			return true
		}
	}
	return false
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
	root, err := sessionsRoot()
	if err != nil {
		return a.fail(1, err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return a.fail(1, err)
	}
	keys := []string{cwdKey(cwd)}
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
			l.ClientPid, _ = lockHolder(dir)
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
	t, err := resolve(name, "", "")
	if err != nil {
		return err
	}
	if _, err := os.Stat(t.dir); err != nil {
		return a.fail(1, fmt.Errorf("no %s", t))
	}
	unlock, err := lockClient(t)
	if err != nil {
		return a.fail(1, err)
	}
	defer unlock()
	if err := stopHost(t.dir); err != nil {
		return a.fail(1, err)
	}
	removeNamed(t)
	return nil
}
