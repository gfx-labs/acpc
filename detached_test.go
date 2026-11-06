package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func statusOf(t *testing.T, dir string) statusOut {
	t.Helper()
	var out bytes.Buffer
	require.Equal(t, 0, run([]string{"status", "--dir", dir}, nil, &out, &bytes.Buffer{}))
	var st statusOut
	require.NoError(t, json.Unmarshal(out.Bytes(), &st), out.String())
	return st
}

// The command outlives the CLI process that started it; status, wait
// and stop work from another process.
func TestDetachedStartOutlivesCLI(t *testing.T) {
	bin := buildCLI(t)
	dir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "go")
	cmd := exec.Command(bin, "start", "--dir", dir, "--", "sh", "-c",
		"while ! test -e "+marker+"; do sleep 0.05; done; echo out; exit 7")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output() // returns once the CLI exits
	require.NoError(t, err, stderr.String())
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	require.NoError(t, err)

	st := statusOf(t, dir)
	assert.Equal(t, "running", st.State)
	assert.Equal(t, pid, st.Pid)

	require.NoError(t, os.WriteFile(marker, nil, 0o600))
	assert.Equal(t, 7, run([]string{"wait", "--dir", dir}, nil, &bytes.Buffer{}, &bytes.Buffer{}))
	st = statusOf(t, dir)
	assert.Equal(t, "exited", st.State)
	require.NotNil(t, st.Exit)
	assert.Equal(t, 7, st.Exit.ExitCode)
	b, err := os.ReadFile(filepath.Join(dir, "output.log"))
	require.NoError(t, err)
	assert.Equal(t, "out\n", string(b))
}

func TestDetachedStop(t *testing.T) {
	dir := t.TempDir()
	var out, errb bytes.Buffer
	require.Equal(t, 0, run([]string{"start", "--dir", dir, "--grace", "5s", "--", "sleep", "60"}, nil, &out, &errb), errb.String())
	assert.Equal(t, "running", statusOf(t, dir).State)

	out.Reset()
	require.Equal(t, 0, run([]string{"stop", "--dir", dir, "--grace", "20s"}, nil, &out, &errb), errb.String())
	assert.Contains(t, out.String(), `"signal"`)
	// sleep dies of SIGTERM: wait reports 128+15.
	assert.Equal(t, 143, run([]string{"wait", "--dir", dir}, nil, &bytes.Buffer{}, &bytes.Buffer{}))
	assert.Equal(t, "exited", statusOf(t, dir).State)
}

func TestDetachedUsage(t *testing.T) {
	assert.Equal(t, 2, run([]string{"status"}, nil, &bytes.Buffer{}, &bytes.Buffer{}))
	assert.Equal(t, 2, run([]string{"start", "--", "true"}, nil, &bytes.Buffer{}, &bytes.Buffer{}))
	assert.Equal(t, 1, run([]string{"wait", "--dir", t.TempDir()}, nil, &bytes.Buffer{}, &bytes.Buffer{}))
	assert.Equal(t, 125, run([]string{"start", "--dir", t.TempDir(), "--", "/nonexistent/x"}, nil, &bytes.Buffer{}, &bytes.Buffer{}))
}
