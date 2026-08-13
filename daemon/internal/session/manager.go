// Package session manages Copilot ACP sessions: one subprocess per session,
// a replayable event log, and a fail-safe pending-permission registry.
package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/heisenberg-alt/wingman/daemon/internal/acp"
	"github.com/heisenberg-alt/wingman/daemon/internal/proto"
)

// Session statuses.
const (
	StatusStarting           = "starting"
	StatusIdle               = "idle"
	StatusRunning            = "running"
	StatusAwaitingPermission = "awaiting_permission"
	StatusDone               = "done"
	StatusError              = "error"
)

// Config configures the Manager.
type Config struct {
	// CopilotPath is the copilot binary; defaults to "copilot".
	CopilotPath string
	// PermissionTimeout is how long a permission request waits for the phone
	// before failing safe to deny. Defaults to 5 minutes.
	PermissionTimeout time.Duration
	// StateDir, when set, persists recent working directories there.
	StateDir string
	// OnPermissionRequest, when set, is invoked (on its own goroutine) each
	// time a session appends a permission.request — the Phase 4 push
	// notification trigger.
	OnPermissionRequest func(sessionID, requestID, title string, optionIDs []string)
	Logger              *slog.Logger
}

// Manager owns all sessions in this daemon.
type Manager struct {
	cfg      Config
	mu       sync.Mutex
	sessions map[string]*Session
	recent   []string // most recent first, capped
	// closing suppresses subprocess-exit status changes during CloseAll so
	// a graceful shutdown doesn't mark resumable sessions done/error.
	closing atomic.Bool
}

const maxRecentDirs = 20

