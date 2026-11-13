package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/gfx-labs/acphost/pkg/acphost"
)

// The ACP agent of the session tests is this test binary, run as
// "<test binary> fake-acp-agent [MODE]". A real host runs it, so the
// tests cover the client against the host's socket and journal. The first
// word of a prompt picks what the turn does; any other prompt is echoed
// back after the agent's session id.

const (
	fakeAgentArg = "fake-acp-agent"
	// Modes.
	agentLoads     = "load"     // session/load works and replays a message
	agentLoadFails = "loadfail" // session/load is offered and fails
	agentQuits     = "quit"     // exits 3 on initialize
	agentPlain     = "plain"    // no image or embeddedContext prompt capability
)

// fakeAgentCommand is the agent command line for a test, with the "--".
func fakeAgentCommand(mode ...string) []string {
	return append([]string{"--", os.Args[0], fakeAgentArg}, mode...)
}

type fakeAgent struct {
	mode    string
	mu      sync.Mutex // serializes writes to stdout
	session string
	cancel  chan struct{} // a session/cancel arrived
	answers chan message  // responses to this agent's requests
}

// The fake agent speaks raw JSON-RPC with its own types, as an agent this
// repository did not write would.
type (
	message struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id,omitempty"`
		Method  string          `json:"method,omitempty"`
		Params  json.RawMessage `json:"params,omitempty"`
		Result  json.RawMessage `json:"result,omitempty"`
		Error   *rpcError       `json:"error,omitempty"`
	}
	rpcError struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	contentText struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	initializeResult struct {
		ProtocolVersion   int `json:"protocolVersion"`
		AgentCapabilities struct {
			LoadSession        bool `json:"loadSession"`
			PromptCapabilities struct {
				Image           bool `json:"image"`
				EmbeddedContext bool `json:"embeddedContext"`
			} `json:"promptCapabilities"`
		} `json:"agentCapabilities"`
	}
	// promptBlock is a content block of a prompt, as the fake agent reads it.
	promptBlock struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		MimeType string `json:"mimeType"`
		URI      string `json:"uri"`
		Data     string `json:"data"`
		Resource struct {
			URI      string `json:"uri"`
			MimeType string `json:"mimeType"`
			Text     string `json:"text"`
			Blob     string `json:"blob"`
		} `json:"resource"`
	}
	sessionParams struct {
		SessionID string `json:"sessionId"`
	}
	sessionResult struct {
		SessionID string `json:"sessionId"`
	}
	promptParams struct {
		Prompt []promptBlock `json:"prompt"`
	}
	promptResult struct {
		StopReason string `json:"stopReason"`
	}
	permissionResult struct {
		Outcome struct {
			Outcome  string `json:"outcome"`
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	}
	agentUpdate struct {
		SessionID string       `json:"sessionId"`
		Update    agentMessage `json:"update"`
	}
	agentMessage struct {
		Kind    string      `json:"sessionUpdate"`
		Content contentText `json:"content"`
	}
	agentToolCall struct {
		ToolCallID string `json:"toolCallId"`
		Title      string `json:"title"`
		Kind       string `json:"kind"`
	}
	agentOption struct {
		OptionID string `json:"optionId"`
		Kind     string `json:"kind"`
	}
	agentPermission struct {
		SessionID string        `json:"sessionId"`
		ToolCall  agentToolCall `json:"toolCall"`
		Options   []agentOption `json:"options"`
	}
)

// runFakeAgent serves ACP on stdin and stdout until stdin closes.
func runFakeAgent(args []string) {
	a := &fakeAgent{cancel: make(chan struct{}, 1), answers: make(chan message, 1)}
	if len(args) > 0 {
		a.mode = args[0]
	}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<24)
	for sc.Scan() {
		var m message
		if json.Unmarshal(sc.Bytes(), &m) == nil {
			a.handle(m)
		}
	}
	os.Exit(0)
}

func (a *fakeAgent) send(m message) {
	m.JSONRPC = "2.0"
	b, _ := json.Marshal(m)
	a.mu.Lock()
	defer a.mu.Unlock()
	_, _ = os.Stdout.Write(append(b, '\n'))
}

func (a *fakeAgent) say(text string) {
	a.send(message{Method: "session/update", Params: rawJSON(agentUpdate{
		SessionID: a.session,
		Update:    agentMessage{Kind: "agent_message_chunk", Content: contentText{Type: "text", Text: text}},
	})})
}

