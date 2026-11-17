package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gfx-labs/acpc/pkg/acpc"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/gfx-labs/acphost/pkg/acphost"
	acp "github.com/gfx-labs/go-acp"
	"github.com/urfave/cli/v3"
	"go.yaml.in/yaml/v3"
	"golang.org/x/sys/unix"
)

// acpc a2a serves an ACP agent over A2A: an A2A context is an acpc
// session, kept in the sessions root of --cwd, and an A2A task is one turn
// in it. It listens only on loopback or a unix socket; a proxy in front
// adds authentication, and the card file says how to reach it.

func (a *app) cmdA2A(ctx context.Context, cmd *cli.Command) error {
	argv := a.argv(cmd)
	if len(argv) == 0 {
		return errors.New("a2a: no agent command (give it after --)")
	}
	policy, err := policyFrom(cmd)
	if err != nil {
		return err
	}
	ask := cmd.Bool("ask")
	if ask && (cmd.Bool(acpc.ApproveAll) || cmd.Bool(acpc.ApproveReads) || cmd.Bool(acpc.DenyAll)) {
		return errors.New("give --ask or one of --approve-all, --approve-reads and --deny-all")
	}
	iso := isolationFrom(cmd)
	if iso.config == "-" {
		return errors.New("a2a: -c - is not supported: the server reads no stdin")
	}
	c, err := iso.load(nil)
	if err != nil {
		return err
	}
	cwd, err := filepath.Abs(cmd.String("cwd"))
	if err != nil {
		return err
	}
	root, err := acpc.SessionsRoot()
	if err != nil {
		return a.fail(1, err)
	}
	card, err := readCard(cmd.String("card"))
	if err != nil {
		return err
	}
	ln, err := listenLoopback(cmd.String("listen"))
	if err != nil {
		return a.fail(1, err)
	}
	defer func() { _ = ln.Close() }()
	if err := fillCard(card, ln.Addr(), argv); err != nil {
		return a.fail(1, err)
	}
	enforcer, err := a2asrv.NewAgentCardEnforcer(card)
	if err != nil {
		return a.fail(1, err)
	}
	b := &bridge{
		app: a, config: c, cwd: cwd, dir: filepath.Join(root, acpc.CwdKey(cwd)),
		lease: cmd.Duration("lease"), argv: argv, policy: policy, ask: ask,
		turns: map[a2a.TaskID]*turn{},
	}
	mux := http.NewServeMux()
	mux.Handle(a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(card))
	mux.Handle("/", a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(b, a2asrv.WithCallInterceptors(enforcer))))
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: time.Minute}
	ctx, stop := signal.NotifyContext(ctx, unix.SIGINT, unix.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()
	sayf(a.stderr, "acpc: serving A2A on %s %s, sessions in %s\n", ln.Addr().Network(), ln.Addr(), cwd)
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return a.fail(1, err)
	}
	return nil
}

// listenLoopback listens on addr, which has to be a loopback TCP address
// or unix:PATH.
func listenLoopback(addr string) (net.Listener, error) {
	if path, ok := strings.CutPrefix(addr, "unix:"); ok {
		return net.Listen("unix", path)
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("--listen %q: %w", addr, err)
	}
	refused := fmt.Errorf("--listen %q: acpc a2a listens only on loopback or unix:PATH; put a proxy in front for other addresses", addr)
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, refused
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	if ta, ok := ln.Addr().(*net.TCPAddr); !ok || !ta.IP.IsLoopback() {
		_ = ln.Close()
		return nil, refused
	}
	return ln, nil
}

