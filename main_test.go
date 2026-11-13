package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/gfx-labs/acphost/pkg/acphost"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	acphost.MaybeChild()
	maybeFakeAgent()
	// A queued --no-wait prompt is sent by this binary re-exec'd as acpc.
	if len(os.Args) > 1 && os.Args[1] == "prompt" {
		os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

func TestParseConfig(t *testing.T) {
	var c Config
	require.NoError(t, parseConfig([]byte(`
sandbox:
  ro: [/nix, {src: /etc, dst: /host-etc}]
  rw: [/tmp/w]
  network: pasta
  deny: [10.0.0.0/8]
  dns: {forward: 169.254.1.1}
user: {uid: 1000, gid: 1000, map_uid: 100000, map_gid: 100000}
cgroup: {parent: /sys/fs/cgroup/x, pids_max: 64}
env: [A=b]
`), &c))
	assert.Equal(t, "/host-etc", c.Sandbox.RO[1].Dst)
	assert.Equal(t, "169.254.1.1", c.Sandbox.DNS.Forward)
	assert.Equal(t, 100000, c.User.MapUID)
	assert.Equal(t, int64(64), c.Cgroup.PidsMax)

	for _, bad := range []string{"sandbox: {networ: none}", "nope: 1", "user: {uid: 1, extra: 2}"} {
		var c Config
		assert.Error(t, parseConfig([]byte(bad), &c), bad)
	}
}

func needBwrap(t *testing.T) {
	t.Helper()
	p, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("bwrap not on PATH")
	}
	if err := exec.Command(p, "--ro-bind", "/", "/", "true").Run(); err != nil {
		t.Skipf("bwrap does not work here: %v", err)
	}
}

// buildCLI builds the binary, for tests that need a real process to signal.
func buildCLI(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "acpc")
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	require.NoError(t, err, string(out))
	return bin
}
