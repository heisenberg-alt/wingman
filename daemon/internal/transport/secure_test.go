package transport_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/heisenberg-alt/wingman/daemon/internal/acptest"
	"github.com/heisenberg-alt/wingman/daemon/internal/pairing"
	"github.com/heisenberg-alt/wingman/daemon/internal/proto"
	"github.com/heisenberg-alt/wingman/daemon/internal/securechan"
	"github.com/heisenberg-alt/wingman/daemon/internal/session"
	"github.com/heisenberg-alt/wingman/daemon/internal/transport"
)

// chanConn is an in-memory MessageConn for handshake tests.
type chanConn struct {
	in, out chan []byte
	done    chan struct{}
}

func newChanPipe() (*chanConn, *chanConn) {
	a := make(chan []byte, 64)
	b := make(chan []byte, 64)
	done := make(chan struct{})
	return &chanConn{in: a, out: b, done: done}, &chanConn{in: b, out: a, done: done}
}

func (c *chanConn) Read(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, errors.New("closed")
	case data := <-c.in:
		return data, nil
	}
}

func (c *chanConn) Write(ctx context.Context, data []byte) error {
	cp := make([]byte, len(data))
	copy(cp, data)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return errors.New("closed")
	case c.out <- cp:
		return nil
	}
}

func (c *chanConn) Close() error {
	select {
	case <-c.done:
	default:
		close(c.done)
	}
	return nil
}

func newSecureServer(t *testing.T) (*transport.SecureServer, *pairing.Registry, *pairing.Tokens, []byte) {
	t.Helper()
	static, err := pairing.LoadOrCreateKey(filepath.Join(t.TempDir(), "static.json"))
	if err != nil {
		t.Fatal(err)
	}
	registry, err := pairing.LoadRegistry(filepath.Join(t.TempDir(), "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	tokens := &pairing.Tokens{}
	mgr := session.NewManager(session.Config{
		CopilotPath: acptest.Build(t),
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	t.Cleanup(mgr.CloseAll)
	ss := &transport.SecureServer{
		Server:   &transport.Server{Manager: mgr},
		Static:   static,
		Registry: registry,
		Tokens:   tokens,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return ss, registry, tokens, static.Public
}

// connectSecure performs the client-side handshake against a SecureServer
// served over an in-memory pipe.
func connectSecure(t *testing.T, ss *transport.SecureServer, daemonPub []byte) (*securechan.Conn, []byte) {
	t.Helper()
	clientKey, err := securechan.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	clientSide, serverSide := newChanPipe()
	go ss.ServeConn(context.Background(), serverSide)

	sc, err := securechan.Initiate(context.Background(), clientSide, clientKey, daemonPub)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	return sc, clientKey.Public
}

func TestPairingWithValidTokenThenServes(t *testing.T) {
	ss, registry, tokens, daemonPub := newSecureServer(t)
	token := tokens.Issue(time.Minute)

	sc, clientPub := connectSecure(t, ss, daemonPub)
	c := newTestClient(t, sc)

	res := c.call(proto.CmdPairRequest, "", proto.PairRequest{Token: token, DeviceName: "test-phone"})
	if !res.OK {
		t.Fatalf("pairing failed: %s", res.Error)
	}
	if !registry.IsAuthorized(clientPub) {
		t.Error("client key not registered after pairing")
	}

	// The same connection is served immediately after pairing.
	if res := c.call(proto.CmdSessionList, "", nil); !res.OK {
		t.Errorf("list after pairing failed: %s", res.Error)
	}
}

func TestPairingWithInvalidTokenRejected(t *testing.T) {
	ss, registry, _, daemonPub := newSecureServer(t)

	sc, clientPub := connectSecure(t, ss, daemonPub)
	c := newTestClient(t, sc)

	res := c.call(proto.CmdPairRequest, "", proto.PairRequest{Token: "bogus", DeviceName: "evil"})
	if res.OK || !strings.Contains(res.Error, "invalid or expired") {
		t.Errorf("res = %+v, want invalid token error", res)
	}
	if registry.IsAuthorized(clientPub) {
		t.Error("client registered despite bad token")
	}
}

func TestUnpairedNonPairMessageCloses(t *testing.T) {
	ss, _, _, daemonPub := newSecureServer(t)

	sc, _ := connectSecure(t, ss, daemonPub)
	c := newTestClient(t, sc)

	// First message is not pair.request: the server must close without reply.
	c.send(proto.CmdSessionList, "", nil)
	if _, ok := c.waitFor(func(proto.Envelope) bool { return true }, 500*time.Millisecond); ok {
		t.Error("server replied to an unpaired non-pairing message")
	}
}

func TestPairedDeviceSkipsGate(t *testing.T) {
	ss, registry, _, daemonPub := newSecureServer(t)

	clientKey, err := securechan.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Add("pre-paired", clientKey.Public); err != nil {
		t.Fatal(err)
	}

	clientSide, serverSide := newChanPipe()
	go ss.ServeConn(context.Background(), serverSide)
	sc, err := securechan.Initiate(context.Background(), clientSide, clientKey, daemonPub)
	if err != nil {
		t.Fatal(err)
	}
	c := newTestClient(t, sc)

	if res := c.call(proto.CmdSessionList, "", nil); !res.OK {
		t.Errorf("paired device list failed: %s", res.Error)
	}
}
