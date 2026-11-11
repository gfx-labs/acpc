// Command acphost runs acphost by hand: one command or an ACP host under
// its isolation (#165), and named agent sessions to prompt from a shell.
//
//	acphost run  -c sandbox.yaml [flags] -- CMD ARGS...
//	acphost host -c sandbox.yaml --dir SESSIONDIR [--lease 30m] -- CMD ARGS...
//	acphost start [--host] -c sandbox.yaml --dir DIR -- CMD ARGS...
//	acphost status|stop|wait --dir DIR | -s NAME
//
//	acphost new    -s NAME [-c sandbox.yaml] -- CMD ARGS...
//	acphost prompt -s NAME TEXT...
//	acphost exec   [-c sandbox.yaml] TEXT -- CMD ARGS...
//	acphost attach|cancel|rm -s NAME
//	acphost ls
//
// run executes one command attached to the terminal and exits with its
// status. host writes spec.json into SESSIONDIR and runs the ACP host
// (acphost.Main) in the foreground. start runs a command, or with --host
// an ACP host, detached; status, stop and wait use its dir. new starts a
// detached host for an ACP agent and keeps it under a name; prompt sends
// it a turn. See docs/notes/165-cli.md, 200-cli-detached.md,
// 211-cli-host-detached.md and 290-cli-sessions.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gfx-labs/acphost/pkg/acphost"
	"github.com/gfx-labs/acphost/pkg/cgroup"
	"github.com/gfx-labs/acphost/pkg/proc"
	"github.com/gfx-labs/acphost/pkg/sandbox"
	"github.com/gfx-labs/acphost/pkg/userns"
	"github.com/urfave/cli/v3"
)

func main() {
	acphost.MaybeChild()
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return newApp(stdin, stdout, stderr).run(args)
}

// isolation is the config file and the flags that override it.
type isolation struct {
	config, network string
	ro, rw, join    []string
}

func (i isolation) set() bool {
	return i.config != "" || i.network != "" || len(i.ro) > 0 || len(i.rw) > 0 || len(i.join) > 0
}

func isolationFrom(cmd *cli.Command) isolation {
	return isolation{
		config: cmd.String("config"), network: cmd.String("network"),
		ro: cmd.StringSlice("ro"), rw: cmd.StringSlice("rw"), join: cmd.StringSlice("join"),
	}
}

type opts struct {
	isolation
	mode                  string // run or host
	dir                   string
	lease, grace          time.Duration
	listen                string
	tlsCert, tlsKey       string
	tlsClientCA           string
	tlsAllow              []string
	print, check, verbose bool
	detachHost            bool // start --host: run an ACP host detached
	argv                  []string
}

// load reads the config file and applies the flag overrides.
func (i isolation) load(stdin io.Reader) (Config, error) {
	c, err := readConfig(i.config, stdin)
	if err != nil {
		return c, err
	}
	if i.network != "" || len(i.ro) > 0 || len(i.rw) > 0 {
		if c.Sandbox == nil {
			c.Sandbox = &sandbox.Sandbox{}
		}
		if i.network != "" {
			c.Sandbox.Network = i.network
		}
		for _, m := range i.ro {
			c.Sandbox.RO = append(c.Sandbox.RO, parseMount(m))
		}
		for _, m := range i.rw {
			c.Sandbox.RW = append(c.Sandbox.RW, parseMount(m))
		}
	}
	for _, j := range i.join {
		t, p, ok := strings.Cut(j, "=")
		if !ok {
			return c, fmt.Errorf("--join %q: want TYPE=PATH", j)
		}
		if c.Join == nil {
			c.Join = map[string]string{}
		}
		c.Join[t] = p
	}
	return c, nil
}

// opts reads the flags of run, host and start. start parses as run, so it
// takes the same flags.
func (a *app) opts(cmd *cli.Command) (opts, error) {
	o := opts{
		isolation: isolationFrom(cmd), mode: cmd.Name, dir: cmd.String("dir"),
		lease: cmd.Duration("lease"), grace: cmd.Duration("grace"),
		listen: cmd.String("listen"), tlsCert: cmd.String("tls-cert"), tlsKey: cmd.String("tls-key"),
		tlsClientCA: cmd.String("tls-client-ca"), tlsAllow: cmd.StringSlice("tls-allow"),
		print: cmd.Bool("print"), check: cmd.Bool("check"), verbose: cmd.Bool("verbose"),
		detachHost: cmd.Bool("host"), argv: a.argv(cmd),
	}
	if o.mode == "start" {
		o.mode = "run"
	}
	if len(o.argv) == 0 {
		return o, errors.New("no command (give it after --)")
	}
	if o.mode == "host" && o.dir == "" {
		return o, errors.New("host needs --dir")
	}
	return o, nil
}

// build turns the config and flag overrides into a Spec.
func build(o opts, stdin io.Reader) (acphost.Spec, error) {
	c, err := o.load(stdin)
	if err != nil {
		return acphost.Spec{}, err
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

// cmdRun is run and host.
func (a *app) cmdRun(_ context.Context, cmd *cli.Command) error {
	o, err := a.opts(cmd)
	if err != nil {
		return err
	}
	if o.detachHost {
		return errors.New("--host is only for start (host already runs a host)")
	}
	s, err := build(o, a.stdin)
	if err != nil {
		return err
	}
	switch {
	case o.check:
		return exit(check(s, a.stdout, a.stderr))
	case o.print:
		return exit(printPlan(s, a.stdout, a.stderr))
	case o.mode == "host":
		return exit(host(o, s, a.stderr))
	}
	if err := acphost.CheckSpawn(s, false); err != nil {
		return a.fail(125, err)
	}
	cmdIn := a.stdin
	if o.config == "-" {
		cmdIn = nil // stdin held the config
	}
	p, err := proc.Start(context.Background(), procSpec(s), proc.Stdin(cmdIn), proc.Stdout(a.stdout), proc.Stderr(a.stderr))
	if err != nil {
		return a.fail(125, err)
	}
	ex, err := p.Wait()
	if o.verbose {
		writeJSON(a.stderr, p.Isolation())
	}
	if err != nil {
		return a.fail(125, err)
	}
	return exit(exitCode(ex, a.stderr))
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