// readCard reads the agent card file: an A2A AgentCard in YAML or JSON
// whose empty fields fillCard fills. No file is an empty card.
func readCard(path string) (*a2a.AgentCard, error) {
	card := &a2a.AgentCard{}
	if path == "" {
		return card, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var v any
	if err := yaml.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	j, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	d := json.NewDecoder(bytes.NewReader(j))
	d.DisallowUnknownFields()
	if err := d.Decode(card); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return card, nil
}

// fillCard completes the card for the agent argv served at addr. A server
// on a unix socket has no URL to offer, so its card has to give one.
func fillCard(card *a2a.AgentCard, addr net.Addr, argv []string) error {
	if len(card.SupportedInterfaces) == 0 {
		if addr.Network() != "tcp" {
			return errors.New("a server on a unix socket needs supportedInterfaces in its --card file: the URL clients reach it at")
		}
		card.SupportedInterfaces = []*a2a.AgentInterface{a2a.NewAgentInterface("http://"+addr.String()+"/", a2a.TransportProtocolJSONRPC)}
	}
	if card.Name == "" {
		card.Name = filepath.Base(argv[0])
	}
	if card.Description == "" {
		card.Description = "The ACP agent " + strings.Join(argv, " ") + ", served by acpc"
	}
	if len(card.DefaultInputModes) == 0 {
		card.DefaultInputModes = []string{"text/plain"}
	}
	if len(card.DefaultOutputModes) == 0 {
		card.DefaultOutputModes = []string{"text/plain"}
	}
	if card.Skills == nil {
		card.Skills = []a2a.AgentSkill{}
	}
	card.Capabilities.Streaming = true
	return nil
}

// bridge is the A2A agent executor: each task is a turn in the acpc
// session of its context.
type bridge struct {
	app    *app
	config acpc.Config
	cwd    string
	dir    string // sessions of cwd
	lease  time.Duration
	argv   []string
	policy string
	ask    bool

	mu    sync.Mutex
	turns map[a2a.TaskID]*turn
}

// turn is one task's prompt. Its goroutine owns the session and sends
// what the agent does as events until the turn ends, when it closes
// events. Execute yields them; one that stops at input-required leaves the
// turn waiting for the reply, which the next Execute for the task brings.
type turn struct {
	info       a2a.TaskInfo
	events     chan a2a.Event
	answers    chan *a2a.Message
	interrupts chan os.Signal
	stopAsk    chan struct{}
	stopOnce   sync.Once
	paused     bool // at input-required with no Execute yielding events; guarded by bridge.mu
}

func (t *turn) stop() {
	select {
	case t.interrupts <- unix.SIGINT:
	default:
	}
	t.stopOnce.Do(func() { close(t.stopAsk) })
}

// Execute implements a2asrv.AgentExecutor.
func (b *bridge) Execute(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if ec.StoredTask != nil {
			b.resume(ec, yield)
			return
		}
		in, err := promptInput(ec.Message)
		if err != nil {
			yield(nil, err)
			return
		}
		if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
			return
		}
		t := &turn{
			info:       ec.TaskInfo(),
			events:     make(chan a2a.Event),
			answers:    make(chan *a2a.Message, 1),
			interrupts: make(chan os.Signal, 2),
			stopAsk:    make(chan struct{}),
		}
		b.mu.Lock()
		b.turns[t.info.TaskID] = t
		b.mu.Unlock()
		go b.run(t, in)
		if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
			b.abandon(t)
			return
		}
		b.relay(t, yield)
	}
}

// resume passes the reply to a turn waiting at input-required and yields
// the rest of the turn.
func (b *bridge) resume(ec *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
	b.mu.Lock()
	t := b.turns[ec.TaskID]
	waiting := t != nil && t.paused
	if waiting {
		t.paused = false
	}
	b.mu.Unlock()
	if !waiting {
		yield(nil, fmt.Errorf("task %s is not waiting for input: %w", ec.TaskID, a2a.ErrInvalidParams))
		return
	}
	t.answers <- ec.Message
	if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
		b.abandon(t)
		return
	}
	b.relay(t, yield)
}

// relay yields the turn's events until it ends or waits for input.
func (b *bridge) relay(t *turn, yield func(a2a.Event, error) bool) {
	for ev := range t.events {
		if !yield(ev, nil) {
			b.abandon(t)
			return
		}
		if su, ok := ev.(*a2a.TaskStatusUpdateEvent); ok && su.Status.State == a2a.TaskStateInputRequired {
			b.mu.Lock()
			t.paused = true
			b.mu.Unlock()
			return
		}
	}
	b.forget(t)
}

// abandon cancels a turn whose events nobody takes any more and drains
// them until it ends.
func (b *bridge) abandon(t *turn) {
	t.stop()
	go func() {
		for range t.events {
		}
		b.forget(t)
	}()
}

func (b *bridge) forget(t *turn) {
	b.mu.Lock()
	delete(b.turns, t.info.TaskID)
	b.mu.Unlock()
}

