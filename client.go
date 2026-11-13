package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"

	acp "github.com/gfx-labs/go-acp"

	"github.com/gfx-labs/acphost/pkg/acphost"
)

// The ACP client side of a session, over the host's socket, with go-acp
// for the protocol. The host forwards everything to the agent except its
// own extension methods. What the agent did is shown from the journal,
// not from the socket: a journal frame has a seq, so the client knows
// exactly what it has shown. A message on the socket only says that the
// journal has grown.

// errInterrupted is a signal that ends the command: one outside a turn,
// or a second one while a turn is being cancelled.
var errInterrupted = errors.New("interrupted")

// Permission policies.
const (
	approveAll   = "approve-all"
	approveReads = "approve-reads"
	denyAll      = "deny-all"
)

// link is a connection to a session's host.
type link struct {
	nc   net.Conn
	conn *acp.ClientSideConnection
	// activity has a value when the host sent something since the last
	// look, so the journal has grown.
	activity chan struct{}
	// turnEnd gets the host's turn_end notification: the end of a turn
	// another connection started.
	turnEnd chan acphost.TurnEnd
	served  chan struct{} // closed when Serve has returned
}

// dial connects to the session's host and serves the agent's messages
// until the connection ends.
func (s *session) dial() (*link, acphost.Welcome, error) {
	nc, rd, w, err := acphost.Connect(context.Background(), s.t.dir, acphost.WithMethodPrefix(s.spec.MethodPrefix))
	if err != nil {
		return nil, w, err
	}
	l := &link{
		nc: nc, activity: make(chan struct{}, 1),
		turnEnd: make(chan acphost.TurnEnd, 1), served: make(chan struct{}),
	}
	l.conn = acp.NewClientSideConnection(&agentRequests{s: s, l: l}, nc, rd, nil)
	end := acphost.MethodName(w.MethodPrefix, "turn_end")
	if err := l.conn.RegisterExtensionNotification(end, func(_ context.Context, _ string, params jsontext.Value) (any, *acp.RequestError) {
		var te acphost.TurnEnd
		_ = json.Unmarshal(params, &te)
		select {
		case l.turnEnd <- te:
		default:
		}
		return nil, nil
	}); err != nil {
		_ = nc.Close()
		return nil, w, err
	}
	go func() {
		defer close(l.served)
		_ = l.conn.Serve(context.Background())
	}()
	return l, w, nil
}

// grew notes that the journal has grown.
func (l *link) grew() {
	select {
	case l.activity <- struct{}{}:
	default:
	}
}

func (l *link) close() {
	_ = l.nc.Close()
	<-l.served
}

// agentRequests answers what the agent asks of the client. Only permission
// requests are served; the client offers no file, terminal or elicitation
// capability, so the agent does that work itself, inside its sandbox.
type agentRequests struct {
	s *session
	l *link
}

func (r *agentRequests) SessionUpdate(context.Context, acp.SessionNotification) error {
	r.l.grew()
	return nil
}

func (r *agentRequests) RequestPermission(_ context.Context, p acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	r.l.grew()
	return acp.RequestPermissionResponse{Outcome: r.s.permit(p)}, nil
}

func (r *agentRequests) CompleteElicitation(context.Context, acp.CompleteElicitationNotification) error {
	return nil
}

func (r *agentRequests) CreateElicitation(context.Context, acp.CreateElicitationRequest) (acp.CreateElicitationResponse, error) {
	return acp.CreateElicitationResponse{}, acp.NewMethodNotFound(acp.ClientMethodElicitationCreate)
}

func (r *agentRequests) ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, acp.NewMethodNotFound(acp.ClientMethodFsReadTextFile)
}

func (r *agentRequests) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, acp.NewMethodNotFound(acp.ClientMethodFsWriteTextFile)
}

func (r *agentRequests) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalCreate)
}

func (r *agentRequests) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalKill)
}

func (r *agentRequests) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalOutput)
}

func (r *agentRequests) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalRelease)
}

func (r *agentRequests) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalWaitForExit)
}

