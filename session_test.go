package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/gfx-labs/acphost/pkg/acphost"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// The session commands against a real detached host running the fake
// agent of agent_test.go. Nothing here waits on the clock: a turn that has
// to be running blocks in the agent until the test cancels or releases it.

// shortDir is a temp dir with a short path: a session's socket path has
// to fit in 107 bytes, and t.TempDir puts the test's name in it.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "s")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// stopHosts stops the hosts left in the dirs matching glob.
func stopHosts(t *testing.T, glob string) {
	t.Helper()
	t.Cleanup(func() {
		dirs, _ := filepath.Glob(glob)
		for _, d := range dirs {
			_ = stopHost(d)
		}
	})
}

// sessionEnv gives the test its own sessions root and working directory
// and returns the root.
func sessionEnv(t *testing.T) string {
	t.Helper()
	state := shortDir(t)
	t.Setenv("XDG_STATE_HOME", state)
	t.Chdir(shortDir(t))
	root := filepath.Join(state, "acphost", "sessions")
	stopHosts(t, filepath.Join(root, "*", "*"))
	return root
}

// acpIn runs one command line in this process with stdin.
func acpIn(stdin string, args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = run(args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func acp(args ...string) (code int, stdout, stderr string) { return acpIn("", args...) }

// mustAcp runs a command line that has to succeed and returns its stdout.
func mustAcp(t *testing.T, args ...string) string {
	t.Helper()
	code, out, errs := acp(args...)
	require.Equal(t, 0, code, "%v: %s", args, errs)
	return out
}

func newSession(t *testing.T, flags []string, mode ...string) string {
	t.Helper()
	return strings.TrimSpace(mustAcp(t, append(append([]string{"new"}, flags...), fakeAgentCommand(mode...)...)...))
}

var echoed = regexp.MustCompile(`(sess-\d+)> `)

// agentSession is the agent's session id in an echoed prompt.
func agentSession(t *testing.T, out string) string {
	t.Helper()
	m := echoed.FindStringSubmatch(out)
	require.NotNil(t, m, "no echoed prompt in %q", out)
	return m[1]
}

func listSessions(t *testing.T, flags ...string) []listed {
	t.Helper()
	var l []listed
	out := mustAcp(t, append([]string{"ls", "--format", "json"}, flags...)...)
	require.NoError(t, json.Unmarshal([]byte(out), &l), out)
	return l
}

func hostPid(t *testing.T) int {
	t.Helper()
	var st statusOut
	out := mustAcp(t, "status", "-s", defaultSession)
	require.NoError(t, json.Unmarshal([]byte(out), &st), out)
	require.Equal(t, "running", st.State)
	return st.Pid
}

// One conversation across prompts and across a host restart: each prompt
// shows only its own turn, the agent keeps its session id, and a name
// belongs to the working directory it was created in.
func TestSessionConversation(t *testing.T) {
	root := sessionEnv(t)
	cwd, err := os.Getwd()
	require.NoError(t, err)

	code, _, errs := acp("prompt", "hello")
	assert.Equal(t, 1, code)
	assert.Contains(t, errs, "acphost new creates one")

	dir := newSession(t, nil, agentLoads)
	assert.Equal(t, defaultSession, filepath.Base(dir))
	assert.True(t, strings.HasPrefix(dir, root+string(filepath.Separator)), dir)
	code, _, errs = acp(append([]string{"new"}, fakeAgentCommand()...)...)
	assert.Equal(t, 1, code)
	assert.Contains(t, errs, "already exists")

	out := mustAcp(t, "prompt", "hello", "there")
	assert.True(t, strings.HasPrefix(out, "=== turn "), "the handshake is not shown: %q", out)
	assert.Contains(t, out, "> hello there")
	assert.Contains(t, out, "end_turn")
	id := agentSession(t, out)

	out = mustAcp(t, "prompt", "again")
	assert.Contains(t, out, id+"> again")
	assert.NotContains(t, out, "hello", "an earlier turn is not shown twice")

	l := listSessions(t)
	require.Len(t, l, 1)
	assert.Equal(t, defaultSession, l[0].Name)
	assert.Equal(t, "running", l[0].State)
	assert.Equal(t, cwd, l[0].Cwd)
	assert.Equal(t, fakeAgentCommand(agentLoads)[1:], l[0].Command)
	assert.NotNil(t, l[0].Last)

	// Another working directory does not see the name.
	t.Chdir(shortDir(t))
	assert.Empty(t, listSessions(t))
	assert.Len(t, listSessions(t, "--all"), 1)
	code, _, _ = acp("prompt", "hello")
	assert.Equal(t, 1, code)
	t.Chdir(cwd)

	// A stopped host: the next prompt starts one, and the new agent loads
	// the conversation. What it replays while loading is not shown.
	first := hostPid(t)
	mustAcp(t, "stop", "-s", defaultSession)
	assert.Equal(t, "stopped", listSessions(t)[0].State)
	out = mustAcp(t, "prompt", "third")
	assert.Contains(t, out, id+"> third")
	assert.NotContains(t, out, "replayed")
	assert.NotEqual(t, first, hostPid(t))

	mustAcp(t, "rm", "-s", defaultSession)
	assert.NoDirExists(t, dir)
	assert.Empty(t, listSessions(t))
}

// An agent that cannot load the conversation after a restart gets a new
// one, seeded with a summary of the journal in front of the prompt.
func TestSessionReloadFallback(t *testing.T) {
	for name, tc := range map[string]struct {
		mode      []string
		loadError string
	}{
		"no load capability": {nil, ""},
		"load fails":         {[]string{agentLoadFails}, "session not found"},
	} {
		t.Run(name, func(t *testing.T) {
			sessionEnv(t)
			newSession(t, nil, tc.mode...)
			old := agentSession(t, mustAcp(t, "prompt", "remember", "apples"))
			mustAcp(t, "stop", "-s", defaultSession)

			code, out, errs := acp("prompt", "next")
			require.Equal(t, 0, code, errs)
			assert.Contains(t, errs, "could not reload the conversation")
			if tc.loadError != "" {
				assert.Contains(t, errs, tc.loadError)
			}
			fresh := agentSession(t, out)
			assert.NotEqual(t, old, fresh)
			// The agent echoes the prompt it got: summary, marker, text.
			_, got, _ := strings.Cut(out, fresh+"> ")
			summary, text, ok := strings.Cut(got, acphost.SummaryEnd)
			require.True(t, ok, got)
			assert.Contains(t, summary, "remember apples")
			assert.Contains(t, text, "next")

			// The new conversation is the session's from here on.
			code, out, errs = acp("prompt", "more")
			require.Equal(t, 0, code, errs)
			assert.NotContains(t, errs, "could not reload")
			assert.Contains(t, out, fresh+"> more")
		})
	}
}

// The permission policy picks the option the agent is answered with, the
// one-time one when the agent offers both.
func TestSessionPermissions(t *testing.T) {
	sessionEnv(t)
	newSession(t, nil)
	for _, tc := range []struct {
		flags   []string
		kind    string
		allowed bool
	}{
		{nil, "read", true},
		{nil, "search", true},
		{nil, "edit", false},
		{[]string{"--approve-reads"}, "execute", false},
		{[]string{"--approve-all"}, "edit", true},
		{[]string{"--deny-all"}, "read", false},
	} {
		code, out, errs := acp(append(append([]string{"prompt"}, tc.flags...), "ask", tc.kind)...)
		require.Equal(t, 0, code, errs)
		if tc.allowed {
			assert.Contains(t, out, "permission: selected once", "%v %s", tc.flags, tc.kind)
			assert.NotContains(t, errs, "refused")
		} else {
			assert.Contains(t, out, "permission: selected no", "%v %s", tc.flags, tc.kind)
			assert.Contains(t, errs, "refused permission: touch "+tc.kind+" ("+tc.kind+")")
		}
	}
	code, _, errs := acp("prompt", "--approve-all", "--deny-all", "ask", "read")
	assert.Equal(t, 2, code)
	assert.Contains(t, errs, "one of")
}

// cancel reaches a turn that another process is waiting on: that prompt
// cancels its turn and exits 130, and the session stays usable.
func TestSessionCancelForegroundTurn(t *testing.T) {
	bin := buildCLI(t)
	sessionEnv(t)
	newSession(t, nil)

	cmd := exec.Command(bin, "prompt", "hold")
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	// Read while the command runs, for the failure messages.
	stderr := &watched{}
	cmd.Stderr = stderr
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	rd := bufio.NewReader(stdout)
	var shown strings.Builder
	for !strings.Contains(shown.String(), "holding") {
		line, err := rd.ReadString('\n')
		require.NoError(t, err, "%s%s", shown.String(), stderr.String())
		shown.WriteString(line)
	}

	// The session has one client at a time.
	code, _, errs := acp("prompt", "hi")
	assert.Equal(t, 1, code)
	assert.Contains(t, errs, "in use by pid")
	assert.Equal(t, cmd.Process.Pid, listSessions(t)[0].ClientPid)

	mustAcp(t, "cancel")
	rest, err := rd.ReadString(0) // to EOF
	require.ErrorContains(t, err, "EOF")
	var exit *exec.ExitError
	require.ErrorAs(t, cmd.Wait(), &exit, stderr.String())
	assert.Equal(t, 130, exit.ExitCode(), stderr.String())
	assert.Contains(t, rest, "cancelled")

	assert.Contains(t, mustAcp(t, "prompt", "after"), "> after")
}

// watched is command output that can be read while the command runs, and
// that reports when it first contains marker, if one is set.
type watched struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	marker string
	seen   chan struct{}
	once   sync.Once
}

func (w *watched) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	if w.marker != "" && bytes.Contains(w.buf.Bytes(), []byte(w.marker)) {
		w.once.Do(func() { close(w.seen) })
	}
	return len(p), nil
}

