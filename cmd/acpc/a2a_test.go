package main

import (
	"bufio"
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// acpc a2a against the fake agent, through the a2a-go client: a context
// keeps its session across tasks, a permission request waits at
// input-required for the client's reply, and cancel ends a running turn.

// serveA2A starts the acpc binary bin as acpc a2a with flags and returns
// a client for it.
func serveA2A(t *testing.T, bin string, flags ...string) *a2aclient.Client {
	t.Helper()
	args := append(append([]string{"a2a", "--listen", "127.0.0.1:0"}, flags...), fakeAgentCommand()...)
	cmd := exec.Command(bin, args...)
	stderr, err := cmd.StderrPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	line, err := bufio.NewReader(stderr).ReadString('\n')
	require.NoError(t, err, line)
	fields := strings.Fields(line)
	require.GreaterOrEqual(t, len(fields), 6, line)
	url := "http://" + strings.TrimSuffix(fields[5], ",") + "/"
	c, err := a2aclient.NewFromEndpoints(context.Background(), []*a2a.AgentInterface{a2a.NewAgentInterface(url, a2a.TransportProtocolJSONRPC)})
	require.NoError(t, err)
	return c
}

// streamed is what a task's stream showed: the agent's text and the
// status it stopped at.
type streamed struct {
	task  a2a.TaskID
	text  string
	state a2a.TaskState
	said  string // the last status message
}

// send streams one message and returns what it showed. onText sees what
// it showed so far after each part of the agent's text.
func send(t *testing.T, c *a2aclient.Client, msg *a2a.Message, onText func(streamed)) streamed {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var s streamed
	for ev, err := range c.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: msg}) {
		require.NoError(t, err)
		switch e := ev.(type) {
		case *a2a.Task:
			s.task = e.ID
		case *a2a.TaskArtifactUpdateEvent:
			s.task = e.TaskID
			for _, p := range e.Artifact.Parts {
				s.text += p.Text()
			}
			if onText != nil {
				onText(s)
			}
		case *a2a.TaskStatusUpdateEvent:
			s.task, s.state = e.TaskID, e.Status.State
			if e.Status.Message != nil {
				s.said = messageText(e.Status.Message)
			}
		}
	}
	return s
}

func userMessage(contextID string, text string) *a2a.Message {
	m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	m.ContextID = contextID
	return m
}

func TestA2A(t *testing.T) {
	bin := buildCLI(t)
	sessionEnv(t)
	c := serveA2A(t, bin, "--ask")

	first := send(t, c, userMessage("ctx-a", "hello"), nil)
	assert.Equal(t, a2a.TaskStateCompleted, first.state, first.said)
	assert.Contains(t, first.text, "> hello")
	again := send(t, c, userMessage("ctx-a", "again"), nil)
	assert.Equal(t, a2a.TaskStateCompleted, again.state, again.said)
	assert.Equal(t, agentSession(t, first.text), agentSession(t, again.text), "a context keeps its session")
	other := send(t, c, userMessage("ctx-b", "other"), nil)
	assert.NotEqual(t, agentSession(t, first.text), agentSession(t, other.text), "another context has its own")

	asked := send(t, c, userMessage("ctx-a", "ask edit"), nil)
	require.Equal(t, a2a.TaskStateInputRequired, asked.state, asked.said)
	assert.Contains(t, asked.said, "touch edit")
	reply := userMessage("ctx-a", "maybe")
	reply.TaskID = asked.task
	retried := send(t, c, reply, nil)
	require.Equal(t, a2a.TaskStateInputRequired, retried.state, retried.said)
	assert.Contains(t, retried.said, "not one of the options")
	reply = userMessage("ctx-a", "once")
	reply.TaskID = asked.task
	answered := send(t, c, reply, nil)
	assert.Equal(t, a2a.TaskStateCompleted, answered.state, answered.said)
	assert.Contains(t, answered.text, "permission: selected once")

	waiting := send(t, c, userMessage("ctx-a", "ask execute"), nil)
	require.Equal(t, a2a.TaskStateInputRequired, waiting.state, waiting.said)
	stopped, err := c.CancelTask(context.Background(), &a2a.CancelTaskRequest{ID: waiting.task})
	require.NoError(t, err)
	assert.Equal(t, a2a.TaskStateCanceled, stopped.Status.State)

	cancelled := make(chan error, 1)
	held := send(t, c, userMessage("ctx-a", "hold"), func(s streamed) {
		if strings.Contains(s.text, "holding") {
			go func() {
				_, err := c.CancelTask(context.Background(), &a2a.CancelTaskRequest{ID: s.task})
				cancelled <- err
			}()
		}
	})
	assert.Equal(t, a2a.TaskStateCanceled, held.state, held.said)
	require.NoError(t, <-cancelled)

	after := send(t, c, userMessage("ctx-a", "after"), nil)
	assert.Equal(t, a2a.TaskStateCompleted, after.state, after.said)
	assert.Equal(t, agentSession(t, first.text), agentSession(t, after.text))
}

func TestA2AListensOnlyOnLoopback(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", "192.0.2.1:0", ":0"} {
		code, _, errs := acpcRun(append([]string{"a2a", "--listen", addr}, fakeAgentCommand()...)...)
		assert.Equal(t, 1, code, addr)
		assert.Contains(t, errs, "only on loopback", addr)
	}
}
