// Package hub implements the relay's rendezvous logic: it pairs one host
// (daemon) and one client (phone) per room and pipes opaque frames between
// them. Payloads are end-to-end encrypted; the relay never sees plaintext.
package hub

import (
	"context"
	"log/slog"
	"net/http"
	"sync"

	"github.com/coder/websocket"
)

// Hub routes connections into rooms.
type Hub struct {
	Logger *slog.Logger

	mu    sync.Mutex
	rooms map[string]*room
}

type room struct {
	host *websocket.Conn
	// done is closed when the host connection is torn down.
	done     chan struct{}
	closeOne sync.Once
}

func (r *room) teardown() {
	r.closeOne.Do(func() { close(r.done) })
}

// New creates an empty Hub.
func New(logger *slog.Logger) *Hub {
	if logger == nil {
		logger = slog.Default()
	}
	return &Hub{Logger: logger, rooms: make(map[string]*room)}
}

// Handler returns the relay's HTTP routes.
func (h *Hub) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/host", h.handleHost)
	mux.HandleFunc("/v1/join", h.handleJoin)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

// handleHost registers a daemon connection for a room and parks until the
// connection is consumed by a join or torn down. A reconnecting host replaces
// any stale one, since a dead parked connection cannot be detected reliably.
func (h *Hub) handleHost(w http.ResponseWriter, r *http.Request) {
	roomID := r.URL.Query().Get("room")
	if roomID == "" {
		http.Error(w, "missing room", http.StatusBadRequest)
		return
	}

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(16 << 20)

	rm := &room{host: conn, done: make(chan struct{})}
	h.mu.Lock()
	if old, exists := h.rooms[roomID]; exists {
		old.teardown()
	}
	h.rooms[roomID] = rm
	h.mu.Unlock()

	h.Logger.Info("host connected", "room", roomID)

	// Park until the pump (started by a join) finishes or the client
	// disconnects without ever joining.
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
	h.Logger.Info("host disconnected", "room", roomID)
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

	client, err := websocket.Accept(w, r, nil)
	if err != nil {
		rm.teardown()
		return
	}
	client.SetReadLimit(16 << 20)

	h.Logger.Info("client joined", "room", roomID)

	ctx, cancel := context.WithCancel(r.Context())
	var wg sync.WaitGroup
	wg.Add(2)
	go pump(ctx, cancel, &wg, rm.host, client)
	go pump(ctx, cancel, &wg, client, rm.host)
	wg.Wait()

	_ = client.Close(websocket.StatusNormalClosure, "")
	rm.teardown() // tears down the host handler, forcing the daemon to redial
	h.Logger.Info("client left", "room", roomID)
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
