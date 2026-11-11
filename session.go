package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gfx-labs/acphost/pkg/acphost"
	"github.com/gfx-labs/acphost/pkg/proc"
	"github.com/urfave/cli/v3"
	"golang.org/x/sys/unix"
)

// A session is a host's session directory plus two files of this client:
// client.json and client.lock. Named sessions live under
// $XDG_STATE_HOME/acphost/sessions/<cwd key>/<name>, so a name means a
// different session in each working directory. See
// docs/notes/290-cli-sessions.md.

const (
	// stateFile holds clientState.
	stateFile = "client.json"
	// lockFile is flocked by the one client using the session and holds
	// its pid. A second connection would replace the first at the host.
	lockFile = "client.lock"
	// defaultSession is the name used without -s.
	defaultSession = "default"
)

// clientState is what this client remembers about a session between
// commands.
type clientState struct {
	// ACPSession is the agent's id for the conversation (session/new).
	ACPSession string `json:"acp_session"`
	// HostPid is the host whose agent has ACPSession open. Another host
	// pid means a new agent process, which has to load it first.
	HostPid int `json:"host_pid"`
	// Seq is the last journal frame shown to the user.
	Seq int64 `json:"seq"`
}

func readState(dir string) (clientState, error) {
	var st clientState
	b, err := os.ReadFile(filepath.Join(dir, stateFile))
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("%s: %w", stateFile, err)
	}
	return st, nil
}

// writeState replaces client.json by rename, so an interrupted command
// does not leave half a file.
func writeState(dir string, st clientState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, stateFile+".tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, stateFile))
}

// target is a session directory and the name it was found by.
type target struct {
	name string // empty when given as --dir
	dir  string
}

func (t target) String() string {
	if t.name != "" {
		return "session " + t.name
	}
	return "session dir " + t.dir
}

// sessionsRoot is where named sessions are kept.
func sessionsRoot() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "acphost", "sessions"), nil
}

// cwdKey names a working directory under the sessions root. It is a short
// hash because the socket path below it has to fit in 107 bytes.
func cwdKey(cwd string) string {
	sum := sha256.Sum256([]byte(cwd))
	return hex.EncodeToString(sum[:6])
}

// checkName accepts a name that is safe as one path element.
func checkName(name string) error {
	ok := name != "" && len(name) <= 32 && name[0] != '.' && name[0] != '-'
	for _, r := range name {
		ok = ok && (r == '.' || r == '_' || r == '-' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'))
	}
	if !ok {
		return fmt.Errorf("bad session name %q: use up to 32 letters, digits, '.', '_' or '-', starting with a letter or digit", name)
	}
	return nil
}

// resolve finds the session directory for -s name or --dir dir. fallback
// is the name used when neither is given; empty makes one of them required.
func resolve(name, dir, fallback string) (target, error) {
	switch {
	case name != "" && dir != "":
		return target{}, errors.New("give -s or --dir, not both")
	case dir != "":
		abs, err := filepath.Abs(dir)
		return target{dir: abs}, err
	case name == "" && fallback == "":
		return target{}, errors.New("needs -s NAME or --dir DIR")
	case name == "":
		name = fallback
	}
	if err := checkName(name); err != nil {
		return target{}, err
	}
	root, err := sessionsRoot()
	if err != nil {
		return target{}, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return target{}, err
	}
	return target{name: name, dir: filepath.Join(root, cwdKey(cwd), name)}, nil
}

// sessionRef is the session a detached command (status, stop, wait) names.
func sessionRef(cmd *cli.Command) (target, error) {
	t, err := resolve(cmd.String("session"), cmd.String("dir"), "")
	if err != nil {
		return t, fmt.Errorf("%s: %w", cmd.Name, err)
	}
	return t, nil
}

// inUseError is a session whose client lock another process holds.
type inUseError struct {
	t   target
	pid int
}

func (e *inUseError) Error() string {
	return fmt.Sprintf("%s is in use by pid %d (a prompt or attach is running)", e.t, e.pid)
}

// lockClient takes the session's client lock and records this pid in it.
// It does not wait: a held lock is an *inUseError.
func lockClient(t target) (unlock func(), err error) {
	f, err := os.OpenFile(filepath.Join(t.dir, lockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		pid := readPid(f)
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, &inUseError{t: t, pid: pid}
		}
		return nil, err
	}
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())), 0)
	return func() { _ = f.Close() }, nil
}

func readPid(f *os.File) int {
	b := make([]byte, 20)
	n, _ := f.ReadAt(b, 0)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b[:n])))
	return pid
}

// lockHolder returns the pid of the client using the session, if any.
func lockHolder(dir string) (pid int, held bool) {
	f, err := os.Open(filepath.Join(dir, lockFile))
	if err != nil {
		return 0, false
	}
	defer func() { _ = f.Close() }()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
		return 0, false
	}
	return readPid(f), true
}

