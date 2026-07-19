package securechan

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

// pipeConn is an in-memory MessageConn for tests.
type pipeConn struct {
	in  chan []byte
	out chan []byte
}

func newPipe() (*pipeConn, *pipeConn) {
	a := make(chan []byte, 16)
	b := make(chan []byte, 16)
	return &pipeConn{in: a, out: b}, &pipeConn{in: b, out: a}
}

func (p *pipeConn) Read(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case data, ok := <-p.in:
		if !ok {
			return nil, errors.New("closed")
		}
		return data, nil
	}
}

func (p *pipeConn) Write(ctx context.Context, data []byte) error {
	cp := make([]byte, len(data))
	copy(cp, data)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case p.out <- cp:
		return nil
	}
}

func (p *pipeConn) Close() error { return nil }

func TestHandshakeAndRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	clientKey, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	serverKey, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}

	clientPipe, serverPipe := newPipe()

	type result struct {
		conn *Conn
		err  error
	}
	serverCh := make(chan result, 1)
	go func() {
		conn, err := Respond(ctx, serverPipe, serverKey)
		serverCh <- result{conn, err}
	}()

	clientConn, err := Initiate(ctx, clientPipe, clientKey, serverKey.Public)
	if err != nil {
		t.Fatalf("initiate: %v", err)
	}
	serverRes := <-serverCh
	if serverRes.err != nil {
		t.Fatalf("respond: %v", serverRes.err)
	}
	serverConn := serverRes.conn

	// Mutual authentication.
	if !bytes.Equal(clientConn.PeerStatic(), serverKey.Public) {
		t.Error("client saw wrong server key")
	}
	if !bytes.Equal(serverConn.PeerStatic(), clientKey.Public) {
		t.Error("server saw wrong client key")
	}

	// Bidirectional round trip.
	for i := 0; i < 3; i++ {
		if err := clientConn.Write(ctx, []byte("ping")); err != nil {
			t.Fatal(err)
		}
		got, err := serverConn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "ping" {
			t.Fatalf("got %q", got)
		}
		if err := serverConn.Write(ctx, []byte("pong")); err != nil {
			t.Fatal(err)
		}
		got, err = clientConn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "pong" {
			t.Fatalf("got %q", got)
		}
	}
}

func TestTamperedCiphertextFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	clientKey, _ := GenerateKey()
	serverKey, _ := GenerateKey()
	clientPipe, serverPipe := newPipe()

	go func() {
		conn, err := Respond(ctx, serverPipe, serverKey)
		if err != nil {
			return
		}
		_ = conn.Write(ctx, []byte("secret"))
	}()

	clientConn, err := Initiate(ctx, clientPipe, clientKey, nil)
	if err != nil {
		t.Fatalf("initiate: %v", err)
	}

	// Intercept and flip a bit before decryption.
	ct, err := clientPipe.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ct[0] ^= 0xFF
	if _, err := clientConn.dec.Decrypt(nil, nil, ct); err == nil {
		t.Fatal("tampered ciphertext decrypted successfully")
	}
}

func TestWrongExpectedPeerRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	clientKey, _ := GenerateKey()
	serverKey, _ := GenerateKey()
	otherKey, _ := GenerateKey()
	clientPipe, serverPipe := newPipe()

	go func() { _, _ = Respond(ctx, serverPipe, serverKey) }()

	if _, err := Initiate(ctx, clientPipe, clientKey, otherKey.Public); err == nil {
		t.Fatal("handshake with wrong pinned key succeeded")
	}
}
