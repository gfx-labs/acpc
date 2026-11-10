// Command acphost runs acphost by hand, to debug and test sandboxing and
// isolation without jobshop (#165).
//
//	acphost run  -c sandbox.yaml [flags] -- CMD ARGS...
//	acphost host -c sandbox.yaml --dir SESSIONDIR [--lease 30m] -- CMD ARGS...
//	acphost start [--host] -c sandbox.yaml --dir DIR -- CMD ARGS...
//	acphost status|stop|wait --dir DIR
//
// run executes one command attached to the terminal and exits with its
// status. host writes spec.json into SESSIONDIR and runs the ACP host
// (acphost.Main) in the foreground. start runs a command, or with --host
// an ACP host, detached; status, stop and wait use its dir. See
// docs/notes/165-cli.md, 200-cli-detached.md and 211-cli-host-detached.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/elee1766/acphost/pkg/acphost"
	"github.com/elee1766/acphost/pkg/cgroup"
	"github.com/elee1766/acphost/pkg/proc"
	"github.com/elee1766/acphost/pkg/sandbox"
	"github.com/elee1766/acphost/pkg/userns"
)

func main() {
	acphost.MaybeChild()
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

const usage = `usage:
  acphost run  -c FILE [flags] -- CMD ARGS...
  acphost host -c FILE --dir SESSIONDIR [--lease 30m] [flags] -- CMD ARGS...
  acphost start -c FILE --dir STATEDIR [--grace 10s] [flags] -- CMD ARGS...
  acphost start --host -c FILE --dir SESSIONDIR [--lease 30m] [flags] -- CMD ARGS...
  acphost status --dir STATEDIR
  acphost stop --dir STATEDIR [--grace 30s]
  acphost wait --dir STATEDIR
`

// multi collects a repeated flag.
type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(s string) error { *m = append(*m, s); return nil }

type opts struct {
	mode                  string
	config, dir, network  string
	lease, grace          time.Duration
	ro, rw, join          multi
	listen                string
	tlsCert, tlsKey       string
	tlsClientCA           string
	tlsAllow              multi
	print, check, verbose bool
	detachHost            bool // start --host: run an ACP host detached
	argv                  []string
}

func parse(args []string, stderr io.Writer) (opts, error) {
	var o opts
	if len(args) == 0 || (args[0] != "run" && args[0] != "host") {
		return o, errUsage
	}
	o.mode = args[0]
	fs := flag.NewFlagSet("acphost "+o.mode, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.config, "c", "", "config file (YAML or JSON), - for stdin")
	fs.StringVar(&o.dir, "dir", "", "working dir (run) or session dir (host)")
	fs.StringVar(&o.network, "network", "", "override sandbox network: host, none, pasta")
	fs.DurationVar(&o.lease, "lease", 30*time.Minute, "host: exit after no controller for this long")
	fs.DurationVar(&o.grace, "grace", 10*time.Second, "start: SIGTERM to SIGKILL delay on stop")
	fs.Var(&o.ro, "ro", "add a read-only bind SRC[:DST] (repeatable)")
	fs.Var(&o.rw, "rw", "add a read-write bind SRC[:DST] (repeatable)")
	fs.Var(&o.join, "join", "join a namespace TYPE=PATH (repeatable)")
	fs.StringVar(&o.listen, "listen", "", "host: also listen on this TCP HOST:PORT (an explicit host; port 0 picks one)")
	fs.StringVar(&o.tlsCert, "tls-cert", "", "host: server certificate (PEM) for the TCP listener")
	fs.StringVar(&o.tlsKey, "tls-key", "", "host: server key (PEM) for the TCP listener")
	fs.StringVar(&o.tlsClientCA, "tls-client-ca", "", "host: CA bundle (PEM) client certificates must chain to")
	fs.Var(&o.tlsAllow, "tls-allow", "host: allow only clients with this DNS or URI SAN (repeatable)")
	fs.BoolVar(&o.print, "print", false, "print the argv and isolation record, run nothing")
	fs.BoolVar(&o.check, "check", false, "run the pre-spawn checks only")
	fs.BoolVar(&o.verbose, "v", false, "print the isolation record to stderr after the run")
	fs.BoolVar(&o.detachHost, "host", false, "start: run an ACP host detached (--dir is the session dir)")
	if err := fs.Parse(args[1:]); err != nil {
		return o, err
	}
	o.argv = fs.Args()
	if len(o.argv) == 0 {
		return o, fmt.Errorf("no command: %w", errUsage)
	}
	if o.mode == "host" && o.dir == "" {
		return o, errors.New("host needs --dir")
	}
	return o, nil
}

// build turns the config and flag overrides into a Spec.
func build(o opts, stdin io.Reader) (acphost.Spec, error) {
	c, err := readConfig(o.config, stdin)
	if err != nil {
		return acphost.Spec{}, err
	}
	if o.network != "" || len(o.ro) > 0 || len(o.rw) > 0 {
		if c.Sandbox == nil {
			c.Sandbox = &sandbox.Sandbox{}
		}
		if o.network != "" {
			c.Sandbox.Network = o.network
		}
		for _, m := range o.ro {
			c.Sandbox.RO = append(c.Sandbox.RO, parseMount(m))
		}
		for _, m := range o.rw {
			c.Sandbox.RW = append(c.Sandbox.RW, parseMount(m))
		}
	}
	for _, j := range o.join {
		t, p, ok := strings.Cut(j, "=")
		if !ok {
			return acphost.Spec{}, fmt.Errorf("--join %q: want TYPE=PATH", j)
		}
		if c.Join == nil {
			c.Join = map[string]string{}
		}
		c.Join[t] = p
	}
	dir := o.dir
	if dir == "" {
		if dir, err = os.Getwd(); err != nil {
			return acphost.Spec{}, err
		}
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return acphost.Spec{}, err
	}
	s := acphost.Spec{
		Protocol: acphost.Protocol, Session: "cli", Command: o.argv[0], Args: o.argv[1:],
		Env: c.Env, Dir: dir, LeaseMS: o.lease.Milliseconds(),
		Sandbox: c.Sandbox, User: c.User, Cgroup: c.Cgroup, Join: c.Join,
		Listen: c.Listen, TLS: c.TLS,
	}
	if o.listen != "" {
		s.Listen = o.listen
	}
	if o.tlsCert != "" || o.tlsKey != "" || o.tlsClientCA != "" || len(o.tlsAllow) > 0 {
		t := acphost.ListenTLS{}
		if s.TLS != nil {
			t = *s.TLS
		}
		if o.tlsCert != "" {
			t.Cert = o.tlsCert
		}
		if o.tlsKey != "" {
			t.Key = o.tlsKey
		}
		if o.tlsClientCA != "" {
			t.ClientCA = o.tlsClientCA
		}
		t.Allow = append(slices.Clone(t.Allow), o.tlsAllow...)
		s.TLS = &t
	}
	if o.mode != "host" && (s.Listen != "" || s.TLS != nil) {
		return acphost.Spec{}, errors.New("listen and tls are for a host (host, or start --host)")
	}
	if o.mode == "host" {
		s.Session = filepath.Base(dir)
		// The agent runs in the caller's cwd; SESSIONDIR holds host state.
		if s.Dir, err = os.Getwd(); err != nil {
			return acphost.Spec{}, err
		}
	} else {
		s.Session = fmt.Sprintf("acphost-run-%d", os.Getpid())
	}
	return s, nil
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 && isDetached(args[0]) {
		return detached(args, stdin, stdout, stderr)
	}
	o, err := parse(args, stderr)
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			say(stderr, "acphost:", err)
		}
		if errors.Is(err, errUsage) {
			sayf(stderr, "%s", usage)
		}
		return 2
	}
	if o.detachHost {
		say(stderr, "acphost: --host is only for start (host already runs a host)")
		return 2
	}
	s, err := build(o, stdin)
	if err != nil {
		say(stderr, "acphost:", err)
		return 2
	}
	switch {
	case o.check:
		return check(s, stdout, stderr)
	case o.print:
		return printPlan(s, stdout, stderr)
	case o.mode == "host":
		return host(o, s, stderr)
	}
	if err := acphost.CheckSpawn(s, false); err != nil {
		say(stderr, "acphost:", err)
		return 125
	}
	cmdIn := stdin
	if o.config == "-" {
		cmdIn = nil // stdin held the config
	}
	p, err := proc.Start(context.Background(), procSpec(s), proc.Stdin(cmdIn), proc.Stdout(stdout), proc.Stderr(stderr))
	if err != nil {
		say(stderr, "acphost:", err)
		return 125
	}
	ex, err := p.Wait()
	if o.verbose {
		writeJSON(stderr, p.Isolation())
	}
	if err != nil {
		say(stderr, "acphost:", err)
		return 125
	}
	return exitCode(ex, stderr)
}