// Cancel implements a2asrv.AgentExecutor. A running turn is cancelled
// through the agent and its Execute reports the end. A turn waiting for
// input has no Execute, so Cancel waits for it to end and reports it.
func (b *bridge) Cancel(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		b.mu.Lock()
		t := b.turns[ec.TaskID]
		paused := t != nil && t.paused
		if paused {
			t.paused = false
		}
		b.mu.Unlock()
		if t == nil {
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, nil), nil)
			return
		}
		t.stop()
		if !paused {
			return
		}
		for range t.events {
		}
		b.forget(t)
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, nil), nil)
	}
}

// run runs the turn and ends it with its final status.
func (b *bridge) run(t *turn, in acpc.PromptInput) {
	defer close(t.events)
	out := &a2aOutput{t: t}
	stop, err := b.prompt(t, out, in)
	t.events <- finalStatus(t.info, stop, err)
}

// prompt sends in to the session of the turn's context, creating the
// session for a new context. It waits behind other prompts for the
// session, from this server or from acpc prompt.
func (b *bridge) prompt(t *turn, out *a2aOutput, in acpc.PromptInput) (acp.StopReason, error) {
	tg := b.target(t.info.ContextID)
	if err := os.MkdirAll(tg.Dir, 0o700); err != nil {
		return "", err
	}
	e, err := acpc.Enqueue(tg.Dir, acpc.Queued{Input: in, Policy: b.policy})
	if err != nil {
		return "", err
	}
	defer e.Done()
	opts := b.app.options(nil, b.policy)
	opts.Output = out
	opts.Interrupts = t.interrupts
	if b.ask {
		opts.Ask = t.ask
	}
	spec := specFor(b.config, hostName(tg), b.cwd, b.lease, b.argv)
	s, err := acpc.OpenQueued(tg, spec, opts, e)
	if err != nil {
		return "", err
	}
	defer s.Close()
	if _, statErr := os.Stat(filepath.Join(tg.Dir, acphost.SpecFile)); statErr != nil {
		err = s.Create()
	} else {
		err = s.Reach(acpc.Queued{Config: &b.config})
	}
	if err != nil {
		return "", err
	}
	if err := s.AwaitRunning(); err != nil {
		return "", err
	}
	out.live = true
	return s.Prompt(in)
}

// target is the session of an A2A context: a name derived from the
// context id, which the client chooses, so that it is a safe path element.
func (b *bridge) target(contextID string) acpc.Target {
	sum := sha256.Sum256([]byte(contextID))
	name := "a2a-" + hex.EncodeToString(sum[:8])
	return acpc.Target{Name: name, Dir: filepath.Join(b.dir, name)}
}

// ask puts a permission request to the A2A client: the task waits at
// input-required until a reply names one of the options.
func (t *turn) ask(p acpc.Permission) (string, bool) {
	text := "Permission requested: " + p.Title
	if p.Kind != "" {
		text += " (" + p.Kind + ")"
	}
	text += ". Reply with one of these options:"
	for _, o := range p.Options {
		text += fmt.Sprintf("\n%s: %s (%s)", o.ID, o.Name, o.Kind)
	}
	for {
		select {
		case t.events <- a2a.NewStatusUpdateEvent(t.info, a2a.TaskStateInputRequired, a2a.NewMessageForTask(a2a.MessageRoleAgent, t.info, a2a.NewTextPart(text))):
		case <-t.stopAsk:
			return "", false
		}
		select {
		case m := <-t.answers:
			reply := strings.TrimSpace(messageText(m))
			for _, o := range p.Options {
				if reply == o.ID || strings.EqualFold(reply, o.Name) {
					return o.ID, true
				}
			}
			text = fmt.Sprintf("%q is not one of the options. Reply with one of: %s", reply, optionIDs(p.Options))
		case <-t.stopAsk:
			return "", false
		}
	}
}

func optionIDs(opts []acpc.PermissionOption) string {
	ids := make([]string, len(opts))
	for i, o := range opts {
		ids[i] = o.ID
	}
	return strings.Join(ids, ", ")
}

// a2aOutput turns the journal frames of a turn into A2A events: the
// agent's text as one streamed artifact, and each tool call's title, as it
// changes, as a working status. Frames before the turn, shown while the
// session catches up, are left out.
type a2aOutput struct {
	t        *turn
	live     bool
	artifact a2a.ArtifactID
	titles   map[string]string // the last title shown for each tool call
}

