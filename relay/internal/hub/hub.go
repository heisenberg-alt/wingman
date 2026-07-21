// Package hub implements the relay's rendezvous logic: it pairs one host
// (daemon) and one client (phone) per room and pipes opaque frames between
// them. Payloads are end-to-end encrypted; the relay never sees plaintext.
//
// Hardening for public exposure: optional bearer-token auth, keepalive pings
// on parked host connections, per-IP connection rate limiting, and a cap on
// concurrent rooms.
package hub

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Config tunes the hub. Zero values select safe defaults.
type Config struct {
	// Token, when non-empty, is required (constant-time compared) as the
	// "token" query parameter on /v1/host and /v1/join.
	Token string
	// MaxRooms caps concurrently hosted rooms. Default 128.
	MaxRooms int
	// PingInterval is the keepalive cadence on parked host connections,
	// which also detects dead hosts. Default 30s.
	PingInterval time.Duration
	// PerIPPerMinute limits new connections per client IP. Default 60.
	PerIPPerMinute int
}

// Hub routes connections into rooms.
type Hub struct {
	cfg    Config
	logger *slog.Logger

	mu    sync.Mutex
	rooms map[string]*room

	limiterMu sync.Mutex
	limiter   map[string]*bucket
}

type room struct {
	host *websocket.Conn
	// done is closed when the host connection is torn down.
	done     chan struct{}
	closeOne sync.Once
	// claimed is set when a client takes the room, stopping the keepalive.
	claimed chan struct{}
}

func (r *room) teardown() {
	r.closeOne.Do(func() { close(r.done) })
}

type bucket struct {
	tokens   float64
	lastFill time.Time
}

// New creates a Hub.
func New(logger *slog.Logger, cfg Config) *Hub {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.MaxRooms <= 0 {
		cfg.MaxRooms = 128
	}
	if cfg.PingInterval <= 0 {
		cfg.PingInterval = 30 * time.Second
	}
	if cfg.PerIPPerMinute <= 0 {
		cfg.PerIPPerMinute = 60
	}
	return &Hub{
		cfg:     cfg,
		logger:  logger,
		rooms:   make(map[string]*room),
		limiter: make(map[string]*bucket),
	}
}

// Handler returns the relay's HTTP routes.
func (h *Hub) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/host", h.guard(h.handleHost))
	mux.HandleFunc("/v1/join", h.guard(h.handleJoin))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

