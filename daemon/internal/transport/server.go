// Package transport serves the Wingman wire protocol. The protocol handler is
// transport-agnostic: it speaks over any securechan.MessageConn, fed by the
// loopback WebSocket listener, the Noise-secured external listener, or the
// relay dialer.
package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/coder/websocket"

	"github.com/heisenberg-alt/wingman/daemon/internal/proto"
	"github.com/heisenberg-alt/wingman/daemon/internal/securechan"
	"github.com/heisenberg-alt/wingman/daemon/internal/session"
	"github.com/heisenberg-alt/wingman/daemon/internal/wsconn"
)

// Server handles clients speaking the Wingman protocol.
type Server struct {
	Manager *session.Manager
}

// Handler returns the HTTP handler for the loopback /ws endpoint.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		s.ServeConn(r.Context(), wsconn.New(conn))
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

// ServeConn runs the protocol loop over mc until the connection closes. It
// closes mc before returning. Commands are dispatched on their own goroutines
// so a slow operation (e.g. session.create spawning a subprocess) cannot
// block permission approvals arriving on the same connection.
func (s *Server) ServeConn(ctx context.Context, mc securechan.MessageConn) {
	c := &client{
		srv:     s,
		mc:      mc,
		watches: make(map[string]func()),
	}
	defer c.close()
	for {
		data, err := mc.Read(ctx)
		if err != nil {
			return
		}
		var env proto.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		go c.handle(ctx, env)
	}
}

type client struct {
	srv *Server
	mc  securechan.MessageConn

	writeMu sync.Mutex

	mu      sync.Mutex
	watches map[string]func() // sessionID → unsubscribe
}

func (c *client) close() {
	c.mu.Lock()
	for _, cancel := range c.watches {
		cancel()
	}
	c.watches = map[string]func(){}
	c.mu.Unlock()
	_ = c.mc.Close()
}

func (c *client) handle(ctx context.Context, env proto.Envelope) {
	switch env.Type {
	case proto.CmdSessionList:
		c.reply(ctx, env, proto.SessionList{Sessions: c.srv.Manager.List()}, nil)

	case proto.CmdSessionCreate:
		var p proto.SessionCreate
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			c.reply(ctx, env, nil, err)
			return
		}
		sess, err := c.srv.Manager.Create(ctx, p.Cwd)
		if err != nil {
			c.reply(ctx, env, nil, err)
			return
		}
		if p.Prompt != "" {
			if err := sess.SendPrompt(p.Prompt); err != nil {
				c.reply(ctx, env, nil, err)
				return
			}
		}
		c.reply(ctx, env, sess.Info(), nil)

	case proto.CmdSessionPrompt:
		sess, err := c.session(env)
		if err == nil {
			var p proto.SessionPrompt
			if uerr := json.Unmarshal(env.Payload, &p); uerr != nil {
				err = uerr
			} else {
				err = sess.SendPrompt(p.Text)
			}
		}
		c.reply(ctx, env, nil, err)

	case proto.CmdSessionApprove:
		sess, err := c.session(env)
		if err == nil {
			var p proto.SessionApprove
			if uerr := json.Unmarshal(env.Payload, &p); uerr != nil {
				err = uerr
			} else {
				err = sess.Approve(p.RequestID, p.OptionID)
			}
		}
		c.reply(ctx, env, nil, err)

	case proto.CmdSessionCancel:
		sess, err := c.session(env)
		if err == nil {
			err = sess.Cancel()
		}
		c.reply(ctx, env, nil, err)

	case proto.CmdSessionWatch:
		sess, err := c.session(env)
		if err != nil {
			c.reply(ctx, env, nil, err)
			return
		}
		var p proto.SessionWatch
		_ = json.Unmarshal(env.Payload, &p)
		c.watch(sess, p.FromSeq)
		c.reply(ctx, env, nil, nil)

	case proto.CmdSessionUnwatch:
		c.mu.Lock()
		if cancel, ok := c.watches[env.SessionID]; ok {
			cancel()
			delete(c.watches, env.SessionID)
		}
		c.mu.Unlock()
		c.reply(ctx, env, nil, nil)

	default:
		c.reply(ctx, env, nil, fmt.Errorf("unknown command %q", env.Type))
	}
}

func (c *client) session(env proto.Envelope) (*session.Session, error) {
	sess, ok := c.srv.Manager.Get(env.SessionID)
	if !ok {
		return nil, fmt.Errorf("unknown session %q", env.SessionID)
	}
	return sess, nil
}

// watch replays events after fromSeq, then streams live events. Replay and
// live delivery are serialized per session to preserve seq ordering.
func (c *client) watch(sess *session.Session, fromSeq uint64) {
	c.mu.Lock()
	if cancel, ok := c.watches[sess.ID]; ok {
		cancel() // replace an existing watch
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.watches[sess.ID] = cancel
	c.mu.Unlock()

	go func() {
		live, unsub := sess.Log.Subscribe()
		defer unsub()

		lastSeq := fromSeq
		for _, evt := range sess.Log.Since(fromSeq) {
			if err := c.sendEvent(ctx, sess.ID, evt); err != nil {
				return
			}
			lastSeq = evt.Seq
		}
		for {
			select {
			case <-ctx.Done():
				return
			case evt, ok := <-live:
				if !ok {
					return
				}
				if evt.Seq <= lastSeq {
					continue // already replayed
				}
				// Fill any gap caused by drops on the live channel.
				if evt.Seq > lastSeq+1 {
					for _, missed := range sess.Log.Since(lastSeq) {
						if missed.Seq >= evt.Seq {
							break
						}
						if err := c.sendEvent(ctx, sess.ID, missed); err != nil {
							return
						}
					}
				}
				if err := c.sendEvent(ctx, sess.ID, evt); err != nil {
					return
				}
				lastSeq = evt.Seq
			}
		}
	}()
}

func (c *client) sendEvent(ctx context.Context, sessionID string, evt session.Event) error {
	return c.send(ctx, proto.Envelope{
		V:         proto.Version,
		SessionID: sessionID,
		Seq:       evt.Seq,
		Type:      evt.Type,
		Payload:   evt.Payload,
	})
}

func (c *client) reply(ctx context.Context, cmd proto.Envelope, data any, err error) {
	_ = c.sendBytes(ctx, proto.ResultEnvelope(cmd.ID, data, err))
}

func (c *client) send(ctx context.Context, env proto.Envelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return c.sendBytes(ctx, data)
}

func (c *client) sendBytes(ctx context.Context, data []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.mc.Write(ctx, data)
}