// tracef writes a step of the ACP handshake when the session traces them.
// Steps are written as they happen, before the frames they cause.
func (s *session) tracef(format string, args ...any) {
	if s.trace && s.out != nil {
		sayf(s.out.w, format+"\n", args...)
	}
}

func (s *session) journal() string { return filepath.Join(s.t.dir, acphost.JournalFile) }

// catchUp shows the journal frames after the last one this client showed
// and leaves the cursor at the journal's end. Without an output the
// journal is left unread, so a later attach still shows it.
func (s *session) catchUp() {
	if s.out == nil {
		return
	}
	frames, off, err := acphost.ReadJournal(s.journal(), s.state.Seq)
	if err != nil {
		return // no journal yet
	}
	s.off = off
	s.render(frames)
}

// show renders what the journal gained since the last call.
func (s *session) show() {
	if s.out == nil {
		return
	}
	frames, off, err := acphost.ReadFrom(s.journal(), s.off)
	if err != nil {
		return
	}
	s.off = off
	s.render(frames)
}

func (s *session) render(frames []acphost.Frame) {
	for _, fr := range frames {
		if fr.Seq <= s.state.Seq {
			continue
		}
		if !s.muted {
			s.out.frame(fr)
		}
		s.state.Seq = fr.Seq
	}
}

// wait runs fn, which talks to the agent, and shows the journal as the
// host's messages arrive until fn returns or done has a value. A signal
// cancels the running turn.
func (s *session) wait(fn func(context.Context) error, done <-chan struct{}) error {
	result := make(chan error, 1)
	if fn != nil {
		go func() { result <- fn(context.Background()) }()
	}
	served := s.link.served
	for {
		select {
		case err := <-result:
			// The host journals a frame before it sends it.
			s.show()
			if errors.Is(err, acp.ErrClosed) {
				return s.lost()
			}
			return err
		case <-done:
			s.show()
			return nil
		case <-s.link.activity:
			s.show()
		case <-served:
			if fn == nil {
				s.show()
				return s.lost()
			}
			// fn returns acp.ErrClosed, or a response read just before.
			served = nil
		case <-s.sigs:
			if err := s.interrupt(); err != nil {
				return err
			}
		}
	}
}

// hostGone is a host that stopped while this client was connected.
type hostGone struct{ exit acphost.Exit }

func (e *hostGone) Error() string {
	s := "the host stopped: " + e.exit.Reason
	switch {
	case e.exit.Error != "":
		s += ": " + e.exit.Error
	case e.exit.Signal != "":
		s += " (agent killed by " + e.exit.Signal + ")"
	case e.exit.Reason == "agent exited":
		s += fmt.Sprintf(" (status %d)", e.exit.ExitCode)
	}
	return s
}

// lost explains a closed connection. The host removes exit.json when it
// starts and writes it before it closes its connections, so one that is
// there is this host's.
func (s *session) lost() error {
	if ex, ok := acphost.ReadExit(s.t.dir); ok {
		return &hostGone{ex}
	}
	return errors.New("the host closed the connection (another client connected to the session)")
}

// interrupt cancels the running turn on the first signal. The agent then
// ends the turn itself, so the command keeps waiting for that.
func (s *session) interrupt() error {
	s.interrupts++
	if !s.inTurn || s.interrupts > 1 {
		return errInterrupted
	}
	say(s.a.stderr, "acpc: cancelling the turn (interrupt again to stop waiting)")
	return s.cancelTurn()
}

func (s *session) cancelTurn() error {
	// From here on a permission request is answered as cancelled, as ACP asks.
	s.cancelling.Store(true)
	return s.link.conn.Cancel(context.Background(), acp.CancelNotification{SessionId: acp.SessionId(s.state.ACPSession)})
}