// NewManager creates a Manager, restoring any sessions persisted under
// cfg.StateDir from a previous run.
func NewManager(cfg Config) *Manager {
	if cfg.CopilotPath == "" {
		cfg.CopilotPath = "copilot"
	}
	if cfg.PermissionTimeout <= 0 {
		cfg.PermissionTimeout = 5 * time.Minute
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	m := &Manager{cfg: cfg, sessions: make(map[string]*Session)}
	m.loadRecentDirs()
	m.restoreSessions()
	return m
}

// Session is one live or dormant Copilot ACP session. Dormant sessions
// (restored from disk, no subprocess) are respawned lazily on the next
// prompt via ACP session/load.
type Session struct {
	ID        string
	Cwd       string
	CreatedAt time.Time
	Log       *Log

	mgr   *Manager
	acpID string
	dir   string // persistence dir; "" = in-memory only

	// suppress drops ACP notifications while session/load replays history
	// the daemon already has persisted.
	suppress atomic.Bool

	mu      sync.Mutex
	client  *acp.Client // nil while dormant
	status  string
	pending map[string]*pendingPermission
}

type pendingPermission struct {
	ch chan string // buffered(1); receives chosen optionId, "" = cancel
}

// Create spawns a copilot ACP subprocess, performs the handshake, and opens a
// session rooted at cwd. ctx bounds only the handshake; the subprocess itself
// is tied to the daemon's lifetime so sessions survive client disconnects.
func (m *Manager) Create(ctx context.Context, cwd string) (*Session, error) {
	if !filepath.IsAbs(cwd) {
		return nil, fmt.Errorf("cwd must be an absolute path")
	}
	info, err := os.Stat(cwd)
	if err != nil {
		return nil, fmt.Errorf("cwd: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("cwd %q is not a directory", cwd)
	}

	s := &Session{
		ID:        newID(),
		Cwd:       cwd,
		CreatedAt: time.Now().UTC(),
		mgr:       m,
		status:    StatusStarting,
		pending:   make(map[string]*pendingPermission),
	}

	if root := m.sessionsRoot(); root != "" {
		s.dir = filepath.Join(root, s.ID)
		if err := os.MkdirAll(s.dir, 0o700); err != nil {
			return nil, fmt.Errorf("session dir: %w", err)
		}
		log, err := OpenLog(filepath.Join(s.dir, "log.jsonl"))
		if err != nil {
			_ = os.RemoveAll(s.dir)
			return nil, err
		}
		s.Log = log
		defer func() {
			if s.acpID == "" { // failed after persistence setup
				_ = s.Log.Close()
				_ = os.RemoveAll(s.dir)
			}
		}()
	} else {
		s.Log = NewLog()
	}

	// Deliberately not ctx: the subprocess must outlive the creating
	// connection. Shutdown is handled by Manager.CloseAll.
	client, err := acp.Spawn(context.Background(), acp.Options{
		Command:        m.cfg.CopilotPath,
		Dir:            cwd,
		OnNotification: s.onNotification,
		OnRequest:      s.onRequest,
	})
	if err != nil {
		return nil, err
	}
	s.client = client

	if _, err := client.Initialize(ctx); err != nil {
		client.Close()
		return nil, fmt.Errorf("initialize: %w", err)
	}
	acpID, err := client.NewSession(ctx, cwd)
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("session/new: %w", err)
	}
	s.acpID = acpID
	s.setStatus(StatusIdle)
	s.watchExit(client)

	m.mu.Lock()
	m.sessions[s.ID] = s
	m.mu.Unlock()
	m.rememberDir(cwd)

	m.cfg.Logger.Info("session created", "id", s.ID, "cwd", cwd)
	return s, nil
}

// watchExit reaps the subprocess: when it exits outside a graceful daemon
// shutdown, a session that was idle completed normally; anything else is an
// error. During CloseAll the status is left untouched so the persisted
// session stays resumable after a restart.
func (s *Session) watchExit(client *acp.Client) {
	go func() {
		<-client.Done()
		if s.mgr.closing.Load() {
			return
		}
		s.mu.Lock()
		st := s.status
		s.mu.Unlock()
		switch st {
		case StatusIdle, StatusDone:
			s.setStatus(StatusDone)
		default:
			s.setStatus(StatusError)
		}
	}()
}

// ensureClient respawns the Copilot subprocess for a dormant (restored)
// session and reattaches to its conversation via ACP session/load. Callers
// hold the busy status, so at most one resume runs at a time. History
// replayed by session/load is suppressed — it is already in the log.
func (s *Session) ensureClient(ctx context.Context) error {
	s.mu.Lock()
	if s.client != nil {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	client, err := acp.Spawn(context.Background(), acp.Options{
		Command:        s.mgr.cfg.CopilotPath,
		Dir:            s.Cwd,
		OnNotification: s.onNotification,
		OnRequest:      s.onRequest,
	})
	if err != nil {
		return err
	}
	if _, err := client.Initialize(ctx); err != nil {
		client.Close()
		return fmt.Errorf("initialize: %w", err)
	}
	s.suppress.Store(true)
	err = client.LoadSession(ctx, s.acpID, s.Cwd)
	s.suppress.Store(false)
	if err != nil {
		client.Close()
		return fmt.Errorf("session/load: %w", err)
	}

	s.mu.Lock()
	s.client = client
	s.mu.Unlock()
	s.watchExit(client)
	s.mgr.cfg.Logger.Info("session resumed", "id", s.ID)
	return nil
}

// Get returns a session by id.
func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	return s, ok
}

// List returns infos for all sessions, newest first.
func (m *Manager) List() []proto.SessionInfo {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()

	out := make([]proto.SessionInfo, len(sessions))
	for i, s := range sessions {
		out[i] = s.Info()
	}
	slices.SortFunc(out, func(a, b proto.SessionInfo) int {
		return b.CreatedAt.Compare(a.CreatedAt)
	})
	return out
}

// CloseAll terminates every live session subprocess without disturbing the
// sessions' persisted statuses, so they restore as resumable.
func (m *Manager) CloseAll() {
	m.closing.Store(true)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		s.mu.Lock()
		c := s.client
		s.mu.Unlock()
		if c != nil {
			_ = c.Close()
		}
	}
}

// Remove deletes a session that is not in an active turn, releasing its
// subprocess and any persisted state.
func (m *Manager) Remove(id string) error {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("unknown session %q", id)
	}
	switch st := s.Info().Status; st {
	case StatusRunning, StatusAwaitingPermission, StatusStarting:
		m.mu.Unlock()
		return fmt.Errorf("session is %s; cancel or wait for the turn to end first", st)
	}
	delete(m.sessions, id)
	m.mu.Unlock()

	s.mu.Lock()
	c := s.client
	s.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
	_ = s.Log.Close()
	if s.dir != "" {
		if err := os.RemoveAll(s.dir); err != nil {
			m.cfg.Logger.Warn("remove session dir", "id", id, "err", err)
		}
	}
	m.cfg.Logger.Info("session removed", "id", id)
	return nil
}

