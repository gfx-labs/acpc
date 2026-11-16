package acpc

import (
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"

	acp "github.com/gfx-labs/go-acp"
	acp2 "github.com/gfx-labs/go-acp/v2"

	"github.com/gfx-labs/acphost/pkg/acphost"
)

// ACP protocol versions a session can speak. Version 2 is the upstream
// draft: experimental, only used when asked for, and never chosen by
// default. See docs/notes/2026-11-16-acp-v2.md.
const (
	ProtocolV1 = 1
	ProtocolV2 = 2
)

// protocol is one ACP version's client end of a link. Session holds the
// logic both versions share; this holds what differs on the wire.
type protocol interface {
	serve(ctx context.Context) error
	onExtNotification(method string, fn func(params []byte)) error
	// initialize sends initialize, or reads the cached result when raw is set.
	initialize(ctx context.Context, raw []byte, info acp.Implementation) (agentInit, error)
	newSession(ctx context.Context, cwd string) (string, error)
	// reopen opens id in a new agent process: session/load in v1,
	// session/resume in v2.
	reopen(ctx context.Context, id, cwd string) error
	// prompt sends a prompt. done is true when its result ended the turn
	// (v1); in v2 the result only acknowledges it and the host's turn_end
	// ends it.
	prompt(ctx context.Context, id string, blocks []acp.ContentBlock) (stop acp.StopReason, done bool, err error)
	cancel(ctx context.Context, id string) error
}

// agentInit is what the client uses from an initialize result.
type agentInit struct {
	raw                    []byte // the result, for the next connection
	version                int
	name, agentVersion     string
	reopen                 bool // the agent can open a session again after a restart
	image, embeddedContext bool
}

// permissionAsk is a permission request in either version.
type permissionAsk struct {
	kind, title string
	options     []permissionChoice
}

type permissionChoice struct{ id, kind string }

// errNotV2 is an agent that answered a v2 initialize with another version.
var errNotV2 = errors.New("the agent does not speak ACP protocol version 2 (experimental --protocol 2)")

// v1 is ACP protocol version 1, the default.
type v1 struct{ conn *acp.ClientSideConnection }

func newV1(r *agentRequests, w io.Writer, rd io.Reader) *v1 {
	return &v1{conn: acp.NewClientSideConnection(r, w, rd, nil)}
}

func (p *v1) serve(ctx context.Context) error { return p.conn.Serve(ctx) }

func (p *v1) onExtNotification(method string, fn func([]byte)) error {
	return p.conn.RegisterExtensionNotification(method, func(_ context.Context, _ string, params jsontext.Value) (any, *acp.RequestError) {
		fn(params)
		return nil, nil
	})
}

func (p *v1) initialize(ctx context.Context, raw []byte, info acp.Implementation) (agentInit, error) {
	var init acp.InitializeResponse
	if len(raw) > 0 {
		if err := jsonv2.Unmarshal(raw, &init); err != nil {
			return agentInit{}, fmt.Errorf("initialize result from the host: %w", err)
		}
	} else {
		var err error
		init, err = p.conn.Initialize(ctx, acp.InitializeRequest{
			ProtocolVersion: acp.ProtocolVersion(acp.ProtocolVersionNumber), ClientInfo: &info,
		})
		if err != nil {
			return agentInit{}, err
		}
		raw, _ = jsonv2.Marshal(init)
	}
	ai := agentInit{
		raw: raw, version: int(init.ProtocolVersion), reopen: init.AgentCapabilities.LoadSession,
		image: init.AgentCapabilities.PromptCapabilities.Image, embeddedContext: init.AgentCapabilities.PromptCapabilities.EmbeddedContext,
	}
	if init.AgentInfo != nil {
		ai.name, ai.agentVersion = init.AgentInfo.Name, init.AgentInfo.Version
	}
	return ai, nil
}

func (p *v1) newSession(ctx context.Context, cwd string) (string, error) {
	res, err := p.conn.NewSession(ctx, acp.NewSessionRequest{Cwd: cwd, McpServers: []acp.McpServer{}})
	return string(res.SessionId), err
}

func (p *v1) reopen(ctx context.Context, id, cwd string) error {
	_, err := p.conn.LoadSession(ctx, acp.LoadSessionRequest{SessionId: acp.SessionId(id), Cwd: cwd, McpServers: []acp.McpServer{}})
	return err
}

func (p *v1) prompt(ctx context.Context, id string, blocks []acp.ContentBlock) (acp.StopReason, bool, error) {
	res, err := p.conn.Prompt(ctx, acp.PromptRequest{SessionId: acp.SessionId(id), Prompt: blocks})
	return res.StopReason, true, err
}

func (p *v1) cancel(ctx context.Context, id string) error {
	return p.conn.Cancel(ctx, acp.CancelNotification{SessionId: acp.SessionId(id)})
}

// v2 is the ACP protocol version 2 draft.
type v2 struct{ conn *acp2.ClientSideConnection }

func newV2(r *agentRequests, w io.Writer, rd io.Reader) *v2 {
	return &v2{conn: acp2.NewClientSideConnection(v2Requests{r}, w, rd, nil)}
}

func (p *v2) serve(ctx context.Context) error { return p.conn.Serve(ctx) }

func (p *v2) onExtNotification(method string, fn func([]byte)) error {
	return p.conn.RegisterExtensionNotification(method, func(_ context.Context, _ string, params jsontext.Value) (any, *acp2.RequestError) {
		fn(params)
		return nil, nil
	})
}