func procSpec(s acphost.Spec) proc.Spec {
	return proc.Spec{
		Command: s.Command, Args: s.Args, Dir: s.Dir, Env: s.Env,
		Sandbox: s.Sandbox, Cgroup: s.Cgroup, User: s.User, Join: s.Join, Name: s.Session,
	}
}

// check runs CheckSpawn and the host preflight and prints each result.
func check(s acphost.Spec, stdout, stderr io.Writer) int {
	code := 0
	if err := acphost.CheckSpawn(s, false); err != nil {
		say(stdout, "FAIL spawn:", err)
		describePaths(stdout, err.Error())
		code = 1
	} else {
		say(stdout, "ok   spawn")
	}
	rep, err := acphost.Preflight(s)
	if err != nil {
		say(stderr, "acphost: preflight:", err)
		return 1
	}
	if len(rep.Checks) > 0 {
		say(stdout, rep.String())
	}
	if !rep.OK() {
		code = 1
	}
	return code
}

// describePaths prints mode and owner for each absolute path in msg, so
// a permission failure shows what is wrong with the file.
func describePaths(w io.Writer, msg string) {
	for _, f := range strings.Fields(msg) {
		f = strings.Trim(f, `"':,()`)
		if !strings.HasPrefix(f, "/") {
			continue
		}
		st, err := os.Lstat(f)
		if err != nil {
			sayf(w, "     %s: %v\n", f, err)
			continue
		}
		sayf(w, "     %s: mode %s owner %s\n", f, st.Mode(), owner(st))
	}
}