func (a *fakeAgent) handle(m message) {
	switch m.Method {
	case "":
		a.answers <- m
	case "initialize":
		if a.mode == agentQuits {
			os.Exit(3)
		}
		res := initializeResult{ProtocolVersion: 1}
		res.AgentCapabilities.LoadSession = a.mode == agentLoads || a.mode == agentLoadFails
		// The default agent takes images and embedded files; "plain" only
		// the baseline, text and resource links.
		res.AgentCapabilities.PromptCapabilities.Image = a.mode != agentPlain
		res.AgentCapabilities.PromptCapabilities.EmbeddedContext = a.mode != agentPlain
		a.send(message{ID: m.ID, Result: rawJSON(res)})
	case "session/new":
		a.session = fmt.Sprintf("sess-%d", os.Getpid())
		a.send(message{ID: m.ID, Result: rawJSON(sessionResult{SessionID: a.session})})
	case "session/load":
		if a.mode == agentLoadFails {
			a.send(message{ID: m.ID, Error: &rpcError{Code: -32002, Message: "session not found"}})
			return
		}
		var p sessionParams
		_ = json.Unmarshal(m.Params, &p)
		a.session = p.SessionID
		a.say("replayed\n")
		a.send(message{ID: m.ID, Result: rawJSON(struct{}{})})
	case "session/prompt":
		// A turn runs beside the reader, which has to see its cancel.
		go a.turn(m)
	case "session/cancel":
		select {
		case a.cancel <- struct{}{}:
		default:
		}
	}
}

func (a *fakeAgent) turn(m message) {
	var p promptParams
	_ = json.Unmarshal(m.Params, &p)
	var text string
	if len(p.Prompt) > 0 {
		text = p.Prompt[0].Text
	}
	// A seeded prompt has the summary first; the command is what follows.
	_, command, seeded := strings.Cut(text, acphost.SummaryEnd)
	if !seeded {
		command = text
	}
	words := strings.Fields(command)
	arg := ""
	if len(words) > 1 {
		arg = words[1]
	}
	stop := "end_turn"
	switch {
	case len(words) == 0:
	case words[0] == "blocks":
		// One line per block after the text: type, mime type, uri, size.
		for _, b := range p.Prompt[1:] {
			uri, mt, size := b.URI, b.MimeType, len(b.Resource.Text)+len(b.Resource.Blob)
			if b.Type == "resource" {
				uri, mt = b.Resource.URI, b.Resource.MimeType
			}
			if b.Type == "image" {
				size = len(b.Data)
			}
			a.say(fmt.Sprintf("block %s %s %s %d\n", b.Type, mt, uri, size))
		}
	case words[0] == "ask":
		a.say("permission: " + a.ask(arg) + "\n")
	case words[0] == "hold":
		a.say("holding\n")
		<-a.cancel
		stop = "cancelled"
	case words[0] == "wait":
		a.say("waiting\n")
		// Opening a FIFO blocks until the test opens it for writing.
		released := make(chan struct{})
		go func() {
			_, _ = os.ReadFile(arg)
			close(released)
		}()
		select {
		case <-released:
			a.say("released\n")
		case <-a.cancel:
			stop = "cancelled"
		}
	case words[0] == "stop":
		stop = arg
	case words[0] == "env":
		a.say(arg + "=" + os.Getenv(arg) + "\n")
	case words[0] == "fail":
		a.send(message{ID: m.ID, Error: &rpcError{Code: -32000, Message: "boom"}})
		return
	case words[0] == "die":
		os.Exit(7)
	default:
		a.say(a.session + "> " + text + "\n")
	}
	a.send(message{ID: m.ID, Result: rawJSON(promptResult{StopReason: stop})})
}

// ask requests permission for a tool call of the given kind and returns
// the outcome as text. The always options come first: the client should
// still pick a one-time one.
func (a *fakeAgent) ask(kind string) string {
	a.send(message{ID: json.RawMessage(`"perm"`), Method: "session/request_permission", Params: rawJSON(agentPermission{
		SessionID: a.session,
		ToolCall:  agentToolCall{ToolCallID: "t1", Title: "touch " + kind, Kind: kind},
		Options: []agentOption{
			{OptionID: "always", Kind: "allow_always"},
			{OptionID: "never", Kind: "reject_always"},
			{OptionID: "once", Kind: "allow_once"},
			{OptionID: "no", Kind: "reject_once"},
		},
	})})
	res := <-a.answers
	var r permissionResult
	_ = json.Unmarshal(res.Result, &r)
	return strings.TrimSpace(r.Outcome.Outcome + " " + r.Outcome.OptionID)
}

// maybeFakeAgent runs the fake agent if the test binary was started as one.
func maybeFakeAgent() {
	if i := slices.Index(os.Args, fakeAgentArg); i == 1 {
		runFakeAgent(os.Args[i+1:])
	}
}

func rawJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
