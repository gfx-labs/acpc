package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elee1766/acphost/pkg/acphost"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	acphost.MaybeChild()
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

func TestBuildOverrides(t *testing.T) {
	o, err := parse([]string{"run", "--network", "none", "--ro", "/a:/b", "--rw", "/w", "--join", "net=/proc/1/ns/net", "--", "true"}, &bytes.Buffer{})
	require.NoError(t, err)
	s, err := build(o, nil)
	require.NoError(t, err)
	assert.Equal(t, "none", s.Sandbox.Network)
	assert.Equal(t, "/b", s.Sandbox.RO[0].Dst)
	assert.Equal(t, "/w", s.Sandbox.RW[0].Src)
	assert.Equal(t, "/proc/1/ns/net", s.Join["net"])
	assert.Equal(t, []string{"true"}, append([]string{s.Command}, s.Args...))
}

func TestRunExitCodeNoIsolation(t *testing.T) {
	var out bytes.Buffer
	code := run([]string{"run", "--dir", t.TempDir(), "--", "sh", "-c", "echo hi; exit 3"}, nil, &out, &bytes.Buffer{})
	assert.Equal(t, 3, code)
	assert.Equal(t, "hi\n", out.String())
}

// buildCLI builds the binary, so the e2e run goes through a real main
// and its helper re-execs.
func buildCLI(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "acphost")
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	require.NoError(t, err, string(out))
	return bin
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

func TestE2ERunSandbox(t *testing.T) {
	needBwrap(t)
	bin := buildCLI(t)
	work := t.TempDir()
	secret := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(secret, "key"), []byte("x"), 0o600))
	ro := ""
	for _, p := range []string{"/nix", "/bin", "/usr", "/lib", "/lib64", "/etc"} {
		ro += "    - {src: " + p + ", skip_missing: true}\n"
	}
	cfg := "sandbox:\n  ro:\n" + ro + "    - " + secret + "\n  rw: [" + work + "]\n  hide: [" + secret + "]\n  network: none\n  env_keep: [PATH]\n"
	script := `test -e ` + secret + `/key && echo visible; touch /etc/x 2>/dev/null && echo writable; ` +
		`touch ok; cat /sys/class/net/*/operstate 2>/dev/null | wc -l; exit 5`
	cmd := exec.Command(bin, "run", "-c", "-", "--dir", work, "--", "sh", "-c", script)
	cmd.Stdin = strings.NewReader(cfg)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var ee *exec.ExitError
	require.ErrorAs(t, err, &ee, stderr.String())
	assert.Equal(t, 5, ee.ExitCode(), stderr.String())
	assert.NotContains(t, stdout.String(), "visible", "hidden path")
	assert.NotContains(t, stdout.String(), "writable", "read-only bind")
	_, err = os.Stat(filepath.Join(work, "ok"))
	assert.NoError(t, err, "rw dir")

	// --print runs nothing and shows the bwrap argv.
	cmd = exec.Command(bin, "run", "-c", "-", "--dir", work, "--print", "--", "true")
	cmd.Stdin = strings.NewReader(cfg)
	out, err := cmd.Output()
	require.NoError(t, err)
	assert.Contains(t, string(out), "--unshare-net")
}