// printPlan prints the bwrap/pasta argv and newuidmap lines without
// running anything.
func printPlan(s acphost.Spec, stdout, stderr io.Writer) int {
	plan := struct {
		Argv   []string             `json:"argv"`
		Join   []string             `json:"join,omitempty"`
		User   *userns.User         `json:"user,omitempty"`
		Maps   *acphost.UserApplied `json:"user_maps,omitempty"`
		Cgroup *cgroup.Cgroup       `json:"cgroup,omitempty"`
	}{Argv: append([]string{s.Command}, s.Args...), Join: acphost.JoinPlan(s), User: s.User, Cgroup: s.Cgroup}
	if s.Sandbox != nil {
		argv, err := sandbox.Args(*s.Sandbox, s.Command, s.Args, s.Dir)
		if err != nil {
			say(stderr, "acphost:", err)
			return 1
		}
		plan.Argv = argv
	}
	if s.User != nil {
		m, err := userMaps(*s.User)
		if err != nil {
			say(stderr, "acphost:", err)
			return 1
		}
		plan.Maps = m
	}
	writeJSON(stdout, plan)
	return 0
}

func host(o opts, s acphost.Spec, stderr io.Writer) int {
	if err := os.MkdirAll(o.dir, 0o700); err != nil {
		say(stderr, "acphost:", err)
		return 1
	}
	if err := acphost.CheckSpawn(s, false, acphost.WithHostDir(o.dir)); err != nil {
		say(stderr, "acphost:", err)
		return 125
	}
	if err := acphost.WriteSpec(o.dir, s); err != nil {
		say(stderr, "acphost:", err)
		return 1
	}
	say(stderr, "acphost: socket", filepath.Join(o.dir, acphost.ACPSocket))
	var mainOpts []acphost.Option
	if s.Listen != "" {
		// The bound address is known once the host listens; port 0 is
		// only resolved then.
		mainOpts = append(mainOpts, acphost.WithListening(func(l acphost.ListenInfo) { sayListen(stderr, l) }))
	}
	if err := acphost.Main(o.dir, mainOpts...); err != nil {
		say(stderr, "acphost:", err)
		return 1
	}
	if o.verbose {
		if iso, ok, _ := acphost.ReadIsolation(o.dir); ok {
			writeJSON(stderr, iso)
		}
	}
	return 0
}

// sayListen reports the TCP listener: its bound address and whether mutual
// TLS is on.
func sayListen(w io.Writer, l acphost.ListenInfo) {
	if l.TLS {
		say(w, "acphost: listening", l.Addr, "(mutual tls)")
		return
	}
	say(w, "acphost: listening", l.Addr, "(no tls, no authentication)")
}

func writeJSON(w io.Writer, v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	_, _ = w.Write(append(b, '\n'))
}

// say and sayf write CLI output; a failed write to the terminal has
// nowhere better to be reported.
func say(w io.Writer, a ...any) { _, _ = fmt.Fprintln(w, a...) }

func sayf(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }

// errUsage is returned for a bad command line; run prints usage.
var errUsage = errors.New("bad usage")
