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
	"sync"
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
	Logger            *slog.Logger
}

// Manager owns all sessions in this daemon.
type Manager struct {
	cfg      Config
	mu       sync.Mutex
	sessions map[string]*Session
}

// NewManager creates a Manager.
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
	return &Manager{cfg: cfg, sessions: make(map[string]*Session)}
}

// Session is one live Copilot ACP session.
type Session struct {
	ID        string
	Cwd       string
	CreatedAt time.Time
	Log       *Log

	mgr    *Manager
	client *acp.Client
	acpID  string

	mu      sync.Mutex
	status  string
	pending map[string]*pendingPermission
}

type pendingPermission struct {
	ch chan string // buffered(1); receives chosen optionId, "" = cancel
}

// Create spawns a copilot ACP subprocess, performs the handshake, and opens a
// session rooted at cwd.
func (m *Manager) Create(ctx context.Context, cwd string) (*Session, error) {
	s := &Session{
		ID:        newID(),
		Cwd:       cwd,
		CreatedAt: time.Now().UTC(),
		Log:       NewLog(),
		mgr:       m,
		status:    StatusStarting,
		pending:   make(map[string]*pendingPermission),
	}

	client, err := acp.Spawn(ctx, acp.Options{
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

	// Reap: when the subprocess exits, a session that was idle completed
	// normally; anything else is an error.
	go func() {
		<-client.Done()
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

	m.mu.Lock()
	m.sessions[s.ID] = s
	m.mu.Unlock()

	m.cfg.Logger.Info("session created", "id", s.ID, "cwd", cwd)
	return s, nil
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
	defer m.mu.Unlock()
	out := make([]proto.SessionInfo, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s.Info())
	}
	return out
}

// CloseAll terminates every session subprocess.
func (m *Manager) CloseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		_ = s.client.Close()
	}
}

// Info snapshots the session's public state.
func (s *Session) Info() proto.SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return proto.SessionInfo{ID: s.ID, Cwd: s.Cwd, Status: s.status, CreatedAt: s.CreatedAt}
}

// SendPrompt runs one prompt turn asynchronously; progress and completion are
// reported through the event log.
func (s *Session) SendPrompt(text string) error {
	s.mu.Lock()
	if s.status == StatusRunning || s.status == StatusAwaitingPermission {
		s.mu.Unlock()
		return errors.New("session is busy; cancel first or wait for the turn to end")
	}
	s.mu.Unlock()

	s.setStatus(StatusRunning)
	go func() {
		res, err := s.client.Prompt(context.Background(), s.acpID, text)
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
	return s.client.Cancel(s.acpID)
}

func (s *Session) setStatus(status string) {
	s.mu.Lock()
	if s.status == status {
		s.mu.Unlock()
		return
	}
	s.status = status
	s.mu.Unlock()
	s.Log.Append(proto.EvtSessionState, proto.SessionState{Status: status})
}

// onNotification handles agent→client notifications (session/update).
func (s *Session) onNotification(method string, params json.RawMessage) {
	if method != "session/update" {
		return
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
	defer s.setStatus(StatusRunning)

	select {
	case optionID := <-p.ch:
		if optionID == "" {
			s.Log.Append(proto.EvtPermissionResolved, proto.PermissionResolved{RequestID: requestID, ResolvedBy: "cancel"})
			return acp.RequestPermissionResult{Outcome: acp.PermissionOutcome{Outcome: "cancelled"}}, nil
		}
		s.Log.Append(proto.EvtPermissionResolved, proto.PermissionResolved{RequestID: requestID, OptionID: optionID, ResolvedBy: "phone"})
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
