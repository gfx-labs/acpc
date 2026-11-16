package main

import (
	"testing"

	"github.com/gfx-labs/acpc/pkg/acpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The experimental --protocol 2: the prompt result only acknowledges, the
// turn ends on the agent's idle state_update (turn_end from the host), and
// a restarted agent resumes the conversation with session/resume. A
// v1-only agent is refused rather than silently spoken to in v1.
func TestProtocolV2(t *testing.T) {
	sessionEnv(t)
	dir := newSession(t, []string{"--protocol", "2"}, agentV2)

	out := mustAcp(t, "prompt", "hello")
	assert.Contains(t, out, "> hello")
	assert.Contains(t, out, "end_turn")
	id := agentSession(t, out)

	code, _, errs := acpcRun("prompt", "stop", "max_tokens")
	assert.Equal(t, 1, code, "a stop reason other than end_turn: %s", errs)
	assert.Contains(t, errs, "max_tokens")

	// A turn detached with --no-wait: the session stays busy until the
	// agent reports idle, and cancel ends it.
	mustAcp(t, "prompt", "--no-wait", "hold")
	busy, err := acpc.HostBusy(dir, "")
	require.NoError(t, err)
	assert.True(t, busy, "an acknowledged v2 prompt is still running")
	mustAcp(t, "cancel")
	drained(t, dir)

	mustAcp(t, "stop", "-s", acpc.DefaultSession)
	out = mustAcp(t, "prompt", "again")
	assert.Contains(t, out, id+"> again", "resumed the same session")

	code, _, errs = acpcRun(append([]string{"new", "-s", "old", "--protocol", "2"}, fakeAgentCommand()...)...)
	require.Equal(t, 1, code)
	assert.Contains(t, errs, "does not speak ACP protocol version 2")
}