// lockReleased returns a channel that is closed once no client holds the
// session's lock.
func lockReleased(dir string) <-chan struct{} {
	done := make(chan struct{})
	f, err := os.Open(filepath.Join(dir, lockFile))
	if err != nil {
		close(done)
		return done
	}
	go func() {
		// The goroutine owns f: a blocked flock cannot be cancelled.
		_ = unix.Flock(int(f.Fd()), unix.LOCK_SH)
		_ = f.Close()
		close(done)
	}()
	return done
}

// session is one command's use of a session directory.
type session struct {
	a      *app
	t      target
	spec   acphost.Spec
	state  clientState
	unlock func()
	sigs   chan os.Signal

	link    *link
	welcome acphost.Welcome

	out    *output // nil leaves the journal unread
	off    int64   // journal offset read so far
	policy string  // permission policy

	muted      bool // read the journal without showing it
	inTurn     bool // a signal cancels the turn instead of ending the command
	cancelling bool // session/cancel was sent
	interrupts int
}

// use takes the client lock of the session in t, whose host runs spec, and
// starts catching the signals that cancel a turn.
func (a *app) use(t target, spec acphost.Spec, out *output, policy string) (*session, error) {
	// Before the lock: cancel signals whoever holds it.
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, unix.SIGINT, unix.SIGTERM)
	unlock, err := lockClient(t)
	if err != nil {
		signal.Stop(sigs)
		return nil, err
	}
	state, err := readState(t.dir)
	if err != nil {
		unlock()
		signal.Stop(sigs)
		return nil, err
	}
	return &session{a: a, t: t, spec: spec, state: state, unlock: unlock, sigs: sigs, out: out, policy: policy}, nil
}

// open is use for a session that exists.
func (a *app) open(t target, out *output, policy string) (*session, error) {
	spec, err := readSessionSpec(t)
	if err != nil {
		return nil, err
	}
	return a.use(t, spec, out, policy)
}

func readSessionSpec(t target) (acphost.Spec, error) {
	spec, err := acphost.ReadSpec(t.dir)
	if errors.Is(err, os.ErrNotExist) {
		return spec, fmt.Errorf("no %s (acphost new creates one)", t)
	}
	return spec, err
}

// close saves what was shown, disconnects and releases the lock. The host
// and the agent keep running.
func (s *session) close() {
	if s.link != nil {
		s.link.close()
	}
	if err := writeState(s.t.dir, s.state); err != nil && !errors.Is(err, os.ErrNotExist) {
		say(s.a.stderr, "acphost:", err)
	}
	s.unlock()
	signal.Stop(s.sigs)
}

// start starts the session's host if none runs. A host that cannot start
// is a *spawnError.
func (s *session) start() error {
	if acphost.Alive(s.t.dir) {
		return nil
	}
	if err := acphost.CheckSpawn(s.spec, false, acphost.WithHostDir(s.t.dir)); err != nil {
		return &spawnError{err}
	}
	if _, err := acphost.StartDetached(s.t.dir, nil); err != nil {
		return &spawnError{err}
	}
	return nil
}

// connect dials the host. It shows nothing yet: a command the session
// refuses must not use up what the journal has for the next attach.
func (s *session) connect() error {
	l, w, err := dial(s.t.dir, s.spec.MethodPrefix)
	if err != nil {
		// A host that has already stopped: its exit says more.
		if ex, ok := acphost.ReadExit(s.t.dir); ok {
			return &hostGone{ex}
		}
		return fmt.Errorf("connect to the host: %w", err)
	}
	s.link, s.welcome = l, w
	return nil
}

// stopHost stops the host in dir, if one runs, and waits for it.
func stopHost(dir string) error {
	if !acphost.Alive(dir) {
		return nil
	}
	d, err := proc.Open(dir)
	if err != nil {
		return err
	}
	_, err = stopWait(d, stopGrace)
	return err
}

// reconfigure replaces the session's isolation with c, the config and
// flags given to this command. A running host is stopped, so the next
// start applies it; the agent then loads the conversation again. The same
// isolation as before changes nothing.
func (s *session) reconfigure(c Config) error {
	next := s.spec
	next.Env, next.Sandbox, next.User, next.Cgroup, next.Join = c.Env, c.Sandbox, c.User, c.Cgroup, c.Join
	next.Listen, next.TLS = c.Listen, c.TLS
	was, _ := json.Marshal(s.spec)
	now, _ := json.Marshal(next)
	if string(was) == string(now) {
		return nil
	}
	if acphost.Alive(s.t.dir) {
		l, w, err := dial(s.t.dir, s.spec.MethodPrefix)
		if err != nil {
			return fmt.Errorf("connect to the host: %w", err)
		}
		l.close()
		if w.Busy {
			return errors.New("a turn is running, so the config cannot change now (attach waits for it, cancel stops it)")
		}
		if err := stopHost(s.t.dir); err != nil {
			return err
		}
	}
	if err := acphost.WriteSpec(s.t.dir, next); err != nil {
		return err
	}
	s.spec = next
	return nil
}