func (w *watched) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// A turn nobody waits on: --no-wait leaves it running, the session takes
// no second prompt or config change meanwhile, attach shows it to its end,
// and cancel stops one.
func TestSessionDetachedTurn(t *testing.T) {
	sessionEnv(t)
	newSession(t, nil)
	gate := filepath.Join(shortDir(t), "gate")
	require.NoError(t, unix.Mkfifo(gate, 0o600))
	config := filepath.Join(shortDir(t), "c.yaml")
	require.NoError(t, os.WriteFile(config, []byte("env: [FOO=bar]\n"), 0o600))

	assert.Empty(t, mustAcp(t, "prompt", "--no-wait", "wait", gate))
	code, _, errs := acp("prompt", "second")
	assert.Equal(t, 1, code)
	assert.Contains(t, errs, "a turn is running")
	code, _, errs = acp("prompt", "-c", config, "second")
	assert.Equal(t, 1, code)
	assert.Contains(t, errs, "the config cannot change now")

	out := &watched{marker: "waiting", seen: make(chan struct{})}
	done := make(chan int)
	var attachErr bytes.Buffer
	go func() { done <- run([]string{"attach"}, nil, out, &attachErr) }()
	// attach is connected and has shown the turn so far. Opening the gate
	// returns once the agent reads it, which ends the turn.
	select {
	case <-out.seen:
	case code := <-done:
		require.FailNow(t, "attach returned before the turn ended", "status %d: %s%s", code, out.String(), attachErr.String())
	}
	g, err := os.OpenFile(gate, os.O_WRONLY, 0)
	require.NoError(t, err)
	require.NoError(t, g.Close())
	require.Equal(t, 0, <-done, attachErr.String())
	assert.Contains(t, out.String(), "=== turn ")
	assert.Contains(t, out.String(), "released")
	assert.Contains(t, out.String(), "end_turn")
	assert.Empty(t, mustAcp(t, "attach"), "nothing new since the last attach")

	mustAcp(t, "prompt", "--no-wait", "hold")
	mustAcp(t, "cancel")
	out2 := mustAcp(t, "attach")
	assert.Contains(t, out2, "cancel requested")
	assert.Contains(t, out2, "cancelled")
	code, _, errs = acp("cancel")
	assert.Equal(t, 0, code)
	assert.Contains(t, errs, "no turn is running")
}