// RecentDirs returns recently used working directories, most recent first.
func (m *Manager) RecentDirs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.recent))
	copy(out, m.recent)
	return out
}

func (m *Manager) recentDirsPath() string {
	if m.cfg.StateDir == "" {
		return ""
	}
	return filepath.Join(m.cfg.StateDir, "recent-dirs.json")
}

func (m *Manager) loadRecentDirs() {
	path := m.recentDirsPath()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if err := json.Unmarshal(data, &m.recent); err != nil {
		m.cfg.Logger.Warn("failed to load recent dirs", "path", path, "err", err)
		m.recent = nil
		return
	}
	if len(m.recent) > maxRecentDirs {
		m.recent = m.recent[:maxRecentDirs]
	}
}

// rememberDir moves cwd to the front of the recents list and persists it.
func (m *Manager) rememberDir(cwd string) {
	m.mu.Lock()
	next := make([]string, 0, maxRecentDirs)
	next = append(next, cwd)
	for _, dir := range m.recent {
		if dir != cwd && len(next) < maxRecentDirs {
			next = append(next, dir)
		}
	}
	m.recent = next
	path := m.recentDirsPath()

	if path == "" {
		m.mu.Unlock()
		return
	}

	if err := os.MkdirAll(m.cfg.StateDir, 0o700); err != nil {
		m.mu.Unlock()
		m.cfg.Logger.Warn("failed to create state dir", "dir", m.cfg.StateDir, "err", err)
		return
	}

	data, err := json.Marshal(next)
	if err != nil {
		m.mu.Unlock()
		m.cfg.Logger.Warn("failed to marshal recent dirs", "err", err)
		return
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		m.mu.Unlock()
		m.cfg.Logger.Warn("failed to persist recent dirs", "path", path, "err", err)
		return
	}
	m.mu.Unlock()
}

// Info snapshots the session's public state.
func (s *Session) Info() proto.SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return proto.SessionInfo{ID: s.ID, Cwd: s.Cwd, Status: s.status, CreatedAt: s.CreatedAt}
}

// SendPrompt runs one prompt turn asynchronously; progress and completion are
// reported through the event log. The busy check and the transition to
// running are atomic to prevent concurrent prompts racing past each other.
func (s *Session) SendPrompt(text string) error {
	s.mu.Lock()
	if s.status == StatusRunning || s.status == StatusAwaitingPermission {
		s.mu.Unlock()
		return errors.New("session is busy; cancel first or wait for the turn to end")
	}
	s.status = StatusRunning
	s.mu.Unlock()
	s.persistMeta()
	s.Log.Append(proto.EvtSessionState, proto.SessionState{Status: StatusRunning})

	go func() {
		// A dormant (restored) session has no subprocess yet: respawn and
		// reattach before prompting.
		resumeCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		err := s.ensureClient(resumeCtx)
		cancel()
		if err != nil {
			s.mgr.cfg.Logger.Warn("resume failed", "session", s.ID, "err", err)
			s.Log.Append(proto.EvtTurnEnded, proto.TurnEnded{StopReason: "error: " + err.Error()})
			s.setStatus(StatusError)
			return
		}
		s.mu.Lock()
		client := s.client
		s.mu.Unlock()

		res, err := client.Prompt(context.Background(), s.acpID, text)
		if err != nil {
			s.mgr.cfg.Logger.Warn("prompt failed", "session", s.ID, "err", err)
			s.Log.Append(proto.EvtTurnEnded, proto.TurnEnded{StopReason: "error: " + err.Error()})
			s.setStatus(StatusError)
			return
		}
		s.Log.Append(proto.EvtTurnEnded, proto.TurnEnded{StopReason: res.StopReason})
		s.setStatus(StatusIdle)
	}()
	return nil
}

// Approve resolves a pending permission request with the chosen option.
func (s *Session) Approve(requestID, optionID string) error {
	s.mu.Lock()
	p, ok := s.pending[requestID]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("no pending permission request %q", requestID)
	}
	select {
	case p.ch <- optionID:
		return nil
	default:
		return fmt.Errorf("permission request %q already resolved", requestID)
	}
}

