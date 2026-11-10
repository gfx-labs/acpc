package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gfx-labs/acphost/pkg/acphost"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildListenFlags(t *testing.T) {
	o, err := parse([]string{
		"host", "--dir", t.TempDir(), "--listen", "0.0.0.0:8443",
		"--tls-cert", "/c.crt", "--tls-key", "/c.key", "--tls-client-ca", "/ca.crt",
		"--tls-allow", "ctl.example", "--tls-allow", "spiffe://x/y", "--", "true",
	}, &bytes.Buffer{})
	require.NoError(t, err)
	s, err := build(o, nil)
	require.NoError(t, err)
	assert.Equal(t, "0.0.0.0:8443", s.Listen)
	assert.Equal(t, &acphost.ListenTLS{
		Cert: "/c.crt", Key: "/c.key", ClientCA: "/ca.crt", Allow: []string{"ctl.example", "spiffe://x/y"},
	}, s.TLS)

	// No flags: no listener.
	o, err = parse([]string{"host", "--dir", t.TempDir(), "--", "true"}, &bytes.Buffer{})
	require.NoError(t, err)
	s, err = build(o, nil)
	require.NoError(t, err)
	assert.Empty(t, s.Listen)
	assert.Nil(t, s.TLS)
}

// The config file takes the same fields, and flags override it.
func TestBuildListenConfig(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "c.yaml")
	require.NoError(t, os.WriteFile(cfg, []byte(`
listen: 127.0.0.1:9000
tls: {cert: /a.crt, key: /a.key, client_ca: /a-ca.crt, allow: [one.example]}
`), 0o600))
	o, err := parse([]string{"host", "-c", cfg, "--dir", t.TempDir(), "--listen", "127.0.0.1:9001", "--tls-allow", "two.example", "--", "true"}, &bytes.Buffer{})
	require.NoError(t, err)
	s, err := build(o, nil)
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:9001", s.Listen)
	assert.Equal(t, &acphost.ListenTLS{
		Cert: "/a.crt", Key: "/a.key", ClientCA: "/a-ca.crt", Allow: []string{"one.example", "two.example"},
	}, s.TLS)
}

// run and start (without --host) have no listener to configure.
func TestListenOnlyForHost(t *testing.T) {
	var errb bytes.Buffer
	assert.Equal(t, 2, run([]string{"run", "--listen", "127.0.0.1:0", "--", "true"}, nil, &bytes.Buffer{}, &errb))
	assert.Contains(t, errb.String(), "for a host")
}

func TestHostListenRejected(t *testing.T) {
	var errb bytes.Buffer
	dir := t.TempDir()
	// An implied host (no host part) is a permanent spawn error: 125.
	assert.Equal(t, 125, run([]string{"start", "--host", "--dir", dir, "--listen", ":8443", "--", "true"}, nil, &bytes.Buffer{}, &errb))
	assert.Contains(t, errb.String(), "explicit host")
	// A TLS block without a listen address too.
	errb.Reset()
	assert.Equal(t, 125, run([]string{"start", "--host", "--dir", t.TempDir(), "--tls-cert", "/c", "--", "true"}, nil, &bytes.Buffer{}, &errb))
	assert.Contains(t, errb.String(), "needs spec listen")
}

// start --host --listen prints the bound address and TLS state next to the
// socket path, and the address works.
func TestStartHostListenPrintsAddress(t *testing.T) {
	dir := t.TempDir()
	agent := filepath.Join(t.TempDir(), "agent")
	require.NoError(t, os.WriteFile(agent, []byte("#!/bin/sh\nexec sleep 60\n"), 0o700))
	var out, errb bytes.Buffer
	require.Equal(t, 0, run([]string{"start", "--host", "--dir", dir, "--listen", "127.0.0.1:0", "--", agent}, nil, &out, &errb), errb.String())
	t.Cleanup(func() {
		run([]string{"stop", "--dir", dir, "--grace", "20s"}, nil, &bytes.Buffer{}, &bytes.Buffer{})
	})

	info, ok := acphost.ReadListen(dir)
	require.True(t, ok)
	assert.Contains(t, errb.String(), acphost.SocketPath(dir))
	assert.Contains(t, errb.String(), "listening "+info.Addr+" (no tls, no authentication)")
	assert.False(t, strings.Contains(errb.String(), "mutual tls"))

	nc, _, _, err := acphost.ConnectTCP(t.Context(), info.Addr)
	require.NoError(t, err)
	_ = nc.Close()

	spec, err := acphost.ReadSpec(dir)
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:0", spec.Listen)
}