// permit picks the option the policy allows: an allow or a reject option,
// the one-time kind first so nothing outlasts the request.
func (s *session) permit(p acp.RequestPermissionRequest) acp.RequestPermissionOutcome {
	if s.cancelling.Load() {
		return acp.NewRequestPermissionOutcomeCancelled()
	}
	var kind acp.ToolKind
	if p.ToolCall.Kind != nil {
		kind = *p.ToolCall.Kind
	}
	allow := s.policy == approveAll || (s.policy == approveReads && (kind == acp.ToolKindRead || kind == acp.ToolKindSearch))
	want := []acp.PermissionOptionKind{acp.PermissionOptionKindRejectOnce, acp.PermissionOptionKindRejectAlways}
	if allow {
		want = []acp.PermissionOptionKind{acp.PermissionOptionKindAllowOnce, acp.PermissionOptionKindAllowAlways}
	} else {
		what := "tool call " + string(p.ToolCall.ToolCallId)
		if p.ToolCall.Title != nil && *p.ToolCall.Title != "" {
			what = *p.ToolCall.Title
		}
		shown := string(kind)
		if shown == "" {
			shown = "kind not given"
		}
		sayf(s.a.stderr, "acpc: refused permission: %s (%s); --approve-all allows it\n", what, shown)
	}
	for _, k := range want {
		for _, o := range p.Options {
			if o.Kind == k {
				return acp.NewRequestPermissionOutcomeSelected(o.OptionId)
			}
		}
	}
	return acp.NewRequestPermissionOutcomeCancelled()
}

// ensure makes the agent ready for a prompt: initialized, and with the ACP
// session open in this agent process. A host started since the session
// was last opened runs a new agent, which has to load the session again.
// If it cannot, the conversation starts over and ensure returns a summary
// of the journal to put in front of the next prompt.
func (s *session) ensure() (seed string, err error) {
	// What the agent sends here (a load replays the whole conversation)
	// is not new to the user.
	s.muted = true
	defer func() {
		s.show()
		s.muted = false
	}()
	var init acp.InitializeResponse
	if len(s.welcome.Initialize) > 0 {
		// The host replays the answer of the agent's one initialize.
		if err := jsonv2.Unmarshal(s.welcome.Initialize, &init); err != nil {
			return "", fmt.Errorf("initialize result from the host: %w", err)
		}
	} else {
		s.tracef("[client] initialize (running)")
		err := s.wait(func(ctx context.Context) error {
			var err error
			init, err = s.link.conn.Initialize(ctx, acp.InitializeRequest{
				ProtocolVersion: acp.ProtocolVersion(acp.ProtocolVersionNumber),
				ClientInfo:      &acp.Implementation{Name: "acpc", Version: version()},
			})
			return err
		}, nil)
		if err != nil {
			return "", fmt.Errorf("initialize: %w", err)
		}
		// The host only caches it for the next connection; this one asks
		// ensure again before the prompt.
		if b, err := jsonv2.Marshal(init); err == nil {
			s.welcome.Initialize = b
		}
		agent := acp.Implementation{}
		if init.AgentInfo != nil {
			agent = *init.AgentInfo
		}
		s.tracef("[client] initialized protocol version %d, agent %s %s, loadSession %t",
			init.ProtocolVersion, agent.Name, agent.Version, init.AgentCapabilities.LoadSession)
	}
	s.caps = init.AgentCapabilities.PromptCapabilities
	switch {
	case s.state.ACPSession == "":
		return "", s.newSession()
	case s.state.HostPid == s.welcome.Pid:
		return "", nil
	}
	if init.AgentCapabilities.LoadSession {
		s.tracef("[client] session/load %s (running)", s.state.ACPSession)
		err := s.wait(func(ctx context.Context) error {
			_, err := s.link.conn.LoadSession(ctx, acp.LoadSessionRequest{
				SessionId: acp.SessionId(s.state.ACPSession), Cwd: s.spec.Dir, McpServers: []acp.McpServer{},
			})
			return err
		}, nil)
		if err == nil {
			return "", s.opened(s.state.ACPSession)
		}
		var refused *acp.RequestError
		if !errors.As(err, &refused) {
			return "", err
		}
		say(s.a.stderr, "acpc: session/load:", err)
	}
	// As jobshop does: a failed load would fail again on every restart.
	frames, _, _ := acphost.ReadJournal(s.journal(), 0)
	seed = acphost.Summarize(frames, 0)
	say(s.a.stderr, "acpc: the agent could not reload the conversation; starting a new one from a summary of the journal")
	return seed, s.newSession()
}

