package acpc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	acp "github.com/gfx-labs/go-acp"

	"github.com/gfx-labs/acphost/pkg/acphost"
	"github.com/gfx-labs/acphost/pkg/proc"
	"golang.org/x/sys/unix"
)

// A session is a host's session directory plus two files of this client:
// client.json and client.lock. Named sessions live under
// $XDG_STATE_HOME/acpc/sessions/<cwd key>/<name>, so a name means a
// different session in each working directory. See
// docs/notes/290-cli-sessions.md.

const (
	// stateFile holds clientState.
	stateFile = "client.json"
	// lockFile is flocked by the one client using the session and holds
	// its pid. A second connection would replace the first at the host.
	lockFile = "client.lock"
	// DefaultSession is the name used without -s.
	DefaultSession = "default"
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

// Target is a session directory and the name it was found by.
type Target struct {
	Name string // empty when given as a directory
	Dir  string
}

func (t Target) String() string {
	if t.Name != "" {
		return "session " + t.Name
	}
	return "session dir " + t.Dir
}

// SessionsRoot is where named sessions are kept.
func SessionsRoot() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "acpc", "sessions"), nil
}

// CwdKey names a working directory under the sessions root. It is a short
// hash because the socket path below it has to fit in 107 bytes.
func CwdKey(cwd string) string {
	sum := sha256.Sum256([]byte(cwd))
	return hex.EncodeToString(sum[:6])
}

