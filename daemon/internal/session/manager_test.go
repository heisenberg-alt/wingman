package session_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
	// The denied turn is over; the session must settle at idle, not be
	// stranded at running by the permission handler's exit path.
	waitStatus(t, s, session.StatusIdle, 5*time.Second)
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

func TestRemoveRejectsBusySessions(t *testing.T) {
	m := newManager(t, time.Minute)
	s, err := m.Create(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// A session mid-turn (awaiting a permission answer) cannot be removed.
	if err := s.SendPrompt("NEEDPERM hold the turn open"); err != nil {
		t.Fatal(err)
	}
	reqEvt := waitEvent(t, s.Log, proto.EvtPermissionRequest, 10*time.Second)
	if err := m.Remove(s.ID); err == nil {
		t.Fatal("removed a session mid-turn")
	}

	// Once the turn ends, the idle session is removable.
	var req proto.PermissionRequest
	_ = json.Unmarshal(reqEvt.Payload, &req)
	_ = s.Approve(req.RequestID, "reject_once")
	waitEvent(t, s.Log, proto.EvtTurnEnded, 10*time.Second)
	waitStatus(t, s, session.StatusIdle, 5*time.Second)

	if err := m.Remove(s.ID); err != nil {
		t.Fatalf("remove idle session: %v", err)
	}
	if _, ok := m.Get(s.ID); ok {
		t.Error("session still present after Remove")
	}
	if err := m.Remove(s.ID); err == nil {
		t.Error("second remove succeeded")
	}
}

// persistentManager builds a manager rooted at stateDir so sessions persist.
func persistentManager(t *testing.T, stateDir string) *session.Manager {
	t.Helper()
	return session.NewManager(session.Config{
		CopilotPath:       acptest.Build(t),
		PermissionTimeout: time.Minute,
		StateDir:          stateDir,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func TestSessionsSurviveRestart(t *testing.T) {
	stateDir := t.TempDir()

	m1 := persistentManager(t, stateDir)
	s1, err := m1.Create(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.SendPrompt("hello"); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, s1.Log, proto.EvtTurnEnded, 10*time.Second)
	waitStatus(t, s1, session.StatusIdle, 5*time.Second)
	before := s1.Log.Since(0)
	m1.CloseAll()

	// A fresh manager over the same state dir restores the session.
	m2 := persistentManager(t, stateDir)
	t.Cleanup(m2.CloseAll)
	list := m2.List()
	if len(list) != 1 || list[0].ID != s1.ID {
		t.Fatalf("restored list = %+v, want session %s", list, s1.ID)
	}
	if list[0].Status != session.StatusIdle {
		t.Fatalf("restored status = %q, want idle", list[0].Status)
	}

	s2, ok := m2.Get(s1.ID)
	if !ok {
		t.Fatal("restored session not found by id")
	}
	// session.watch { fromSeq } replay works across the restart.
	after := s2.Log.Since(0)
	if len(after) != len(before) {
		t.Fatalf("replay = %d events, want %d", len(after), len(before))
	}

	// A new prompt resumes the conversation via session/load.
	if err := s2.SendPrompt("again"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		ended := 0
		for _, e := range s2.Log.Since(0) {
			if e.Type == proto.EvtTurnEnded {
				ended++
			}
		}
		if ended >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("resumed prompt never completed a turn")
		}
		time.Sleep(20 * time.Millisecond)
	}
	waitStatus(t, s2, session.StatusIdle, 5*time.Second)

	// History replayed by session/load must not be re-appended.
	for _, e := range s2.Log.Since(0) {
		if strings.Contains(string(e.Payload), "REPLAYED-HISTORY") {
			t.Fatal("session/load replay leaked into the event log")
		}
	}
}

func TestMidTurnSessionRestoresAsIdle(t *testing.T) {
	stateDir := t.TempDir()

	// Hand-craft a session persisted mid-turn, as left behind by a crash.
	dir := filepath.Join(stateDir, "sessions", "deadbeef01234567")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	meta := `{"id":"deadbeef01234567","cwd":"/tmp","acpId":"fake-session-1","status":"awaiting_permission","createdAt":"2026-08-01T00:00:00Z"}`
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
	logLine := `{"seq":1,"type":"session.state","payload":{"status":"running"},"time":"2026-08-01T00:00:01Z"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "log.jsonl"), []byte(logLine), 0o600); err != nil {
		t.Fatal(err)
	}

	m := persistentManager(t, stateDir)
	t.Cleanup(m.CloseAll)
	list := m.List()
	if len(list) != 1 || list[0].Status != session.StatusIdle {
		t.Fatalf("restored mid-turn session = %+v, want idle", list)
	}
	s, _ := m.Get("deadbeef01234567")
	events := s.Log.Since(0)
	last := events[len(events)-1]
	if last.Type != proto.EvtSessionState || !strings.Contains(string(last.Payload), "idle") {
		t.Fatalf("last event = %+v, want appended idle state", last)
	}
}

func TestRetentionPrunesOldestSessions(t *testing.T) {
	stateDir := t.TempDir()
	root := filepath.Join(stateDir, "sessions")

	// 52 persisted sessions with ascending creation times.
	for i := 0; i < 52; i++ {
		id := fmt.Sprintf("session%08d", i)
		dir := filepath.Join(root, id)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		meta := fmt.Sprintf(
			`{"id":%q,"cwd":"/tmp","acpId":"fake-session-1","status":"idle","createdAt":"2026-07-01T00:00:%02dZ"}`,
			id, i)
		if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "log.jsonl"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	m := persistentManager(t, stateDir)
	t.Cleanup(m.CloseAll)
	if got := len(m.List()); got != 50 {
		t.Fatalf("restored %d sessions, want 50", got)
	}
	// The two oldest are gone from memory and disk.
	for i := 0; i < 2; i++ {
		id := fmt.Sprintf("session%08d", i)
		if _, ok := m.Get(id); ok {
			t.Errorf("oldest session %s survived retention", id)
		}
		if _, err := os.Stat(filepath.Join(root, id)); !os.IsNotExist(err) {
			t.Errorf("dir for %s still on disk", id)
		}
	}
}
