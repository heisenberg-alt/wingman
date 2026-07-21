package transport_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/heisenberg-alt/wingman/daemon/internal/acptest"
	"github.com/heisenberg-alt/wingman/daemon/internal/proto"
	"github.com/heisenberg-alt/wingman/daemon/internal/securechan"
	"github.com/heisenberg-alt/wingman/daemon/internal/session"
	"github.com/heisenberg-alt/wingman/daemon/internal/transport"
	"github.com/heisenberg-alt/wingman/daemon/internal/wsconn"
)

// testClient speaks the wire protocol over any MessageConn and buffers
// envelopes so tests can wait for specific messages regardless of ordering.
type testClient struct {
	t      *testing.T
	mc     securechan.MessageConn
	nextID atomic.Int64
	inbox  chan proto.Envelope
	buf    []proto.Envelope
}

func newTestClient(t *testing.T, mc securechan.MessageConn) *testClient {
	c := &testClient{t: t, mc: mc, inbox: make(chan proto.Envelope, 256)}
	go func() {
		for {
			data, err := mc.Read(context.Background())
			if err != nil {
				close(c.inbox)
				return
			}
			var env proto.Envelope
			if json.Unmarshal(data, &env) == nil {
				c.inbox <- env
			}
		}
	}()
	return c
}

func (c *testClient) send(msgType, sessionID string, payload any) string {
	c.t.Helper()
	id := strconv.FormatInt(c.nextID.Add(1), 10)
	env := proto.Envelope{V: proto.Version, ID: id, SessionID: sessionID, Type: msgType}
	if payload != nil {
		env.Payload = proto.Marshal(payload)
	}
	data, _ := json.Marshal(env)
	if err := c.mc.Write(context.Background(), data); err != nil {
		c.t.Fatalf("write %s: %v", msgType, err)
	}
	return id
}

// waitFor returns the first buffered or incoming envelope matching pred.
func (c *testClient) waitFor(pred func(proto.Envelope) bool, timeout time.Duration) (proto.Envelope, bool) {
	c.t.Helper()
	for i, env := range c.buf {
		if pred(env) {
			c.buf = append(c.buf[:i], c.buf[i+1:]...)
			return env, true
		}
	}
	deadline := time.After(timeout)
	for {
		select {
		case env, ok := <-c.inbox:
			if !ok {
				return proto.Envelope{}, false
			}
			if pred(env) {
				return env, true
			}
			c.buf = append(c.buf, env)
		case <-deadline:
			return proto.Envelope{}, false
		}
	}
}

// call sends a command and waits for its res reply.
func (c *testClient) call(msgType, sessionID string, payload any) proto.Result {
	c.t.Helper()
	id := c.send(msgType, sessionID, payload)
	env, ok := c.waitFor(func(e proto.Envelope) bool {
		return e.Type == proto.TypeRes && e.ID == id
	}, 10*time.Second)
	if !ok {
		c.t.Fatalf("no reply to %s", msgType)
	}
	var res proto.Result
	if err := json.Unmarshal(env.Payload, &res); err != nil {
		c.t.Fatalf("bad res payload: %v", err)
	}
	return res
}

func (c *testClient) event(evtType string, timeout time.Duration) (proto.Envelope, bool) {
	c.t.Helper()
	return c.waitFor(func(e proto.Envelope) bool { return e.Type == evtType }, timeout)
}