func (o *a2aOutput) Frame(fr acphost.Frame) {
	if !o.live {
		return
	}
	if text := agentText(fr); text != "" {
		var ev *a2a.TaskArtifactUpdateEvent
		if o.artifact == "" {
			ev = a2a.NewArtifactEvent(o.t.info, a2a.NewTextPart(text))
			o.artifact = ev.Artifact.ID
		} else {
			ev = a2a.NewArtifactUpdateEvent(o.t.info, o.artifact, a2a.NewTextPart(text))
		}
		o.t.events <- ev
		return
	}
	if id, title := toolCallTitle(fr); title != "" && o.titles[id] != title {
		if o.titles == nil {
			o.titles = map[string]string{}
		}
		o.titles[id] = title
		o.t.events <- a2a.NewStatusUpdateEvent(o.t.info, a2a.TaskStateWorking, a2a.NewMessageForTask(a2a.MessageRoleAgent, o.t.info, a2a.NewTextPart(title)))
	}
}

func (o *a2aOutput) End() {}

// toolCallTitle is the id and title of a tool call the agent started or
// retitled, or "" for another frame.
func toolCallTitle(fr acphost.Frame) (id, title string) {
	var m struct {
		Method string `json:"method"`
		Params struct {
			Update struct {
				Kind  string `json:"sessionUpdate"`
				ID    string `json:"toolCallId"`
				Title string `json:"title"`
			} `json:"update"`
		} `json:"params"`
	}
	if fr.From != "agent" || json.Unmarshal(fr.Msg, &m) != nil || m.Method != "session/update" {
		return "", ""
	}
	if u := m.Params.Update; u.Kind == "tool_call" || u.Kind == "tool_call_update" {
		return u.ID, u.Title
	}
	return "", ""
}

// finalStatus is the status that ends a task whose turn stopped with stop
// or failed with err.
func finalStatus(info a2a.TaskInfo, stop acp.StopReason, err error) *a2a.TaskStatusUpdateEvent {
	say := func(text string) *a2a.Message {
		return a2a.NewMessageForTask(a2a.MessageRoleAgent, info, a2a.NewTextPart(text))
	}
	switch {
	case errors.Is(err, acpc.ErrInterrupted) || errors.Is(err, acpc.ErrDropped):
		return a2a.NewStatusUpdateEvent(info, a2a.TaskStateCanceled, nil)
	case err != nil:
		return a2a.NewStatusUpdateEvent(info, a2a.TaskStateFailed, say(err.Error()))
	case stop == acp.StopReasonEndTurn:
		return a2a.NewStatusUpdateEvent(info, a2a.TaskStateCompleted, nil)
	case stop == acp.StopReasonCancelled:
		return a2a.NewStatusUpdateEvent(info, a2a.TaskStateCanceled, nil)
	case stop == acp.StopReasonRefusal:
		return a2a.NewStatusUpdateEvent(info, a2a.TaskStateRejected, say("the agent refused the prompt"))
	}
	return a2a.NewStatusUpdateEvent(info, a2a.TaskStateFailed, say("the turn stopped: "+string(stop)))
}

// promptInput is the prompt for an A2A message: its text, data and URL
// parts as text, and its raw parts as files.
func promptInput(m *a2a.Message) (acpc.PromptInput, error) {
	var in acpc.PromptInput
	var text []string
	for i, p := range m.Parts {
		switch c := p.Content.(type) {
		case a2a.Text:
			text = append(text, string(c))
		case a2a.URL:
			text = append(text, string(c))
		case a2a.Data:
			b, err := json.Marshal(c.Value)
			if err != nil {
				return in, fmt.Errorf("part %d: %w", i, err)
			}
			text = append(text, string(b))
		case a2a.Raw:
			name := p.Filename
			if name == "" {
				name = fmt.Sprintf("part-%d", i)
			}
			mt := p.MediaType
			if mt == "" {
				mt = http.DetectContentType(c)
			}
			in.Files = append(in.Files, acpc.Attachment{Path: "/" + filepath.Base(name), MimeType: mt, Data: c})
		}
	}
	in.Text = strings.Join(text, "\n")
	if strings.TrimSpace(in.Text) == "" {
		return in, fmt.Errorf("the message has no text: %w", a2a.ErrInvalidParams)
	}
	return in, nil
}

// messageText is the text parts of m.
func messageText(m *a2a.Message) string {
	var b strings.Builder
	for _, p := range m.Parts {
		b.WriteString(p.Text())
	}
	return b.String()
}
