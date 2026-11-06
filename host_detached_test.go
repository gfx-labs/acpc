package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/elee1766/acphost/pkg/acphost"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hostAgent writes an agent that exits 3 once the file release exists.
func hostAgent(t *testing.T, release string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "agent")
	require.NoError(t, os.WriteFile(p, []byte("#!/bin/sh\nwhile ! test -e "+release+"; do sleep 0.05; done\nexit 3\n"), 0o700))
	return p
}

// start --host returns while the host keeps running, a client can still
// connect to its socket, and status, wait and stop work from other
// processes (#211).
func TestStartHostOutlivesCLI(t *testing.T) {
	bin := buildCLI(t)
	dir := t.TempDir()
	release := filepath.Join(t.TempDir(), "go")
	cmd := exec.Command(bin, "start", "--host", "--dir", dir, "--", hostAgent(t, release))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output() // returns once the CLI exits
	require.NoError(t, err, stderr.String())
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	require.NoError(t, err)
	assert.Contains(t, stderr.String(), acphost.SocketPath(dir))

	// The CLI is gone; the host is up and answers.
	st := statusOf(t, dir)
	assert.Equal(t, "running", st.State)
	assert.Equal(t, pid, st.Pid)
	nc, _, w, err := acphost.Connect(t.Context(), dir)
	require.NoError(t, err)
	_ = nc.Close()
	assert.Equal(t, pid, w.Pid)
	spec, err := acphost.ReadSpec(dir)
	require.NoError(t, err)
	assert.Equal(t, filepath.Base(dir), spec.Session)

	// Let the agent exit: wait returns its exit record.
	require.NoError(t, os.WriteFile(release, nil, 0o600))
	var wout bytes.Buffer
	require.Equal(t, 3, run([]string{"wait", "--dir", dir}, nil, &wout, &bytes.Buffer{}))
	st = statusOf(t, dir)
	assert.Equal(t, "exited", st.State)
	require.NotNil(t, st.Exit)
	assert.Equal(t, 3, st.Exit.ExitCode)
	ex, ok := acphost.ReadExit(dir)
	require.True(t, ok)
	assert.Equal(t, "agent exited", ex.Reason)
}

func TestStartHostStop(t *testing.T) {
	dir := t.TempDir()
	agent := filepath.Join(t.TempDir(), "agent")
	require.NoError(t, os.WriteFile(agent, []byte("#!/bin/sh\nexec sleep 60\n"), 0o700))
	var out, errb bytes.Buffer
	require.Equal(t, 0, run([]string{"start", "--host", "--dir", dir, "--", agent}, nil, &out, &errb), errb.String())
	assert.Equal(t, "running", statusOf(t, dir).State)
	_, _, _, err := acphost.Connect(t.Context(), dir)
	require.NoError(t, err)

	out.Reset()
	start := time.Now()
	require.Equal(t, 0, run([]string{"stop", "--dir", dir, "--grace", "20s"}, nil, &out, &errb), errb.String())
	assert.Less(t, time.Since(start), 15*time.Second)
	assert.Contains(t, out.String(), `"signal"`)
	assert.Equal(t, "exited", statusOf(t, dir).State)
	assert.False(t, acphost.Alive(dir))
	// A second start on the finished dir works.
	require.Equal(t, 0, run([]string{"start", "--host", "--dir", dir, "--", agent}, nil, &out, &errb), errb.String())
	require.Equal(t, 0, run([]string{"stop", "--dir", dir, "--grace", "20s"}, nil, &out, &errb), errb.String())
}

func TestStartHostErrors(t *testing.T) {
	// A running host refuses a second start.
	dir := t.TempDir()
	agent := filepath.Join(t.TempDir(), "agent")
	require.NoError(t, os.WriteFile(agent, []byte("#!/bin/sh\nexec sleep 60\n"), 0o700))
	var errb bytes.Buffer
	require.Equal(t, 0, run([]string{"start", "--host", "--dir", dir, "--", agent}, nil, &bytes.Buffer{}, &errb), errb.String())
	errb.Reset()
	assert.Equal(t, 125, run([]string{"start", "--host", "--dir", dir, "--", agent}, nil, &bytes.Buffer{}, &errb))
	assert.Contains(t, errb.String(), "already running")
	require.Equal(t, 0, run([]string{"stop", "--dir", dir, "--grace", "20s"}, nil, &bytes.Buffer{}, &bytes.Buffer{}))

	// A missing command is a spawn failure: 125, nothing left running.
	errb.Reset()
	d2 := t.TempDir()
	assert.Equal(t, 125, run([]string{"start", "--host", "--dir", d2, "--", "/nonexistent/x"}, nil, &bytes.Buffer{}, &errb))
	assert.Contains(t, errb.String(), "/nonexistent/x")
	assert.False(t, acphost.Alive(d2))

	// --host belongs to start, and needs --dir.
	assert.Equal(t, 2, run([]string{"run", "--host", "--", "true"}, nil, &bytes.Buffer{}, &bytes.Buffer{}))
	assert.Equal(t, 2, run([]string{"start", "--host", "--", "true"}, nil, &bytes.Buffer{}, &bytes.Buffer{}))
}
