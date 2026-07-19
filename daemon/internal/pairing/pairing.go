// Package pairing manages the daemon's static identity key, the registry of
// paired devices, and one-time pairing tokens exchanged via QR code.
package pairing

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"crypto/rand"

	"github.com/flynn/noise"

	"github.com/heisenberg-alt/wingman/daemon/internal/securechan"
)

// LoadOrCreateKey loads the static keypair from path, generating and
// persisting a new one (mode 0600) if the file does not exist.
func LoadOrCreateKey(path string) (noise.DHKey, error) {
	var stored struct {
		Private []byte `json:"private"`
		Public  []byte `json:"public"`
	}

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &stored); err != nil {
			return noise.DHKey{}, fmt.Errorf("pairing: parse key file %s: %w", path, err)
		}
		return noise.DHKey{Private: stored.Private, Public: stored.Public}, nil

	case os.IsNotExist(err):
		key, err := securechan.GenerateKey()
		if err != nil {
			return noise.DHKey{}, err
		}
		stored.Private, stored.Public = key.Private, key.Public
		data, _ := json.Marshal(stored)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return noise.DHKey{}, err
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return noise.DHKey{}, err
		}
		return key, nil

	default:
		return noise.DHKey{}, err
	}
}

// Device is one paired phone.
type Device struct {
	Name      string    `json:"name"`
	PublicKey []byte    `json:"publicKey"`
	AddedAt   time.Time `json:"addedAt"`
}

// Registry is the persistent set of paired devices.
type Registry struct {
	path string

	mu      sync.Mutex
	devices []Device
}

// LoadRegistry reads the registry at path, returning an empty registry if the
// file does not exist yet.
func LoadRegistry(path string) (*Registry, error) {
	r := &Registry{path: path}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &r.devices); err != nil {
		return nil, fmt.Errorf("pairing: parse registry %s: %w", path, err)
	}
	return r, nil
}

// IsAuthorized reports whether pub belongs to a paired device.
func (r *Registry) IsAuthorized(pub []byte) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, d := range r.devices {
		if subtle.ConstantTimeCompare(d.PublicKey, pub) == 1 {
			return true
		}
	}
	return false
}

// Add registers a device and persists the registry.
func (r *Registry) Add(name string, pub []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, d := range r.devices {
		if subtle.ConstantTimeCompare(d.PublicKey, pub) == 1 {
			return nil // already paired
		}
	}
	r.devices = append(r.devices, Device{Name: name, PublicKey: pub, AddedAt: time.Now().UTC()})
	data, err := json.MarshalIndent(r.devices, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(r.path, data, 0o600)
}

// Count returns the number of paired devices.
func (r *Registry) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.devices)
}

// Tokens issues and redeems single-use pairing tokens.
type Tokens struct {
	mu     sync.Mutex
	token  string
	expiry time.Time
}

// Issue creates a new token valid for ttl, replacing any previous token.
func (t *Tokens) Issue(ttl time.Duration) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.token = hex.EncodeToString(b)
	t.expiry = time.Now().Add(ttl)
	return t.token
}

// Redeem consumes the token if it matches and has not expired.
func (t *Tokens) Redeem(token string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token == "" || token == "" || time.Now().After(t.expiry) {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(t.token), []byte(token)) != 1 {
		return false
	}
	t.token = "" // single use
	return true
}

// Payload is the pairing information encoded into the QR code.
type Payload struct {
	V     int    `json:"v"`
	Pub   []byte `json:"pub"`             // daemon static public key
	Lan   string `json:"lan,omitempty"`   // host:port of the external listener
	Relay string `json:"relay,omitempty"` // relay base URL, e.g. wss://relay.example.com
	Room  string `json:"room"`            // relay rendezvous id
	Token string `json:"token"`           // single-use pairing token
	// RelayToken authenticates connections to the relay itself (bearer).
	RelayToken string `json:"relayToken,omitempty"`
}

// Room derives the stable relay rendezvous id from the daemon public key.
func Room(pub []byte) string {
	sum := sha256.Sum256(pub)
	return base64.RawURLEncoding.EncodeToString(sum[:12])
}
