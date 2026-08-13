// Package push sends APNs alerts for permission requests. The daemon talks
// to Apple directly with the app owner's .p8 auth key (ADR-0006); the relay
// never sees tokens or push traffic.
package push

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

// APNs environments and their endpoints.
const (
	EnvProduction = "production"
	EnvSandbox    = "sandbox"
)

var defaultEndpoints = map[string]string{
	EnvProduction: "https://api.push.apple.com",
	EnvSandbox:    "https://api.sandbox.push.apple.com",
}

// ErrTokenGone reports that APNs rejected the device token as no longer
// valid; the caller should drop it from the registry.
var ErrTokenGone = errors.New("push: device token is gone")

// APNSConfig configures the APNs client.
type APNSConfig struct {
	// KeyPath is the .p8 provider auth key from the Apple developer portal.
	KeyPath string
	// KeyID identifies the auth key; TeamID the developer team.
	KeyID  string
	TeamID string
	// BundleID is the app's bundle identifier (the apns-topic).
	BundleID string

	// Endpoints overrides the per-env APNs base URLs (tests only).
	Endpoints map[string]string
	// Client overrides the HTTP client (tests only). APNs requires HTTP/2,
	// which the default client negotiates automatically over TLS.
	Client *http.Client
}

// APNS is a minimal APNs HTTP/2 provider-token client.
type APNS struct {
	cfg    APNSConfig
	key    *ecdsa.PrivateKey
	client *http.Client

	mu        sync.Mutex
	jwt       string
	jwtIssued time.Time
}

// NewAPNS loads the .p8 key and prepares the client.
func NewAPNS(cfg APNSConfig) (*APNS, error) {
	if cfg.KeyID == "" || cfg.TeamID == "" || cfg.BundleID == "" {
		return nil, errors.New("push: apns key id, team id, and bundle id are all required")
	}
	data, err := os.ReadFile(cfg.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("push: read apns key: %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("push: apns key is not PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("push: parse apns key: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("push: apns key is not an ECDSA (ES256) key")
	}
	if cfg.Endpoints == nil {
		cfg.Endpoints = defaultEndpoints
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &APNS{cfg: cfg, key: key, client: client}, nil
}

// Push delivers one alert payload to a device token in the given env.
func (a *APNS) Push(ctx context.Context, token, env string, payload []byte) error {
	base, ok := a.cfg.Endpoints[env]
	if !ok {
		return fmt.Errorf("push: unknown APNs environment %q", env)
	}
	jwt, err := a.providerJWT()
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/3/device/"+token, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("authorization", "bearer "+jwt)
	req.Header.Set("apns-topic", a.cfg.BundleID)
	req.Header.Set("apns-push-type", "alert")
	req.Header.Set("apns-priority", "10")
	req.Header.Set("content-type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var apnsErr struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(body, &apnsErr)
	if resp.StatusCode == http.StatusGone ||
		apnsErr.Reason == "BadDeviceToken" || apnsErr.Reason == "Unregistered" {
		return ErrTokenGone
	}
	return fmt.Errorf("push: APNs %s: %s", resp.Status, apnsErr.Reason)
}

// providerJWT returns a cached ES256 provider token, minting a fresh one
// when the cached token nears Apple's 60-minute validity limit.
func (a *APNS) providerJWT() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.jwt != "" && time.Since(a.jwtIssued) < 50*time.Minute {
		return a.jwt, nil
	}

	now := time.Now()
	header, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": a.cfg.KeyID})
	claims, _ := json.Marshal(map[string]any{"iss": a.cfg.TeamID, "iat": now.Unix()})
	signing := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)

	digest := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, a.key, digest[:])
	if err != nil {
		return "", fmt.Errorf("push: sign provider token: %w", err)
	}
	// JOSE ES256 signatures are r ∥ s, each left-padded to 32 bytes.
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])

	a.jwt = signing + "." + base64.RawURLEncoding.EncodeToString(sig)
	a.jwtIssued = now
	return a.jwt, nil
}
