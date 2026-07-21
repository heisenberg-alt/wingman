package session_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/heisenberg-alt/wingman/daemon/internal/acptest"
	"github.com/heisenberg-alt/wingman/daemon/internal/proto"
	"github.com/heisenberg-alt/wingman/daemon/internal/session"
)

func newManager(t *testing.T, permTimeout time.Duration) *session.Manager {
	t.Helper()
	m := session.NewManager(session.Config{
		CopilotPath:       acptest.Build(t),
		PermissionTimeout: permTimeout,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	t.Cleanup(m.CloseAll)
	return m
}

// waitEvent polls the session log until an event of the given type appears.
func waitEvent(t *testing.T, log *session.Log, evtType string, timeout time.Duration) session.Event {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, e := range log.Since(0) {
			if e.Type == evtType {
				return e
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	var types []string
	for _, e := range log.Since(0) {
		types = append(types, e.Type)
	}
	t.Fatalf("timed out waiting for %q; log has %v", evtType, types)
	return session.Event{}
}

func waitStatus(t *testing.T, s *session.Session, status string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s.Info().Status == status {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for status %q; current %q", status, s.Info().Status)
}

func TestCreateRejectsInvalidCwd(t *testing.T) {
	m := newManager(t, time.Minute)
	ctx := context.Background()

	if _, err := m.Create(ctx, "relative/path"); err == nil {
		t.Error("relative cwd accepted")
	}
	if _, err := m.Create(ctx, "/nonexistent/wingman/dir"); err == nil {
		t.Error("nonexistent cwd accepted")
	}
}

func TestPromptTurnCompletes(t *testing.T) {
	m := newManager(t, time.Minute)
	s, err := m.Create(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	if err := s.SendPrompt("hello"); err != nil {
		t.Fatal(err)
	}
	evt := waitEvent(t, s.Log, proto.EvtTurnEnded, 10*time.Second)
	var turn proto.TurnEnded
	_ = json.Unmarshal(evt.Payload, &turn)
	if turn.StopReason != "end_turn" {
		t.Errorf("stopReason = %q, want end_turn", turn.StopReason)
	}
	waitEvent(t, s.Log, proto.EvtTranscriptDelta, time.Second)
	waitStatus(t, s, session.StatusIdle, 5*time.Second)
}

func TestPermissionApproveFlow(t *testing.T) {
	m := newManager(t, time.Minute)
	s, err := m.Create(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	if err := s.SendPrompt("NEEDPERM write a file"); err != nil {
		t.Fatal(err)
	}
	reqEvt := waitEvent(t, s.Log, proto.EvtPermissionRequest, 10*time.Second)
	var req proto.PermissionRequest
	if err := json.Unmarshal(reqEvt.Payload, &req); err != nil {
		t.Fatal(err)
	}
	if req.Title != "Create file" {
		t.Errorf("title = %q, want Create file", req.Title)
	}
	if len(req.Options) != 2 {
		t.Fatalf("options = %d, want 2", len(req.Options))
	}
	waitStatus(t, s, session.StatusAwaitingPermission, 5*time.Second)

	if err := s.Approve(req.RequestID, "allow_once"); err != nil {
		t.Fatal(err)
	}
	resEvt := waitEvent(t, s.Log, proto.EvtPermissionResolved, 10*time.Second)
	var res proto.PermissionResolved
	_ = json.Unmarshal(resEvt.Payload, &res)
	if res.ResolvedBy != "phone" || res.OptionID != "allow_once" {
		t.Errorf("resolved = %+v, want phone/allow_once", res)
	}
	waitEvent(t, s.Log, proto.EvtTurnEnded, 10*time.Second)
}

func TestPermissionTimeoutFailsSafeToDeny(t *testing.T) {
	m := newManager(t, 200*time.Millisecond)
	s, err := m.Create(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	if err := s.SendPrompt("NEEDPERM dangerous thing"); err != nil {
		t.Fatal(err)
	}
	resEvt := waitEvent(t, s.Log, proto.EvtPermissionResolved, 10*time.Second)
	var res proto.PermissionResolved
	_ = json.Unmarshal(resEvt.Payload, &res)
	if res.ResolvedBy != "timeout" {
		t.Errorf("resolvedBy = %q, want timeout", res.ResolvedBy)
	}
	waitEvent(t, s.Log, proto.EvtTurnEnded, 10*time.Second)
}

func TestBusySessionRejectsSecondPrompt(t *testing.T) {
	m := newManager(t, time.Minute)
	s, err := m.Create(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	if err := s.SendPrompt("NEEDPERM hold the turn open"); err != nil {
		t.Fatal(err)
	}
	reqEvt := waitEvent(t, s.Log, proto.EvtPermissionRequest, 10*time.Second)

	if err := s.SendPrompt("second prompt"); err == nil {
		t.Error("busy session accepted a second prompt")
	}

	var req proto.PermissionRequest
	_ = json.Unmarshal(reqEvt.Payload, &req)
	_ = s.Approve(req.RequestID, "reject_once")
	waitEvent(t, s.Log, proto.EvtTurnEnded, 10*time.Second)
}

func TestApproveUnknownRequestFails(t *testing.T) {
	m := newManager(t, time.Minute)
	s, err := m.Create(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Approve("bogus-id", "allow_once"); err == nil {
		t.Error("approve of unknown request succeeded")
	}
}

func TestListSortsNewestFirst(t *testing.T) {
	m := newManager(t, time.Minute)
	ctx := context.Background()

	first, err := m.Create(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	second, err := m.Create(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	list := m.List()
	if len(list) != 2 {
		t.Fatalf("list = %d sessions, want 2", len(list))
	}
	if list[0].ID != second.ID || list[1].ID != first.ID {
		t.Errorf("list order = [%s %s], want newest first [%s %s]",
			list[0].ID, list[1].ID, second.ID, first.ID)
	}
}

func TestRecentDirsPersistAcrossManagers(t *testing.T) {
	stateDir := t.TempDir()
	newWithState := func() *session.Manager {
		m := session.NewManager(session.Config{
			CopilotPath: acptest.Build(t),
			StateDir:    stateDir,
			Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		t.Cleanup(m.CloseAll)
		return m
	}

	m := newWithState()
	cwd := t.TempDir()
	if _, err := m.Create(context.Background(), cwd); err != nil {
		t.Fatal(err)
	}
	dirs := m.RecentDirs()
	if len(dirs) == 0 || dirs[0] != cwd {
		t.Fatalf("recent dirs = %v, want %q first", dirs, cwd)
	}

	// A fresh manager sharing the state dir loads the same recents.
	m2 := newWithState()
	dirs2 := m2.RecentDirs()
	if len(dirs2) == 0 || dirs2[0] != cwd {
		t.Errorf("reloaded recent dirs = %v, want %q first", dirs2, cwd)
	}
}

func TestRemoveOnlyTerminalSessions(t *testing.T) {
	m := newManager(t, time.Minute)
	s, err := m.Create(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// Idle (non-terminal) sessions cannot be removed.
	if err := m.Remove(s.ID); err == nil {
		t.Fatal("removed an idle session")
	}

	// Closing the subprocess sends the idle session to done.
	m.CloseAll()
	waitStatus(t, s, session.StatusDone, 5*time.Second)

	if err := m.Remove(s.ID); err != nil {
		t.Fatalf("remove done session: %v", err)
	}
	if _, ok := m.Get(s.ID); ok {
		t.Error("session still present after Remove")
	}
	if err := m.Remove(s.ID); err == nil {
		t.Error("second remove succeeded")
	}
}
