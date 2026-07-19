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
// message; on success they are registered and served immediately.
func (ss *SecureServer) ServeConn(ctx context.Context, raw securechan.MessageConn) {
	sc, err := securechan.Respond(ctx, raw, ss.Static)
	if err != nil {
		ss.Logger.Warn("handshake failed", "err", err)
		_ = raw.Close()
		return
	}

	if !ss.Registry.IsAuthorized(sc.PeerStatic()) {
		if !ss.pair(ctx, sc) {
			_ = sc.Close()
			return
		}
	}
	ss.Server.ServeConn(ctx, sc)
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
		res := proto.Result{OK: err == nil}
		if err != nil {
			res.Error = err.Error()
		}
		out, _ := json.Marshal(proto.Envelope{
			V:       proto.Version,
			ID:      env.ID,
			Type:    proto.TypeRes,
			Payload: proto.Marshal(res),
		})
		_ = sc.Write(ctx, out)
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
// secured peer connection at a time and reconnecting with backoff.
func RunRelayHost(ctx context.Context, relayURL, room string, ss *SecureServer) {
	backoff := time.Second
	for ctx.Err() == nil {
		ws, _, err := websocket.Dial(ctx, relayURL+"/v1/host?room="+url.QueryEscape(room), nil)
		if err != nil {
			ss.Logger.Warn("relay dial failed", "err", err, "retry_in", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		ss.Logger.Info("connected to relay", "url", relayURL, "room", room)
		// Blocks until the peer disconnects or the relay drops us.
		ss.ServeConn(ctx, wsconn.New(ws))
	}
}