func (s *session) newSession() error {
	s.tracef("[client] session/new cwd %s (running)", s.spec.Dir)
	var res acp.NewSessionResponse
	err := s.wait(func(ctx context.Context) error {
		var err error
		res, err = s.link.conn.NewSession(ctx, acp.NewSessionRequest{Cwd: s.spec.Dir, McpServers: []acp.McpServer{}})
		return err
	}, nil)
	if err != nil {
		return fmt.Errorf("session/new: %w", err)
	}
	if res.SessionId == "" {
		return errors.New("session/new: no sessionId in the result")
	}
	s.tracef("[client] session %s", res.SessionId)
	return s.opened(string(res.SessionId))
}

// opened records that the agent process behind this host has the ACP
// session id open. Saved now: a later failure must not lose it.
func (s *session) opened(id string) error {
	s.state.ACPSession, s.state.HostPid = id, s.welcome.Pid
	return writeState(s.t.dir, s.state)
}

// promptRequest is the session/prompt for in.
func (s *session) promptRequest(in promptInput) (acp.PromptRequest, error) {
	blocks, err := contentBlocks(in, s.caps)
	if err != nil {
		return acp.PromptRequest{}, err
	}
	return acp.PromptRequest{SessionId: acp.SessionId(s.state.ACPSession), Prompt: blocks}, nil
}

// awaitTurn waits for a turn another connection started. The host reports
// its end with the turn_end extension method. A signal ends the wait and
// leaves the turn running.
func (s *session) awaitTurn() (acp.StopReason, error) {
	ended := make(chan struct{})
	var te acphost.TurnEnd
	go func() {
		select {
		case te = <-s.link.turnEnd:
			close(ended)
		case <-s.link.served:
		}
	}()
	if err := s.wait(nil, ended); err != nil {
		return "", err
	}
	if len(te.Error) > 0 {
		var re acp.RequestError
		_ = json.Unmarshal(te.Error, &re)
		return "", fmt.Errorf("session/prompt: %w", &re)
	}
	var r struct {
		StopReason acp.StopReason `json:"stopReason"`
	}
	_ = json.Unmarshal(te.Result, &r)
	return r.StopReason, nil
}

// turnStatus is the exit status for a finished turn: 0 for end_turn, 130
// for a cancelled one, 1 for any other stop reason or an error.
func (s *session) turnStatus(stop acp.StopReason, err error) int {
	if err != nil {
		return s.report(err)
	}
	switch stop {
	case acp.StopReasonEndTurn:
		return 0
	case acp.StopReasonCancelled:
		return 130
	}
	say(s.a.stderr, "acpc: the turn stopped:", stop)
	return 1
}

// report prints err and returns the exit status for it. For a host that
// stopped it adds the end of the host's log, where the agent's stderr is.
func (s *session) report(err error) int {
	if errors.Is(err, errInterrupted) {
		return 130
	}
	say(s.a.stderr, "acpc:", err)
	var gone *hostGone
	if errors.As(err, &gone) {
		if tail := hostLogTail(s.t.dir); tail != "" {
			sayf(s.a.stderr, "acpc: end of %s:\n%s\n", filepath.Join(s.t.dir, acphost.LogFile), tail)
		}
		// bwrap reports a command it cannot run after the host is up.
		if gone.exit.Reason == acphost.ReasonSpawnFailed {
			return 125
		}
	}
	var spawn *spawnError
	if errors.As(err, &spawn) {
		return 125
	}
	return 1
}

// spawnError is a host that could not be started: exit 125.
type spawnError struct{ err error }

func (e *spawnError) Error() string { return e.err.Error() }
func (e *spawnError) Unwrap() error { return e.err }

// frameMethod is the JSON-RPC method of a journal frame.
func frameMethod(fr acphost.Frame) string {
	var m struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal(fr.Msg, &m)
	return m.Method
}

// hostLogTail is the last lines of the host's log.
func hostLogTail(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, acphost.LogFile))
	if err != nil {
		return ""
	}
	lines := bytes.Split(bytes.TrimSpace(b), []byte("\n"))
	if len(lines) > 15 {
		lines = lines[len(lines)-15:]
	}
	return string(bytes.Join(lines, []byte("\n")))
}