// Cancel interrupts the current turn.
func (s *Session) Cancel() error {
	s.mu.Lock()
	c := s.client
	s.mu.Unlock()
	if c == nil {
		return errors.New("session has no active turn")
	}
	return c.Cancel(s.acpID)
}

func (s *Session) setStatus(status string) {
	s.mu.Lock()
	if s.status == status {
		s.mu.Unlock()
		return
	}
	s.status = status
	s.mu.Unlock()
	s.persistMeta()
	s.Log.Append(proto.EvtSessionState, proto.SessionState{Status: status})
}

// onNotification handles agent→client notifications (session/update).
func (s *Session) onNotification(method string, params json.RawMessage) {
	if method != "session/update" {
		return
	}
	if s.suppress.Load() {
		return // session/load replaying history the log already holds
	}
	var note acp.SessionNotification
	if err := json.Unmarshal(params, &note); err != nil {
		return
	}
	var kind acp.UpdateKind
	_ = json.Unmarshal(note.Update, &kind)
	s.Log.Append(proto.EvtTranscriptDelta, proto.TranscriptDelta{
		Kind: kind.SessionUpdate,
		Data: note.Update,
	})
}

// onRequest handles agent→client requests. session/request_permission blocks
// until the phone answers or the fail-safe timeout denies it.
func (s *Session) onRequest(ctx context.Context, method string, params json.RawMessage) (any, error) {
	if method != "session/request_permission" {
		return nil, &acp.RPCError{Code: -32601, Message: "method not supported: " + method}
	}

	var req acp.RequestPermissionParams
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, err
	}

	requestID := newID()
	p := &pendingPermission{ch: make(chan string, 1)}

	s.mu.Lock()
	s.pending[requestID] = p
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, requestID)
		s.mu.Unlock()
	}()

	options := make([]proto.PermissionOption, len(req.Options))
	for i, o := range req.Options {
		options[i] = proto.PermissionOption{OptionID: o.OptionID, Name: o.Name, Kind: o.Kind}
	}
	title := extractTitle(req.ToolCall)

	s.setStatus(StatusAwaitingPermission)
	s.Log.Append(proto.EvtPermissionRequest, proto.PermissionRequest{
		RequestID: requestID,
		Title:     title,
		ToolCall:  req.ToolCall,
		Options:   options,
	})
	if notify := s.mgr.cfg.OnPermissionRequest; notify != nil {
		optionIDs := make([]string, len(options))
		for i, o := range options {
			optionIDs[i] = o.OptionID
		}
		go notify(s.ID, requestID, title, optionIDs)
	}

	select {
	case optionID := <-p.ch:
		if optionID == "" {
			s.Log.Append(proto.EvtPermissionResolved, proto.PermissionResolved{RequestID: requestID, ResolvedBy: "cancel"})
			return acp.RequestPermissionResult{Outcome: acp.PermissionOutcome{Outcome: "cancelled"}}, nil
		}
		s.Log.Append(proto.EvtPermissionResolved, proto.PermissionResolved{RequestID: requestID, OptionID: optionID, ResolvedBy: "phone"})
		// The turn continues only on this path; restore running before the
		// result reaches the CLI. Every other branch returns a cancelled
		// outcome, which ends the turn — setting running there races the
		// prompt goroutine's terminal status and can strand the session.
		s.setStatus(StatusRunning)
		return acp.RequestPermissionResult{Outcome: acp.PermissionOutcome{Outcome: "selected", OptionID: optionID}}, nil

	case <-time.After(s.mgr.cfg.PermissionTimeout):
		// Fail-safe: deny when nobody answers.
		s.Log.Append(proto.EvtPermissionResolved, proto.PermissionResolved{RequestID: requestID, ResolvedBy: "timeout"})
		return acp.RequestPermissionResult{Outcome: acp.PermissionOutcome{Outcome: "cancelled"}}, nil

	case <-ctx.Done():
		return acp.RequestPermissionResult{Outcome: acp.PermissionOutcome{Outcome: "cancelled"}}, nil
	}
}

func extractTitle(toolCall json.RawMessage) string {
	var tc struct {
		Title string `json:"title"`
	}
	_ = json.Unmarshal(toolCall, &tc)
	return tc.Title
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
