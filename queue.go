package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// A prompt for a session that is in use waits its turn in the session's
// queue directory. Each waiting prompt is one file, named so that names
// sort in arrival order, and flocked by the process that waits for it: a
// foreground prompt, or for --no-wait a detached acpc started for it. A
// file whose lock nobody holds belongs to a waiter that died, and is
// removed. A prompt runs when its file is first and it can take the client
// lock; then it waits on the connection for a running turn to end. See docs/notes/2026-11-13-prompt-queue.md.

const (
	queueDir = "queue"
	queueLog = "queue.log" // stderr of detached waiters
	// queuePoll is how often a waiter looks at the queue and the lock.
	queuePoll = 200 * time.Millisecond
)

// queued is what a queue file holds.
type queued struct {
	Input  promptInput `json:"input"`
	Config *Config     `json:"config,omitempty"` // -c and isolation flags, applied when it runs
	Policy string      `json:"policy"`
	NoWait bool        `json:"no_wait,omitempty"`
}

// entry is this process's file in the queue, locked while it waits.
type entry struct {
	path string
	f    *os.File
}

// enqueue adds q to the queue of the session in dir.
func enqueue(dir string, q queued) (*entry, error) {
	qd := filepath.Join(dir, queueDir)
	if err := os.MkdirAll(qd, 0o700); err != nil {
		return nil, err
	}
	b, err := json.Marshal(q)
	if err != nil {
		return nil, err
	}
	name := fmt.Sprintf("%020d-%d.json", time.Now().UnixNano(), os.Getpid())
	tmp := filepath.Join(qd, "."+name+".tmp")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	// Locked before it has its name, so no scan sees it unlocked.
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return nil, err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return nil, err
	}
	path := filepath.Join(qd, name)
	if err := os.Rename(tmp, path); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return nil, err
	}
	return &entry{path: path, f: f}, nil
}

// enqueueFront adds an entry that sorts before every waiting prompt, for
// a command that needs the session next without sending a prompt.
func enqueueFront(dir string) (*entry, error) {
	qd := filepath.Join(dir, queueDir)
	if err := os.MkdirAll(qd, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(qd, fmt.Sprintf("%020d-%d.json", 0, os.Getpid()))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &entry{path: path, f: f}, nil
}

// adopt takes over an entry whose lock this process inherited as fd.
func adopt(path string, fd uintptr) (*entry, queued, error) {
	var q queued
	f := os.NewFile(fd, path)
	if f == nil {
		return nil, q, fmt.Errorf("queue entry %s: no lock fd", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		_ = f.Close()
		return nil, q, err
	}
	if err := json.Unmarshal(b, &q); err != nil {
		_ = f.Close()
		return nil, q, fmt.Errorf("queue entry %s: %w", path, err)
	}
	return &entry{path: path, f: f}, q, nil
}

// done removes the entry and lets go of its lock.
func (e *entry) done() {
	_ = os.Remove(e.path)
	_ = e.f.Close()
}

// queueAhead returns the live entries in dir's queue that come before
// name (all of them for ""), removing the files of dead waiters.
func queueAhead(dir, name string) []string {
	qd := filepath.Join(dir, queueDir)
	ents, _ := os.ReadDir(qd)
	var live []string
	for _, e := range ents {
		n := e.Name()
		if strings.HasPrefix(n, ".") || !strings.HasSuffix(n, ".json") {
			continue
		}
		if name != "" && n >= name {
			break
		}
		if held(filepath.Join(qd, n)) {
			live = append(live, n)
		} else {
			_ = os.Remove(filepath.Join(qd, n))
		}
	}
	return live
}

// held reports whether a waiter holds the lock of the queue file path.
func held(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	return errors.Is(unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB), unix.EWOULDBLOCK)
}

// errDropped is a queued prompt whose file was removed: cancel --queued
// or rm.
var errDropped = errors.New("the prompt was removed from the queue")

// waitTurn waits until e is first in the queue and the client lock is
// free, and returns holding the lock. A turn may still be running; the
// caller waits for it with the connection. A signal ends the wait with
// errInterrupted.
func waitTurn(t target, e *entry, sigs <-chan os.Signal) (unlock func(), err error) {
	name := filepath.Base(e.path)
	for {
		if _, err := os.Stat(e.path); err != nil {
			return nil, errDropped
		}
		if len(queueAhead(t.dir, name)) == 0 {
			unlock, err := lockClient(t)
			var inUse *inUseError
			if err == nil {
				return unlock, nil
			}
			if !errors.As(err, &inUse) {
				return nil, err
			}
		}
		select {
		case <-sigs:
			return nil, errInterrupted
		case <-time.After(queuePoll):
		}
	}
}

// startWaiter starts a detached acpc that waits for e and sends its prompt
// with --no-wait semantics. It inherits e's lock, so e stays live after
// this process exits.
func startWaiter(t target, e *entry) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logf, err := os.OpenFile(filepath.Join(t.dir, queueLog), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = logf.Close() }()
	cmd := exec.Command(exe, "prompt", "--dir", t.dir, "--queued", e.path)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.ExtraFiles = []*os.File{e.f} // fd 3
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// queuedFD is the fd a detached waiter gets its entry's lock on.
const queuedFD = 3

// clearQueue removes the waiting prompts of the session in dir and
// returns how many there were. Their waiters see the file gone and exit.
func clearQueue(dir string) int {
	names := queueAhead(dir, "")
	for _, n := range names {
		_ = os.Remove(filepath.Join(dir, queueDir, n))
	}
	return len(names)
}