// -c on a later prompt replaces the session's isolation: the host
// restarts with it, and it stays for the prompts after.
func TestSessionReconfigure(t *testing.T) {
	sessionEnv(t)
	cfg := shortDir(t)
	one, two := filepath.Join(cfg, "one.yaml"), filepath.Join(cfg, "two.yaml")
	require.NoError(t, os.WriteFile(one, []byte("env: [FOO=one]\n"), 0o600))
	require.NoError(t, os.WriteFile(two, []byte("env: [FOO=two]\n"), 0o600))

	newSession(t, []string{"-c", one}, agentLoads)
	assert.Equal(t, "FOO=one\n", mustAcp(t, "prompt", "--format", "quiet", "env", "FOO"))
	first := hostPid(t)
	// The same config again changes nothing.
	assert.Equal(t, "FOO=one\n", mustAcp(t, "prompt", "-c", one, "--format", "quiet", "env", "FOO"))
	assert.Equal(t, first, hostPid(t))

	assert.Equal(t, "FOO=two\n", mustAcp(t, "prompt", "-c", two, "--format", "quiet", "env", "FOO"))
	second := hostPid(t)
	assert.NotEqual(t, first, second)
	assert.Equal(t, "FOO=two\n", mustAcp(t, "prompt", "--format", "quiet", "env", "FOO"))
	assert.Equal(t, second, hostPid(t))
}