// guard applies rate limiting and token auth before the endpoint handler.
func (h *Hub) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.allow(clientIP(r)) {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		if h.cfg.Token != "" {
			presented := r.URL.Query().Get("token")
			if subtle.ConstantTimeCompare([]byte(presented), []byte(h.cfg.Token)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next(w, r)
	}
}

// allow implements a per-IP token bucket.
func (h *Hub) allow(ip string) bool {
	h.limiterMu.Lock()
	defer h.limiterMu.Unlock()

	now := time.Now()
	b, ok := h.limiter[ip]
	if !ok {
		// Opportunistic cleanup keeps the map bounded.
		if len(h.limiter) > 4096 {
			h.limiter = make(map[string]*bucket)
		}
		b = &bucket{tokens: float64(h.cfg.PerIPPerMinute), lastFill: now}
		h.limiter[ip] = b
	}
	refill := now.Sub(b.lastFill).Minutes() * float64(h.cfg.PerIPPerMinute)
	b.tokens = min(b.tokens+refill, float64(h.cfg.PerIPPerMinute))
	b.lastFill = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func clientIP(r *http.Request) string {
	// Behind a proxy (Fly, Caddy), the real IP arrives in a header.
	if fwd := r.Header.Get("Fly-Client-IP"); fwd != "" {
		return fwd
	}
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		return fwd
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// handleHost registers a daemon connection for a room and parks until the
// connection is consumed by a join or torn down. A reconnecting host replaces
// any stale one. While parked, keepalive pings hold proxy connections open
// and reap dead hosts.
func (h *Hub) handleHost(w http.ResponseWriter, r *http.Request) {
	roomID := r.URL.Query().Get("room")
	if roomID == "" {
		http.Error(w, "missing room", http.StatusBadRequest)
		return
	}

	h.mu.Lock()
	if len(h.rooms) >= h.cfg.MaxRooms {
		if _, exists := h.rooms[roomID]; !exists {
			h.mu.Unlock()
			http.Error(w, "room capacity reached", http.StatusServiceUnavailable)
			return
		}
	}
	h.mu.Unlock()

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(16 << 20)

	rm := &room{host: conn, done: make(chan struct{}), claimed: make(chan struct{})}
	h.mu.Lock()
	if old, exists := h.rooms[roomID]; exists {
		old.teardown()
	}
	h.rooms[roomID] = rm
	h.mu.Unlock()

	h.logger.Info("host connected", "room", roomID)

	// Keepalive: ping the parked host until a client claims the room or the
	// host dies. A failed ping tears the room down so the daemon redials.
	go func() {
		ticker := time.NewTicker(h.cfg.PingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-rm.claimed:
				return
			case <-rm.done:
				return
			case <-ticker.C:
				// Nothing reads a parked host connection, and coder/websocket
				// only surfaces pongs to Ping during a concurrent Read — so a
				// pong confirmation can never arrive here. The ping still
				// matters: it keeps proxy connections open (the daemon's
				// auto-pong makes traffic bidirectional). Treat a pong
				// deadline as healthy; tear down only if the ping cannot be
				// written at all. Truly dead hosts are reaped when the daemon
				// redials and replaces the room.
				pingCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				err := conn.Ping(pingCtx)
				cancel()
				if err != nil && !errors.Is(err, context.DeadlineExceeded) {
					h.logger.Info("host keepalive failed", "room", roomID)
					rm.teardown()
					return
				}
			}
		}
	}()

	// Park until the pump (started by a join) finishes or teardown.
	select {
	case <-rm.done:
	case <-r.Context().Done():
	}

	h.mu.Lock()
	if h.rooms[roomID] == rm {
		delete(h.rooms, roomID)
	}
	h.mu.Unlock()
	_ = conn.Close(websocket.StatusNormalClosure, "")
	h.logger.Info("host disconnected", "room", roomID)
}

// handleJoin connects a phone to the room's host and pipes frames both ways
// until either side disconnects.
func (h *Hub) handleJoin(w http.ResponseWriter, r *http.Request) {
	roomID := r.URL.Query().Get("room")
	if roomID == "" {
		http.Error(w, "missing room", http.StatusBadRequest)
		return
	}

	h.mu.Lock()
	rm := h.rooms[roomID]
	if rm != nil {
		// Claim the room: one client at a time.
		delete(h.rooms, roomID)
	}
	h.mu.Unlock()
	if rm == nil {
		http.Error(w, "no host for room", http.StatusServiceUnavailable)
		return
	}
	close(rm.claimed)

	client, err := websocket.Accept(w, r, nil)
	if err != nil {
		rm.teardown()
		return
	}
	client.SetReadLimit(16 << 20)

	h.logger.Info("client joined", "room", roomID)

	ctx, cancel := context.WithCancel(r.Context())
	var wg sync.WaitGroup
	wg.Add(2)
	go pump(ctx, cancel, &wg, rm.host, client)
	go pump(ctx, cancel, &wg, client, rm.host)
	wg.Wait()

	_ = client.Close(websocket.StatusNormalClosure, "")
	rm.teardown() // tears down the host handler, forcing the daemon to redial
	h.logger.Info("client left", "room", roomID)
}

// pump copies messages from src to dst until either side fails.
func pump(ctx context.Context, cancel context.CancelFunc, wg *sync.WaitGroup, src, dst *websocket.Conn) {
	defer wg.Done()
	defer cancel()
	for {
		typ, data, err := src.Read(ctx)
		if err != nil {
			return
		}
		if err := dst.Write(ctx, typ, data); err != nil {
			return
		}
	}
}
