package acpc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	acp "github.com/gfx-labs/go-acp"
	"golang.org/x/sys/unix"

	"github.com/gfx-labs/acphost/pkg/acphost"
)

// Create writes the spec, starts the host and opens an ACP session.
func (s *Session) Create() error {
	if err := acphost.WriteSpec(s.t.Dir, s.spec); err != nil {
		return err
	}
	if err := s.Start(); err != nil {
		return err
	}
	if err := s.Connect(); err != nil {
		return err
	}
	s.CatchUp()
	_, err := s.ensure()
	return err
}

// Prompt sends one prompt and returns once the turn ended. A signal
// cancels the turn; a second one returns ErrInterrupted.
func (s *Session) Prompt(in PromptInput) (acp.StopReason, error) {
	req, err := s.ready(in)
	if err != nil {
		return "", err
	}
	s.inTurn = true
	var res acp.PromptResponse
	err = s.wait(func(ctx context.Context) error {
		var err error
		res, err = s.link.conn.Prompt(ctx, req)
		return err
	}, nil)
	s.inTurn = false
	if err != nil && !errors.Is(err, ErrInterrupted) {
		var gone *HostGoneError
		if !errors.As(err, &gone) {
			err = fmt.Errorf("session/prompt: %w", err)
		}
	}
	return res.StopReason, err
}

// ready prepares the agent for a prompt and returns the request to send.
func (s *Session) ready(in PromptInput) (acp.PromptRequest, error) {
	seed, err := s.ensure()
	if err != nil {
		return acp.PromptRequest{}, err
	}
	if seed != "" {
		// Summarize drops everything up to SummaryEnd, so a later
		// summary does not nest this one.
		in.Text = seed + "\n\n" + acphost.SummaryEnd + "\n\n" + in.Text
	}
	return s.promptRequest(in)
}

// Submit sends one prompt and returns once the host has journaled it. A
// client that disconnected or was replaced sooner could lose the prompt:
// the host closes a replaced connection without reading what is left on it.
// Prompt runs with a context that never ends: go-acp sends session/cancel
// when it does. Closing the connection ends the call instead, and the turn
// keeps running.
func (s *Session) Submit(in PromptInput) error {
	req, err := s.ready(in)
	if err != nil {
		return err
	}
	// Follow from the journal's end now, not s.off: without an Output
	// s.off is 0, and an earlier session/prompt would count as this one.
	from := s.off
	if fi, err := os.Stat(s.journal()); err == nil && fi.Size() > from {
		from = fi.Size()
	}
	go func() { _, _ = s.link.conn.Prompt(context.Background(), req) }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-s.link.served:
		case <-s.sigs:
		case <-ctx.Done():
		}
		cancel()
	}()
	taken := false
	err = acphost.FollowJournal(ctx, s.journal(), from, func(fr acphost.Frame) {
		if fr.From == "controller" && frameMethod(fr) == "session/prompt" {
			taken = true
			cancel()
		}
	})
	if err != nil {
		return err
	}
	if !taken {
		return errors.New("stopped before the host took the prompt")
	}
	return nil
}

// Reach applies q's isolation, starts the host if needed and connects.
// A config change waits for a turn started with --no-wait: the host is
// restarted with the new isolation once it is idle.
func (s *Session) Reach(q Queued) error {
	if q.Config != nil && acphost.Alive(s.t.Dir) {
		if err := s.Connect(); err != nil {
			return err
		}
		if err := s.AwaitRunning(); err != nil {
			return err
		}
		s.link.close()
		s.link = nil
	}
	if q.Config != nil {
		if err := s.Reconfigure(*q.Config); err != nil {
			return err
		}
	}
	if err := s.Start(); err != nil {
		return err
	}
	return s.Connect()
}

// AwaitRunning shows what the journal has unseen and, if a turn started
// with --no-wait is running, shows it and waits for its end.
func (s *Session) AwaitRunning() error {
	s.CatchUp()
	if !s.Busy() {
		return nil
	}
	if _, err := s.AwaitTurn(); err != nil {
		return err
	}
	s.end()
	return nil
}

// InterruptClient sends SIGINT to the client holding t's lock. The pid
// comes from a file, so it is only signalled if that process has the lock
// file open, and through a pidfd so the check and the signal are about the
// same process.
func InterruptClient(t Target, pid int) error {
	if pid <= 0 {
		return fmt.Errorf("%s is in use; try again", t)
	}
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return fmt.Errorf("%s is in use by pid %d: %w", t, pid, err)
	}
	defer func() { _ = unix.Close(fd) }()
	// /proc shows resolved paths.
	lock, err := filepath.EvalSymlinks(filepath.Join(t.Dir, lockFile))
	if err != nil {
		return err
	}
	if !hasOpen(pid, lock) {
		return fmt.Errorf("%s is in use, but pid %d is not its client; try again", t, pid)
	}
	return unix.PidfdSendSignal(fd, unix.SIGINT, nil, 0)
}

// hasOpen reports whether process pid has path open.
func hasOpen(pid int, path string) bool {
	fds := fmt.Sprintf("/proc/%d/fd", pid)
	ents, err := os.ReadDir(fds)
	if err != nil {
		return false
	}
	for _, e := range ents {
		if p, err := os.Readlink(filepath.Join(fds, e.Name())); err == nil && p == path {
			return true
		}
	}
	return false
}