// In a sandbox, bwrap finds out that the agent command cannot run only
// after the host started. That is still a spawn failure: 125.
func TestSandboxExecMissingAgent(t *testing.T) {
	needBwrap(t)
	// The agent's working directory has to be inside an rw mount.
	work := shortDir(t)
	t.Chdir(work)
	tmp := shortDir(t)
	t.Setenv("TMPDIR", tmp)
	stopHosts(t, filepath.Join(tmp, "*"))
	ro := ""
	for _, p := range []string{"/nix", "/bin", "/usr", "/lib", "/lib64", "/etc"} {
		ro += "    - {src: " + p + ", skip_missing: true}\n"
	}
	cfg := "sandbox:\n  ro:\n" + ro + "  rw: [" + work + "]\n  network: host\n"
	code, _, errs := acpIn(cfg, "exec", "-c", "-", "hi", "--", "/nonexistent/agent")
	assert.Equal(t, 125, code, errs)
	assert.Contains(t, errs, "spawn failed")
	assert.Contains(t, errs, "/nonexistent/agent")
	left, err := os.ReadDir(tmp)
	require.NoError(t, err)
	assert.Empty(t, left)
}

// exec is one turn with the agent's outcome as the exit status, and it
// leaves no session directory or host behind.
func TestExec(t *testing.T) {
	cfg := filepath.Join(shortDir(t), "c.yaml")
	require.NoError(t, os.WriteFile(cfg, []byte("env: [FOO=bar]\n"), 0o600))
	for name, tc := range map[string]struct {
		args   []string // before the agent command
		stdin  string
		agent  []string
		code   int
		stdout string // a regexp
		stderr string
	}{
		"text":            {args: []string{"--format", "quiet", "hi", "there"}, stdout: `^sess-\d+> hi there\n$`},
		"stdin":           {args: []string{"--format", "quiet"}, stdin: "from stdin", stdout: `^sess-\d+> from stdin\n$`},
		"config":          {args: []string{"-c", cfg, "--format", "quiet", "env", "FOO"}, stdout: `^FOO=bar\n$`},
		"stop reason":     {args: []string{"stop", "max_tokens"}, code: 1, stderr: "the turn stopped: max_tokens"},
		"agent error":     {args: []string{"fail"}, code: 1, stderr: "boom"},
		"agent dies":      {args: []string{"die"}, code: 1, stderr: "the host stopped: agent exited (status 7)"},
		"agent quits":     {args: []string{"hi"}, agent: fakeAgentCommand(agentQuits), code: 1, stderr: "the host stopped: agent exited (status 3)"},
		"no such agent":   {args: []string{"hi"}, agent: []string{"--", "/nonexistent/agent"}, code: 125, stderr: "/nonexistent/agent"},
		"no agent":        {args: []string{"hi"}, agent: []string{}, code: 2, stderr: "no agent command"},
		"bad format":      {args: []string{"--format", "xml", "hi"}, code: 2, stderr: "--format"},
		"no prompt text":  {args: []string{}, code: 2, stderr: "empty prompt"},
		"config is stdin": {args: []string{"-c", "-"}, code: 2, stderr: "no prompt text"},
	} {
		t.Run(name, func(t *testing.T) {
			tmp := shortDir(t)
			t.Setenv("TMPDIR", tmp)
			stopHosts(t, filepath.Join(tmp, "*"))
			agent := tc.agent
			if agent == nil {
				agent = fakeAgentCommand()
			}
			code, out, errs := acpIn(tc.stdin, append(append([]string{"exec"}, tc.args...), agent...)...)
			assert.Equal(t, tc.code, code, errs)
			if tc.stdout != "" {
				assert.Regexp(t, tc.stdout, out)
			}
			if tc.stderr != "" {
				assert.Contains(t, errs, tc.stderr)
			}
			left, err := os.ReadDir(tmp)
			require.NoError(t, err)
			assert.Empty(t, left, "the temporary session is removed")
		})
	}
}