func newServer(t *testing.T) (*transport.Server, *httptest.Server) {
	t.Helper()
	mgr := session.NewManager(session.Config{
		CopilotPath:       acptest.Build(t),
		PermissionTimeout: time.Minute,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	t.Cleanup(mgr.CloseAll)
	srv := &transport.Server{Manager: mgr}
	hts := httptest.NewServer(srv.Handler())
	t.Cleanup(hts.Close)
	return srv, hts
}

func dial(t *testing.T, hts *httptest.Server) *testClient {
	t.Helper()
	url := "ws" + strings.TrimPrefix(hts.URL, "http") + "/ws"
	ws, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return newTestClient(t, wsconn.New(ws))
}

func TestSessionLifecycleOverWebSocket(t *testing.T) {
	_, hts := newServer(t)
	c := dial(t, hts)

	// Empty list.
	res := c.call(proto.CmdSessionList, "", nil)
	if !res.OK {
		t.Fatalf("list failed: %s", res.Error)
	}
	var list proto.SessionList
	_ = json.Unmarshal(res.Data, &list)
	if len(list.Sessions) != 0 {
		t.Fatalf("expected empty list, got %d", len(list.Sessions))
	}

	// Create with a permission-triggering prompt.
	res = c.call(proto.CmdSessionCreate, "", proto.SessionCreate{Cwd: t.TempDir(), Prompt: "NEEDPERM do it"})
	if !res.OK {
		t.Fatalf("create failed: %s", res.Error)
	}
	var info proto.SessionInfo
	_ = json.Unmarshal(res.Data, &info)

	// Watch and drive the permission flow.
	if res := c.call(proto.CmdSessionWatch, info.ID, proto.SessionWatch{FromSeq: 0}); !res.OK {
		t.Fatalf("watch failed: %s", res.Error)
	}
	permEnv, ok := c.event(proto.EvtPermissionRequest, 10*time.Second)
	if !ok {
		t.Fatal("no permission.request event")
	}
	var perm proto.PermissionRequest
	_ = json.Unmarshal(permEnv.Payload, &perm)

	if res := c.call(proto.CmdSessionApprove, info.ID, proto.SessionApprove{
		RequestID: perm.RequestID, OptionID: "allow_once",
	}); !res.OK {
		t.Fatalf("approve failed: %s", res.Error)
	}

	turnEnv, ok := c.event(proto.EvtTurnEnded, 10*time.Second)
	if !ok {
		t.Fatal("no turn.ended event")
	}
	lastSeq := turnEnv.Seq

	// Resume from a later seq on a fresh connection: only newer events replay.
	c2 := dial(t, hts)
	fromSeq := lastSeq - 1
	if res := c2.call(proto.CmdSessionWatch, info.ID, proto.SessionWatch{FromSeq: fromSeq}); !res.OK {
		t.Fatalf("watch (resume) failed: %s", res.Error)
	}
	env, ok := c2.waitFor(func(e proto.Envelope) bool { return e.Seq != 0 }, 10*time.Second)
	if !ok {
		t.Fatal("no replayed event after resume")
	}
	if env.Seq != fromSeq+1 {
		t.Errorf("first replayed seq = %d, want %d", env.Seq, fromSeq+1)
	}
}

func TestUnknownCommandReturnsError(t *testing.T) {
	_, hts := newServer(t)
	c := dial(t, hts)

	res := c.call("bogus.command", "", nil)
	if res.OK || !strings.Contains(res.Error, "unknown command") {
		t.Errorf("res = %+v, want unknown command error", res)
	}
}

func TestCommandsForUnknownSessionFail(t *testing.T) {
	_, hts := newServer(t)
	c := dial(t, hts)

	res := c.call(proto.CmdSessionPrompt, "no-such-session", proto.SessionPrompt{Text: "hi"})
	if res.OK || !strings.Contains(res.Error, "unknown session") {
		t.Errorf("res = %+v, want unknown session error", res)
	}
}

func TestDirsListAndSessionRemove(t *testing.T) {
	srv, hts := newServer(t)
	c := dial(t, hts)

	cwd := t.TempDir()
	res := c.call(proto.CmdSessionCreate, "", proto.SessionCreate{Cwd: cwd})
	if !res.OK {
		t.Fatalf("create: %s", res.Error)
	}
	var info proto.SessionInfo
	_ = json.Unmarshal(res.Data, &info)

	// dirs.list surfaces the cwd, most recent first.
	res = c.call(proto.CmdDirsList, "", nil)
	if !res.OK {
		t.Fatalf("dirs.list: %s", res.Error)
	}
	var dirs proto.DirsList
	_ = json.Unmarshal(res.Data, &dirs)
	if len(dirs.Dirs) == 0 || dirs.Dirs[0] != cwd {
		t.Errorf("dirs = %v, want %q first", dirs.Dirs, cwd)
	}

	// Removing a live session is rejected.
	if res := c.call(proto.CmdSessionRemove, info.ID, nil); res.OK {
		t.Error("removed a non-terminal session")
	}
}