// CheckName accepts a name that is safe as one path element.
func CheckName(name string) error {
	ok := name != "" && len(name) <= 32 && name[0] != '.' && name[0] != '-'
	for _, r := range name {
		ok = ok && (r == '.' || r == '_' || r == '-' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'))
	}
	if !ok {
		return fmt.Errorf("bad session name %q: use up to 32 letters, digits, '.', '_' or '-', starting with a letter or digit", name)
	}
	return nil
}

// Resolve finds the session directory for -s name or --dir dir. fallback
// is the name used when neither is given; empty makes one of them required.
func Resolve(name, dir, fallback string) (Target, error) {
	switch {
	case name != "" && dir != "":
		return Target{}, errors.New("give -s or --dir, not both")
	case dir != "":
		abs, err := filepath.Abs(dir)
		return Target{Dir: abs}, err
	case name == "" && fallback == "":
		return Target{}, errors.New("needs -s NAME or --dir DIR")
	case name == "":
		name = fallback
	}
	if err := CheckName(name); err != nil {
		return Target{}, err
	}
	root, err := SessionsRoot()
	if err != nil {
		return Target{}, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return Target{}, err
	}
	return Target{Name: name, Dir: filepath.Join(root, CwdKey(cwd), name)}, nil
}

// InUseError is a session whose client lock another process holds.
type InUseError struct {
	Target Target
	Pid    int
}

func (e *InUseError) Error() string {
	return fmt.Sprintf("%s is in use by pid %d (a prompt or attach is running)", e.Target, e.Pid)
}

// LockClient takes the session's client lock and records this pid in it.
// It does not wait: a held lock is an *InUseError.
func LockClient(t Target) (unlock func(), err error) {
	f, err := os.OpenFile(filepath.Join(t.Dir, lockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		pid := readPid(f)
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, &InUseError{Target: t, Pid: pid}
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

// LockHolder returns the pid of the client using the session, if any.
func LockHolder(dir string) (pid int, held bool) {
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

// LockReleased returns a channel that is closed once no client holds the
// session's lock.
func LockReleased(dir string) <-chan struct{} {
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

// Output shows journal frames. End finishes a turn's output, for
// example the last line of quiet text.
type Output interface {
	Frame(acphost.Frame)
	End()
}

// Options configure a Session.
type Options struct {
	// Output shows the journal. Nil leaves the journal unread, so the
	// next client still shows it.
	Output Output
	// Policy answers permission requests: ApproveAll, ApproveReads
	// (the default) or DenyAll.
	Policy string
	// Log gets notices such as a refused permission. Nil discards them.
	Log io.Writer
	// Trace gets the steps of the ACP handshake as they happen. Nil
	// leaves them out.
	Trace io.Writer
	// ClientInfo is sent in initialize.
	ClientInfo acp.Implementation
}

// Session is one client's use of a session directory. It holds the
// session's client lock until Close.
type Session struct {
	opts   Options
	t      Target
	spec   acphost.Spec
	state  clientState
	unlock func()
	sigs   chan os.Signal

	link    *link
	welcome acphost.Welcome

	off  int64 // journal offset read so far
	caps acp.PromptCapabilities

	muted      bool        // read the journal without showing it
	inTurn     bool        // a signal cancels the turn instead of ending the wait
	cancelling atomic.Bool // session/cancel was sent; read by permission handlers
	interrupts int
}

// Open takes the client lock of the session in t, whose host runs spec,
// and starts catching SIGINT and SIGTERM, which cancel a turn. A held
// lock is an *InUseError.
func Open(t Target, spec acphost.Spec, opts Options) (*Session, error) {
	return open(t, spec, opts, func(<-chan os.Signal) (func(), error) { return LockClient(t) })
}

// OpenQueued is Open for the prompt queued as e: it waits for its turn.
func OpenQueued(t Target, spec acphost.Spec, opts Options, e *Entry) (*Session, error) {
	return open(t, spec, opts, func(sigs <-chan os.Signal) (func(), error) { return waitTurn(t, e, sigs) })
}

// OpenExisting is Open for a session whose spec is already written.
func OpenExisting(t Target, opts Options) (*Session, error) {
	spec, err := ReadSessionSpec(t)
	if err != nil {
		return nil, err
	}
	return Open(t, spec, opts)
}

func open(t Target, spec acphost.Spec, opts Options, lock func(<-chan os.Signal) (func(), error)) (*Session, error) {
	if opts.Policy == "" {
		opts.Policy = ApproveReads
	}
	if opts.Log == nil {
		opts.Log = io.Discard
	}
	// Before the lock: cancel signals whoever holds it.
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, unix.SIGINT, unix.SIGTERM)
	unlock, err := lock(sigs)
	if err != nil {
		signal.Stop(sigs)
		return nil, err
	}
	state, err := readState(t.Dir)
	if err != nil {
		unlock()
		signal.Stop(sigs)
		return nil, err
	}
	return &Session{opts: opts, t: t, spec: spec, state: state, unlock: unlock, sigs: sigs}, nil
}

// Busy reports whether a turn was running when the session connected.
func (s *Session) Busy() bool { return s.welcome.Busy }

func (s *Session) logln(a ...any) { _, _ = fmt.Fprintln(s.opts.Log, a...) }

func (s *Session) logf(format string, a ...any) { _, _ = fmt.Fprintf(s.opts.Log, format, a...) }

func (s *Session) end() {
	if s.opts.Output != nil {
		s.opts.Output.End()
	}
}

// ReadSessionSpec reads the spec of the session in t.
func ReadSessionSpec(t Target) (acphost.Spec, error) {
	spec, err := acphost.ReadSpec(t.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return spec, fmt.Errorf("no %s (acpc new creates one)", t)
	}
	return spec, err
}

// Close saves what was shown, disconnects and releases the lock. The host
// and the agent keep running.
func (s *Session) Close() {
	if s.link != nil {
		s.link.close()
	}
	if err := writeState(s.t.Dir, s.state); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.logln(err)
	}
	s.unlock()
	signal.Stop(s.sigs)
}

// Start starts the session's host if none runs. A host that cannot start
// is a *SpawnError.
func (s *Session) Start() error {
	if acphost.Alive(s.t.Dir) {
		return nil
	}
	if err := acphost.CheckSpawn(s.spec, false, acphost.WithHostDir(s.t.Dir)); err != nil {
		return &SpawnError{err}
	}
	s.tracef("[acpc] spawning agent: %s", strings.Join(append([]string{s.spec.Command}, s.spec.Args...), " "))
	d, err := acphost.StartDetached(s.t.Dir, nil)
	if err != nil {
		return &SpawnError{err}
	}
	s.tracef("[acpc] host pid %d", d.Pid)
	return nil
}

// Connect dials the host. It shows nothing yet: a command the session
// refuses must not use up what the journal has for the next attach.
func (s *Session) Connect() error {
	l, w, err := s.dial()
	if err != nil {
		// A host that has already stopped: its exit says more.
		if ex, ok := acphost.ReadExit(s.t.Dir); ok {
			return &HostGoneError{ex}
		}
		return fmt.Errorf("connect to the host: %w", err)
	}
	s.link, s.welcome = l, w
	return nil
}

// HostBusy connects to the host in dir only to read whether a turn runs.
func HostBusy(dir, prefix string) (bool, error) {
	nc, _, w, err := acphost.Connect(context.Background(), dir, acphost.WithMethodPrefix(prefix))
	if err != nil {
		return false, err
	}
	_ = nc.Close()
	return w.Busy, nil
}

// StopHost stops the host in dir, if one runs, and waits for it.
func StopHost(dir string) error {
	if !acphost.Alive(dir) {
		return nil
	}
	d, err := proc.Open(dir)
	if err != nil {
		return err
	}
	_, err = StopWait(d, StopGrace)
	return err
}

// StopGrace bounds the wait for a host to stop before it is killed.
const StopGrace = 30 * time.Second

// StopWait stops d and waits for its exit. With a grace it SIGKILLs a host
// that has not finished by then and returns context.DeadlineExceeded.
func StopWait(d *proc.Detached, grace time.Duration) (proc.Exit, error) {
	if err := d.Stop(); err != nil {
		return proc.Exit{}, err
	}
	ctx := context.Background()
	if grace > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, grace)
		defer cancel()
	}
	ex, err := d.Wait(ctx)
	if errors.Is(err, context.DeadlineExceeded) {
		_ = unix.Kill(d.Pid, unix.SIGKILL)
	}
	return ex, err
}

// Reconfigure replaces the session's isolation with c, the config and
// flags given to this command. A running host is stopped, so the next
// start applies it; the agent then loads the conversation again. The same
// isolation as before changes nothing.
func (s *Session) Reconfigure(c Config) error {
	next := s.spec
	next.Env, next.Sandbox, next.User, next.Cgroup, next.Join = c.Env, c.Sandbox, c.User, c.Cgroup, c.Join
	next.Listen, next.TLS = c.Listen, c.TLS
	was, _ := json.Marshal(s.spec)
	now, _ := json.Marshal(next)
	if string(was) == string(now) {
		return nil
	}
	if acphost.Alive(s.t.Dir) {
		busy, err := HostBusy(s.t.Dir, s.spec.MethodPrefix)
		if err != nil {
			return fmt.Errorf("connect to the host: %w", err)
		}
		if busy {
			return errors.New("a turn is running, so the config cannot change now (attach waits for it, cancel stops it)")
		}
		if err := StopHost(s.t.Dir); err != nil {
			return err
		}
	}
	if err := acphost.WriteSpec(s.t.Dir, next); err != nil {
		return err
	}
	s.spec = next
	return nil
}

// Discard stops the host of a session that could not be created and
// removes it. A session given as a directory keeps the directory: only
// the files that make it a session go.
func Discard(t Target) error {
	err := StopHost(t.Dir)
	if t.Name == "" {
		for _, f := range []string{acphost.SpecFile, stateFile, lockFile} {
			_ = os.Remove(filepath.Join(t.Dir, f))
		}
		return err
	}
	removeNamed(t)
	return err
}

// Remove stops the host of the named session t and deletes the session.
// It fails if a client is using the session.
func Remove(t Target) error {
	unlock, err := LockClient(t)
	if err != nil {
		return err
	}
	defer unlock()
	if err := StopHost(t.Dir); err != nil {
		return err
	}
	removeNamed(t)
	return nil
}

// removeNamed deletes a named session's directory, and its working
// directory's when that was the last session in it.
func removeNamed(t Target) {
	_ = os.RemoveAll(t.Dir)
	_ = os.Remove(filepath.Dir(t.Dir))
}