func (p *v2) initialize(ctx context.Context, raw []byte, info acp.Implementation) (agentInit, error) {
	var init acp2.InitializeResponse
	if len(raw) > 0 {
		if err := jsonv2.Unmarshal(raw, &init); err != nil {
			return agentInit{}, fmt.Errorf("initialize result from the host: %w", err)
		}
	} else {
		var err error
		init, err = p.conn.Initialize(ctx, acp2.InitializeRequest{
			ProtocolVersion: acp2.ProtocolVersion(acp2.ProtocolVersionNumber),
			Info:            acp2.Implementation{Name: info.Name, Version: info.Version},
		})
		if err != nil {
			return agentInit{}, err
		}
		raw, _ = jsonv2.Marshal(init)
	}
	if init.ProtocolVersion != acp2.ProtocolVersionNumber {
		return agentInit{}, fmt.Errorf("%w: it answered version %d", errNotV2, init.ProtocolVersion)
	}
	// session/resume is baseline in v2.
	ai := agentInit{raw: raw, version: int(init.ProtocolVersion), reopen: true, name: init.Info.Name, agentVersion: init.Info.Version}
	if s := init.Capabilities.Session; s != nil && s.Prompt != nil {
		ai.image, ai.embeddedContext = s.Prompt.Image != nil, s.Prompt.EmbeddedContext != nil
	}
	return ai, nil
}

func (p *v2) newSession(ctx context.Context, cwd string) (string, error) {
	res, err := p.conn.NewSession(ctx, acp2.NewSessionRequest{Cwd: acp2.AbsolutePath(cwd)})
	return string(res.SessionId), err
}

func (p *v2) reopen(ctx context.Context, id, cwd string) error {
	_, err := p.conn.ResumeSession(ctx, acp2.ResumeSessionRequest{SessionId: acp2.SessionId(id), Cwd: acp2.AbsolutePath(cwd)})
	return err
}

func (p *v2) prompt(ctx context.Context, id string, blocks []acp.ContentBlock) (acp.StopReason, bool, error) {
	// Text, image and resource blocks have the same wire form in both versions.
	b, err := jsonv2.Marshal(blocks)
	if err != nil {
		return "", false, err
	}
	var prompt []acp2.ContentBlock
	if err := jsonv2.Unmarshal(b, &prompt); err != nil {
		return "", false, err
	}
	_, err = p.conn.Prompt(ctx, acp2.PromptRequest{SessionId: acp2.SessionId(id), Prompt: prompt})
	return "", false, err
}

func (p *v2) cancel(ctx context.Context, id string) error {
	return p.conn.Cancel(ctx, acp2.CancelSessionNotification{SessionId: acp2.SessionId(id)})
}

// v2Requests answers a v2 agent's requests with the shared handlers.
type v2Requests struct{ r *agentRequests }

func (q v2Requests) SessionUpdate(context.Context, acp2.UpdateSessionNotification) error {
	q.r.l.grew()
	return nil
}

func (q v2Requests) RequestPermission(_ context.Context, p acp2.RequestPermissionRequest) (acp2.RequestPermissionResponse, error) {
	q.r.l.grew()
	ask := permissionAsk{title: p.Title}
	if p.Subject != nil && p.Subject.ToolCall != nil && p.Subject.ToolCall.ToolCall.Kind != nil {
		ask.kind = string(*p.Subject.ToolCall.ToolCall.Kind)
	}
	for _, o := range p.Options {
		ask.options = append(ask.options, permissionChoice{id: string(o.OptionId), kind: string(o.Kind)})
	}
	var out acp2.RequestPermissionOutcome
	if id, ok := q.r.s.decide(ask); ok {
		out.Selected = &acp2.RequestPermissionOutcomeSelected{Outcome: "selected", OptionId: acp2.PermissionOptionId(id)}
	} else {
		out.Cancelled = &acp2.RequestPermissionOutcomeCancelled{Outcome: "cancelled"}
	}
	return acp2.RequestPermissionResponse{Outcome: out}, nil
}

func (q v2Requests) CompleteElicitation(context.Context, acp2.CompleteElicitationNotification) error {
	return nil
}

func (q v2Requests) CreateElicitation(context.Context, acp2.CreateElicitationRequest) (acp2.CreateElicitationResponse, error) {
	return acp2.CreateElicitationResponse{}, acp2.NewMethodNotFound(acp2.ClientMethodElicitationCreate)
}

// requestError is err as a JSON-RPC error of either version.
func requestError(err error) bool {
	var e1 *acp.RequestError
	var e2 *acp2.RequestError
	return errors.As(err, &e1) || errors.As(err, &e2)
}

// isClosed reports a connection that ended, in either version.
func isClosed(err error) bool { return errors.Is(err, acp.ErrClosed) || errors.Is(err, acp2.ErrClosed) }

// initVersion is the protocolVersion of a cached initialize result, 0 if
// there is none.
func initVersion(raw []byte) int {
	var r struct {
		ProtocolVersion int `json:"protocolVersion"`
	}
	_ = json.Unmarshal(raw, &r)
	return r.ProtocolVersion
}

// protocol is the ACP version to speak on a connection with welcome w:
// what the agent already answered, else the version the session was
// created with, else the one asked for.
func (s *Session) protocol(w acphost.Welcome) int {
	if v := initVersion(w.Initialize); v != 0 {
		return v
	}
	if s.state.Protocol != 0 {
		return s.state.Protocol
	}
	return s.opts.Protocol
}
