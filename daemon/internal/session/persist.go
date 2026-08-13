// Session persistence: each session lives in <StateDir>/sessions/<id>/ as a
// meta.json snapshot plus an append-only log.jsonl event log, so session ids,
// statuses, and transcripts survive daemon restarts (ADR-0005). Restored
// sessions are dormant — no subprocess — until the next prompt respawns
// Copilot and reattaches via ACP session/load.
package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/heisenberg-alt/wingman/daemon/internal/proto"
)

// maxPersistedSessions bounds how many sessions survive a restart; older
// ones (by creation time) are pruned from disk at startup.
const maxPersistedSessions = 50

// meta is the persisted snapshot of a session's identity and status.
type meta struct {
	ID        string    `json:"id"`
	Cwd       string    `json:"cwd"`
	AcpID     string    `json:"acpId"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
}

func (m *Manager) sessionsRoot() string {
	if m.cfg.StateDir == "" {
		return ""
	}
	return filepath.Join(m.cfg.StateDir, "sessions")
}

// persistMeta snapshots the session to meta.json. No-op for in-memory
// sessions (no StateDir).
func (s *Session) persistMeta() {
	if s.dir == "" {
		return
	}
	s.mu.Lock()
	m := meta{ID: s.ID, Cwd: s.Cwd, AcpID: s.acpID, Status: s.status, CreatedAt: s.CreatedAt}
	s.mu.Unlock()

	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	tmp := filepath.Join(s.dir, "meta.json.tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		s.mgr.cfg.Logger.Warn("persist session meta", "session", s.ID, "err", err)
		return
	}
	if err := os.Rename(tmp, filepath.Join(s.dir, "meta.json")); err != nil {
		s.mgr.cfg.Logger.Warn("persist session meta", "session", s.ID, "err", err)
	}
}

// restoreSessions loads persisted sessions from StateDir into the manager as
// dormant sessions and prunes the oldest beyond maxPersistedSessions. Called
// once from NewManager, before the manager is shared.
func (m *Manager) restoreSessions() {
	root := m.sessionsRoot()
	if root == "" {
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if !os.IsNotExist(err) {
			m.cfg.Logger.Warn("read sessions dir", "err", err)
		}
		return
	}

	var restored []*Session
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		s, err := m.restoreSession(dir)
		if err != nil {
			m.cfg.Logger.Warn("skipping unrestorable session", "dir", dir, "err", err)
			continue
		}
		restored = append(restored, s)
	}

	// Retention: newest first, prune everything beyond the cap.
	slices.SortFunc(restored, func(a, b *Session) int {
		return b.CreatedAt.Compare(a.CreatedAt)
	})
	for i, s := range restored {
		if i >= maxPersistedSessions {
			_ = s.Log.Close()
			if err := os.RemoveAll(s.dir); err != nil {
				m.cfg.Logger.Warn("prune session", "session", s.ID, "err", err)
			}
			continue
		}
		m.sessions[s.ID] = s
	}
	if len(restored) > 0 {
		m.cfg.Logger.Info("restored sessions", "count", min(len(restored), maxPersistedSessions))
	}
}

func (m *Manager) restoreSession(dir string) (*Session, error) {
	data, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return nil, err
	}
	var mt meta
	if err := json.Unmarshal(data, &mt); err != nil {
		return nil, fmt.Errorf("meta.json: %w", err)
	}
	if mt.ID == "" || mt.AcpID == "" {
		return nil, fmt.Errorf("meta.json: missing id or acpId")
	}
	if mt.ID != filepath.Base(dir) {
		return nil, fmt.Errorf("meta.json: id %q does not match dir %q", mt.ID, filepath.Base(dir))
	}
	log, err := OpenLog(filepath.Join(dir, "log.jsonl"))
	if err != nil {
		return nil, err
	}

	s := &Session{
		ID:        mt.ID,
		Cwd:       mt.Cwd,
		CreatedAt: mt.CreatedAt,
		Log:       log,
		mgr:       m,
		acpID:     mt.AcpID,
		dir:       dir,
		status:    mt.Status,
		pending:   make(map[string]*pendingPermission),
	}
	// A session persisted mid-turn (daemon died while running or awaiting a
	// permission) has no live turn anymore: settle it at idle so clients see
	// a resumable state, and record the transition for replaying watchers.
	if mt.Status != StatusDone && mt.Status != StatusError && mt.Status != StatusIdle {
		s.status = StatusIdle
		s.Log.Append(proto.EvtSessionState, proto.SessionState{Status: StatusIdle})
		s.persistMeta()
	}
	return s, nil
}
