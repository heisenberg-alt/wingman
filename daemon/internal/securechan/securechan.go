// Package securechan provides an end-to-end encrypted message channel built
// on the Noise XX handshake (X25519, ChaCha20-Poly1305, SHA-256). Both sides
// authenticate with static keys; the daemon verifies the phone against its
// device registry and the phone pins the daemon key obtained during pairing.
//
// The cipher suite is chosen to be implementable with Apple CryptoKit
// primitives (Curve25519, ChaChaPoly, SHA256) on iOS.
package securechan

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"sync"

	"github.com/flynn/noise"
)

// MessageConn is an ordered, reliable, message-oriented transport, such as a
// WebSocket connection. Implementations need not be safe for concurrent use;
// callers serialize access.
type MessageConn interface {
	Read(ctx context.Context) ([]byte, error)
	Write(ctx context.Context, data []byte) error
	Close() error
}

var cipherSuite = noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashSHA256)

// GenerateKey creates a new static X25519 keypair.
func GenerateKey() (noise.DHKey, error) {
	return cipherSuite.GenerateKeypair(rand.Reader)
}

// Conn is an encrypted MessageConn produced by a completed handshake.
// Encryption and decryption are internally serialized: Noise cipher states
// are stateful (nonce counters), and concurrent use would corrupt the stream.
type Conn struct {
	mc   MessageConn
	peer []byte

	encMu sync.Mutex
	enc   *noise.CipherState

	decMu sync.Mutex
	dec   *noise.CipherState
}

// PeerStatic returns the remote party's static public key.
func (c *Conn) PeerStatic() []byte { return c.peer }

// Close closes the underlying transport.
func (c *Conn) Close() error { return c.mc.Close() }

// Read receives and decrypts one message.
func (c *Conn) Read(ctx context.Context) ([]byte, error) {
	ct, err := c.mc.Read(ctx)
	if err != nil {
		return nil, err
	}
	c.decMu.Lock()
	pt, err := c.dec.Decrypt(nil, nil, ct)
	c.decMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("securechan: decrypt: %w", err)
	}
	return pt, nil
}

// Write encrypts and sends one message.
func (c *Conn) Write(ctx context.Context, data []byte) error {
	c.encMu.Lock()
	ct, err := c.enc.Encrypt(nil, nil, data)
	c.encMu.Unlock()
	if err != nil {
		return fmt.Errorf("securechan: encrypt: %w", err)
	}
	return c.mc.Write(ctx, ct)
}

func newHandshake(static noise.DHKey, initiator bool) (*noise.HandshakeState, error) {
	return noise.NewHandshakeState(noise.Config{
		CipherSuite:   cipherSuite,
		Pattern:       noise.HandshakeXX,
		Initiator:     initiator,
		StaticKeypair: static,
	})
}

// Initiate performs the initiator (phone) side of the handshake. If
// expectedPeer is non-nil, the responder's static key must match it.
func Initiate(ctx context.Context, mc MessageConn, static noise.DHKey, expectedPeer []byte) (*Conn, error) {
	hs, err := newHandshake(static, true)
	if err != nil {
		return nil, err
	}

	msg1, _, _, err := hs.WriteMessage(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("securechan: handshake msg1: %w", err)
	}
	if err := mc.Write(ctx, msg1); err != nil {
		return nil, err
	}

	msg2, err := mc.Read(ctx)
	if err != nil {
		return nil, err
	}
	if _, _, _, err := hs.ReadMessage(nil, msg2); err != nil {
		return nil, fmt.Errorf("securechan: handshake msg2: %w", err)
	}

	msg3, cs1, cs2, err := hs.WriteMessage(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("securechan: handshake msg3: %w", err)
	}
	if err := mc.Write(ctx, msg3); err != nil {
		return nil, err
	}

	peer := hs.PeerStatic()
	if expectedPeer != nil && !bytes.Equal(peer, expectedPeer) {
		mc.Close()
		return nil, fmt.Errorf("securechan: responder key mismatch")
	}
	// Initiator sends with cs1 and receives with cs2.
	return &Conn{mc: mc, enc: cs1, dec: cs2, peer: peer}, nil
}

// Respond performs the responder (daemon) side of the handshake. The caller
// authorizes the peer afterwards via PeerStatic.
func Respond(ctx context.Context, mc MessageConn, static noise.DHKey) (*Conn, error) {
	hs, err := newHandshake(static, false)
	if err != nil {
		return nil, err
	}

	msg1, err := mc.Read(ctx)
	if err != nil {
		return nil, err
	}
	if _, _, _, err := hs.ReadMessage(nil, msg1); err != nil {
		return nil, fmt.Errorf("securechan: handshake msg1: %w", err)
	}

	msg2, _, _, err := hs.WriteMessage(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("securechan: handshake msg2: %w", err)
	}
	if err := mc.Write(ctx, msg2); err != nil {
		return nil, err
	}

	msg3, err := mc.Read(ctx)
	if err != nil {
		return nil, err
	}
	_, cs1, cs2, err := hs.ReadMessage(nil, msg3)
	if err != nil {
		return nil, fmt.Errorf("securechan: handshake msg3: %w", err)
	}

	// Responder receives with cs1 and sends with cs2.
	return &Conn{mc: mc, enc: cs2, dec: cs1, peer: hs.PeerStatic()}, nil
}
