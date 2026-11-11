package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"

	"github.com/gfx-labs/acphost/pkg/acphost"
)

// The ACP client side of a session: JSON-RPC over the host's socket. The
// host forwards everything to the agent except its own extension methods.
// What the agent did is shown from the journal, not from the socket: a
// journal frame has a seq, so the client knows exactly what it has shown.
// A message on the socket only says that the journal has grown.

// message is one JSON-RPC message.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError is a JSON-RPC error object.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	s := fmt.Sprintf("%s (code %d)", e.Message, e.Code)
	if len(e.Data) > 0 && string(e.Data) != "null" {
		s += ": " + string(e.Data)
	}
	return s
}

// errInterrupted is a signal that ends the command: one outside a turn,
// or a second one while a turn is being cancelled.
var errInterrupted = errors.New("interrupted")

// ACP message bodies, only the fields this client uses.
type (
	initializeParams struct {
		ProtocolVersion    int                `json:"protocolVersion"`
		ClientCapabilities clientCapabilities `json:"clientCapabilities"`
		ClientInfo         clientInfo         `json:"clientInfo"`
	}
	// clientCapabilities offers nothing: the agent reads and writes files
	// and runs commands itself, inside its sandbox.
	clientCapabilities struct {
		FS       fsCapabilities `json:"fs"`
		Terminal bool           `json:"terminal"`
	}
	fsCapabilities struct {
		ReadTextFile  bool `json:"readTextFile"`
		WriteTextFile bool `json:"writeTextFile"`
	}
	clientInfo struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	initializeResult struct {
		AgentCapabilities struct {
			LoadSession bool `json:"loadSession"`
		} `json:"agentCapabilities"`
	}
	sessionParams struct {
		SessionID  string   `json:"sessionId,omitempty"`
		Cwd        string   `json:"cwd"`
		McpServers []string `json:"mcpServers"`
	}
	sessionResult struct {
		SessionID string `json:"sessionId"`
	}
	promptParams struct {
		SessionID string        `json:"sessionId"`
		Prompt    []contentText `json:"prompt"`
	}
	contentText struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	cancelParams struct {
		SessionID string `json:"sessionId"`
	}
	promptResult struct {
		StopReason string `json:"stopReason"`
	}
	permissionParams struct {
		ToolCall struct {
			ToolCallID string `json:"toolCallId"`
			Title      string `json:"title"`
			Kind       string `json:"kind"`
		} `json:"toolCall"`
		Options []struct {
			OptionID string `json:"optionId"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	permissionResult struct {
		Outcome permissionOutcome `json:"outcome"`
	}
	permissionOutcome struct {
		Outcome  string `json:"outcome"` // selected or cancelled
		OptionID string `json:"optionId,omitempty"`
	}
)

// acpProtocolVersion is the ACP version this client speaks.
const acpProtocolVersion = 1

// Permission policies.
const (
	approveAll   = "approve-all"
	approveReads = "approve-reads"
	denyAll      = "deny-all"
)

// link is a connection to a session's host.
type link struct {
	nc     net.Conn
	in     chan []byte // lines from the host; closed when the connection ends
	done   chan struct{}
	nextID int
}

func dial(dir, prefix string) (*link, acphost.Welcome, error) {
	nc, rd, w, err := acphost.Connect(context.Background(), dir, acphost.WithMethodPrefix(prefix))
	if err != nil {
		return nil, w, err
	}
	l := &link{nc: nc, in: make(chan []byte, 64), done: make(chan struct{})}
	go l.read(rd)
	return l, w, nil
}

func (l *link) read(rd *bufio.Reader) {
	defer close(l.in)
	for {
		line, err := rd.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 {
			select {
			case l.in <- line:
			case <-l.done:
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (l *link) send(m message) error {
	m.JSONRPC = "2.0"
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = l.nc.Write(append(b, '\n'))
	return err
}

func (l *link) close() {
	close(l.done)
	_ = l.nc.Close()
}

func rawJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
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

// wait handles messages from the host until one satisfies until. It
// answers the agent's requests, and on a signal cancels the running turn.
func (s *session) wait(until func(message) bool) (message, error) {
	for {
		select {
		case line, ok := <-s.link.in:
			// The host journals a frame before it sends it.
			s.show()
			if !ok {
				return message{}, s.lost()
			}
			var m message
			if json.Unmarshal(line, &m) != nil {
				continue
			}
			if m.Method != "" && len(m.ID) > 0 {
				if err := s.answer(m); err != nil {
					return message{}, err
				}
				continue
			}
			if until(m) {
				return m, nil
			}
		case <-s.sigs:
			if err := s.interrupt(); err != nil {
				return message{}, err
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
	say(s.a.stderr, "acphost: cancelling the turn (interrupt again to stop waiting)")
	return s.cancelTurn()
}

func (s *session) cancelTurn() error {
	// From here on a permission request is answered as cancelled, as ACP asks.
	s.cancelling = true
	return s.link.send(message{Method: "session/cancel", Params: rawJSON(cancelParams{SessionID: s.state.ACPSession})})
}

// request sends a request and returns its id without waiting.
func (s *session) request(method string, params any) (json.RawMessage, error) {
	s.link.nextID++
	id := json.RawMessage(strconv.Itoa(s.link.nextID))
	return id, s.link.send(message{ID: id, Method: method, Params: rawJSON(params)})
}

// call sends a request and waits for its response.
func (s *session) call(method string, params any) (json.RawMessage, error) {
	id, err := s.request(method, params)
	if err != nil {
		return nil, err
	}
	m, err := s.wait(func(m message) bool { return m.Method == "" && bytes.Equal(m.ID, id) })
	if err != nil {
		return nil, err
	}
	if m.Error != nil {
		return nil, fmt.Errorf("%s: %w", method, m.Error)
	}
	return m.Result, nil
}

// answer replies to a request from the agent. Only permission requests
// are served; the client offers no file or terminal capability.
func (s *session) answer(m message) error {
	if m.Method != "session/request_permission" {
		return s.link.send(message{ID: m.ID, Error: &rpcError{Code: -32601, Message: "method not found: " + m.Method}})
	}
	var p permissionParams
	_ = json.Unmarshal(m.Params, &p)
	return s.link.send(message{ID: m.ID, Result: rawJSON(permissionResult{Outcome: s.permit(p)})})
}

// permit picks the option the policy allows: an allow or a reject option,
// the one-time kind first so nothing outlasts the request.
func (s *session) permit(p permissionParams) permissionOutcome {
	if s.cancelling {
		return permissionOutcome{Outcome: "cancelled"}
	}
	kind := p.ToolCall.Kind
	allow := s.policy == approveAll || (s.policy == approveReads && (kind == "read" || kind == "search"))
	want := []string{"reject_once", "reject_always"}
	if allow {
		want = []string{"allow_once", "allow_always"}
	} else {
		what := p.ToolCall.Title
		if what == "" {
			what = "tool call " + p.ToolCall.ToolCallID
		}
		if kind == "" {
			kind = "kind not given"
		}
		sayf(s.a.stderr, "acphost: refused permission: %s (%s); --approve-all allows it\n", what, kind)
	}
	for _, k := range want {
		for _, o := range p.Options {
			if o.Kind == k {
				return permissionOutcome{Outcome: "selected", OptionID: o.OptionID}
			}
		}
	}
	return permissionOutcome{Outcome: "cancelled"}
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
	init := s.welcome.Initialize
	if len(init) == 0 {
		init, err = s.call("initialize", initializeParams{
			ProtocolVersion: acpProtocolVersion,
			ClientInfo:      clientInfo{Name: "acphost", Version: version()},
		})
		if err != nil {
			return "", err
		}
	}
	var caps initializeResult
	_ = json.Unmarshal(init, &caps)
	switch {
	case s.state.ACPSession == "":
		return "", s.newSession()
	case s.state.HostPid == s.welcome.Pid:
		return "", nil
	}
	if caps.AgentCapabilities.LoadSession {
		_, err := s.call("session/load", sessionParams{SessionID: s.state.ACPSession, Cwd: s.spec.Dir, McpServers: []string{}})
		if err == nil {
			return "", s.opened(s.state.ACPSession)
		}
		var refused *rpcError
		if !errors.As(err, &refused) {
			return "", err
		}
		say(s.a.stderr, "acphost:", err)
	}
	// As jobshop does: a failed load would fail again on every restart.
	frames, _, _ := acphost.ReadJournal(s.journal(), 0)
	seed = acphost.Summarize(frames, 0)
	say(s.a.stderr, "acphost: the agent could not reload the conversation; starting a new one from a summary of the journal")
	return seed, s.newSession()
}

func (s *session) newSession() error {
	res, err := s.call("session/new", sessionParams{Cwd: s.spec.Dir, McpServers: []string{}})
	if err != nil {
		return err
	}
	var r sessionResult
	if err := json.Unmarshal(res, &r); err != nil || r.SessionID == "" {
		return fmt.Errorf("session/new: no sessionId in %s", res)
	}
	return s.opened(r.SessionID)
}

// opened records that the agent process behind this host has the ACP
// session id open. Saved now: a later failure must not lose it.
func (s *session) opened(id string) error {
	s.state.ACPSession, s.state.HostPid = id, s.welcome.Pid
	return writeState(s.t.dir, s.state)
}

// awaitTurn waits for a turn another connection started. The host reports
// its end with the turn_end extension method. A signal ends the wait and
// leaves the turn running.
func (s *session) awaitTurn() (json.RawMessage, error) {
	end := acphost.MethodName(s.spec.MethodPrefix, "turn_end")
	m, err := s.wait(func(m message) bool { return m.Method == end })
	if err != nil {
		return nil, err
	}
	var te acphost.TurnEnd
	_ = json.Unmarshal(m.Params, &te)
	if len(te.Error) > 0 {
		var re rpcError
		_ = json.Unmarshal(te.Error, &re)
		return nil, fmt.Errorf("session/prompt: %w", &re)
	}
	return te.Result, nil
}

// turnStatus is the exit status for a finished turn: 0 for end_turn, 130
// for a cancelled one, 1 for any other stop reason or an error.
func (s *session) turnStatus(res json.RawMessage, err error) int {
	if err != nil {
		return s.report(err)
	}
	var r promptResult
	_ = json.Unmarshal(res, &r)
	switch r.StopReason {
	case "end_turn":
		return 0
	case "cancelled":
		return 130
	}
	say(s.a.stderr, "acphost: the turn stopped:", r.StopReason)
	return 1
}

// report prints err and returns the exit status for it. For a host that
// stopped it adds the end of the host's log, where the agent's stderr is.
func (s *session) report(err error) int {
	if errors.Is(err, errInterrupted) {
		return 130
	}
	say(s.a.stderr, "acphost:", err)
	var gone *hostGone
	if errors.As(err, &gone) {
		if tail := hostLogTail(s.t.dir); tail != "" {
			sayf(s.a.stderr, "acphost: end of %s:\n%s\n", filepath.Join(s.t.dir, acphost.LogFile), tail)
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

// spawnError is a host that could not be started: exit 125, as start
// reports it.
type spawnError struct{ err error }

func (e *spawnError) Error() string { return e.err.Error() }
func (e *spawnError) Unwrap() error { return e.err }

// frameMethod is the JSON-RPC method of a journal frame.
func frameMethod(fr acphost.Frame) string {
	var m message
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
