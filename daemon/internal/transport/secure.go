package transport

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/coder/websocket"
	"github.com/flynn/noise"

	"github.com/heisenberg-alt/wingman/daemon/internal/pairing"
	"github.com/heisenberg-alt/wingman/daemon/internal/proto"
	"github.com/heisenberg-alt/wingman/daemon/internal/securechan"
	"github.com/heisenberg-alt/wingman/daemon/internal/wsconn"
)

// SecureServer wraps the protocol Server behind a Noise XX handshake and the
// paired-device authorization gate. It serves both the external LAN listener
// and relay-carried connections.
type SecureServer struct {
	Server   *Server
	Static   noise.DHKey
	Registry *pairing.Registry
	Tokens   *pairing.Tokens
	Logger   *slog.Logger
}

// Handler returns the HTTP handler for the external (LAN) /ws endpoint.
func (ss *SecureServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ss.ServeConn(r.Context(), wsconn.New(conn))
	})
	return mux
}

// ServeConn performs the handshake and authorization, then runs the protocol.
// Unpaired peers must present a valid single-use pairing token as their first
// message; on success they are registered and served immediately. The return
// value reports whether the Noise handshake completed — callers use it to
// distinguish real peer interactions from transport flaps.
func (ss *SecureServer) ServeConn(ctx context.Context, raw securechan.MessageConn) bool {
	sc, err := securechan.Respond(ctx, raw, ss.Static)
	if err != nil {
		ss.Logger.Warn("handshake failed", "err", err)
		_ = raw.Close()
		return false
	}

	if !ss.Registry.IsAuthorized(sc.PeerStatic()) {
		if !ss.pair(ctx, sc) {
			_ = sc.Close()
			return true
		}
	}
	ss.Server.ServeConn(ctx, sc)
	return true
}

// pair handles the pairing exchange for an unknown peer. It returns true if
// the device was registered.
func (ss *SecureServer) pair(ctx context.Context, sc *securechan.Conn) bool {
	data, err := sc.Read(ctx)
	if err != nil {
		return false
	}
	var env proto.Envelope
	if err := json.Unmarshal(data, &env); err != nil || env.Type != proto.CmdPairRequest {
		ss.Logger.Warn("unpaired peer sent non-pairing message; closing")
		return false
	}
	var req proto.PairRequest
	if err := json.Unmarshal(env.Payload, &req); err != nil {
		return false
	}

	reply := func(err error) {
		_ = sc.Write(ctx, proto.ResultEnvelope(env.ID, nil, err))
	}

	if !ss.Tokens.Redeem(req.Token) {
		reply(errors.New("invalid or expired pairing token"))
		return false
	}
	if err := ss.Registry.Add(req.DeviceName, sc.PeerStatic()); err != nil {
		reply(err)
		return false
	}
	ss.Logger.Info("device paired", "name", req.DeviceName)
	reply(nil)
	return true
}

// RunRelayHost maintains the daemon's connection to the relay, serving one
// secured peer connection at a time. relayToken, when set, authenticates the
// connection to the relay itself. It reconnects immediately after serving
// a real peer (normal operations are often sub-second), and backs off only on
// dial failures or connections that never complete a handshake, so a
// misbehaving relay cannot induce a hot loop.
func RunRelayHost(ctx context.Context, relayURL, room, relayToken string, ss *SecureServer) {
	hostURL := relayURL + "/v1/host?room=" + url.QueryEscape(room)
	if relayToken != "" {
		hostURL += "&token=" + url.QueryEscape(relayToken)
	}

	backoff := time.Second
	wait := func() bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
		return true
	}

	for ctx.Err() == nil {
		ws, _, err := websocket.Dial(ctx, hostURL, nil)
		if err != nil {
			ss.Logger.Warn("relay dial failed", "err", err, "retry_in", backoff)
			if !wait() {
				return
			}
			continue
		}
		ss.Logger.Info("connected to relay", "url", relayURL, "room", room)

		// Blocks until the peer disconnects or the relay drops us.
		if ss.ServeConn(ctx, wsconn.New(ws)) {
			backoff = time.Second
		} else if !wait() {
			return
		}
	}
}
