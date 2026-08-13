package push

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// writeTestKey generates an ES256 key, writes it as a .p8-style PEM, and
// returns the path plus the public key for signature verification.
func writeTestKey(t *testing.T) (string, *ecdsa.PublicKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "AuthKey_TEST.p8")
	pemData := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, pemData, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, &key.PublicKey
}

type capturedPush struct {
	path    string
	headers http.Header
	body    []byte
}

// newAPNSServer runs an HTTP/2 test server that captures pushes and lets
// the test choose the response per device token.
func newAPNSServer(t *testing.T, respond func(token string, w http.ResponseWriter)) (*httptest.Server, *[]capturedPush, *sync.Mutex) {
	t.Helper()
	var mu sync.Mutex
	var pushes []capturedPush
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		pushes = append(pushes, capturedPush{path: r.URL.Path, headers: r.Header.Clone(), body: body})
		mu.Unlock()
		respond(strings.TrimPrefix(r.URL.Path, "/3/device/"), w)
	}))
	ts.EnableHTTP2 = true
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts, &pushes, &mu
}

func newTestAPNS(t *testing.T, ts *httptest.Server) (*APNS, *ecdsa.PublicKey) {
	t.Helper()
	keyPath, pub := writeTestKey(t)
	a, err := NewAPNS(APNSConfig{
		KeyPath:   keyPath,
		KeyID:     "KEY123",
		TeamID:    "TEAM456",
		BundleID:  "dev.wingman.Wingman",
		Endpoints: map[string]string{EnvSandbox: ts.URL, EnvProduction: ts.URL},
		Client:    ts.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return a, pub
}

func TestPushSendsSignedRequest(t *testing.T) {
	ts, pushes, mu := newAPNSServer(t, func(_ string, w http.ResponseWriter) {
		w.WriteHeader(http.StatusOK)
	})
	a, pub := newTestAPNS(t, ts)

	payload := []byte(`{"aps":{"alert":"hi"}}`)
	if err := a.Push(context.Background(), "device-token-1", EnvSandbox, payload); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(*pushes) != 1 {
		t.Fatalf("captured %d pushes, want 1", len(*pushes))
	}
	p := (*pushes)[0]
	if p.path != "/3/device/device-token-1" {
		t.Errorf("path = %q", p.path)
	}
	if got := p.headers.Get("apns-topic"); got != "dev.wingman.Wingman" {
		t.Errorf("apns-topic = %q", got)
	}
	if got := p.headers.Get("apns-push-type"); got != "alert" {
		t.Errorf("apns-push-type = %q", got)
	}
	if string(p.body) != string(payload) {
		t.Errorf("body = %s", p.body)
	}

	// The bearer token must be a valid ES256 JWT for our key.
	bearer := strings.TrimPrefix(p.headers.Get("authorization"), "bearer ")
	parts := strings.Split(bearer, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt has %d segments", len(parts))
	}
	headerJSON, _ := base64.RawURLEncoding.DecodeString(parts[0])
	var hdr struct{ Alg, Kid string }
	_ = json.Unmarshal(headerJSON, &hdr)
	if hdr.Alg != "ES256" || hdr.Kid != "KEY123" {
		t.Errorf("jwt header = %+v", hdr)
	}
	claimsJSON, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
	}
	_ = json.Unmarshal(claimsJSON, &claims)
	if claims.Iss != "TEAM456" || time.Since(time.Unix(claims.Iat, 0)) > time.Minute {
		t.Errorf("jwt claims = %+v", claims)
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	if len(sig) != 64 {
		t.Fatalf("signature length = %d, want 64", len(sig))
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(pub, digest[:], r, s) {
		t.Error("jwt signature does not verify")
	}
}

func TestPushReportsGoneTokens(t *testing.T) {
	ts, _, _ := newAPNSServer(t, func(token string, w http.ResponseWriter) {
		if token == "gone" {
			w.WriteHeader(http.StatusGone)
			_, _ = w.Write([]byte(`{"reason":"Unregistered"}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"reason":"BadMessageId"}`))
	})
	a, _ := newTestAPNS(t, ts)

	if err := a.Push(context.Background(), "gone", EnvSandbox, []byte(`{}`)); !errors.Is(err, ErrTokenGone) {
		t.Errorf("gone token error = %v, want ErrTokenGone", err)
	}
	err := a.Push(context.Background(), "other", EnvSandbox, []byte(`{}`))
	if err == nil || errors.Is(err, ErrTokenGone) {
		t.Errorf("other failure = %v, want plain error", err)
	}
}

func TestRegistryUpsertRemovePersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "push.json")
	r, err := LoadTokens(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Register("tok-1", EnvSandbox, "phone"); err != nil {
		t.Fatal(err)
	}
	// Upsert: same token re-registered with a new env, not duplicated.
	if err := r.Register("tok-1", EnvProduction, "phone"); err != nil {
		t.Fatal(err)
	}
	if err := r.Register("", EnvSandbox, "x"); err == nil {
		t.Error("empty token accepted")
	}
	if err := r.Register("tok-2", "bogus", "x"); err == nil {
		t.Error("bogus env accepted")
	}
	devices := r.List()
	if len(devices) != 1 || devices[0].Env != EnvProduction {
		t.Fatalf("devices = %+v", devices)
	}

	r2, err := LoadTokens(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(r2.List()) != 1 {
		t.Fatalf("reloaded devices = %+v", r2.List())
	}
	r2.Remove("tok-1")
	if len(r2.List()) != 0 {
		t.Error("token survived Remove")
	}
	r3, _ := LoadTokens(path)
	if len(r3.List()) != 0 {
		t.Error("removal did not persist")
	}
}

func TestNotifierPushesAndDropsGoneTokens(t *testing.T) {
	ts, pushes, mu := newAPNSServer(t, func(token string, w http.ResponseWriter) {
		if token == "gone" {
			w.WriteHeader(http.StatusGone)
			_, _ = w.Write([]byte(`{"reason":"Unregistered"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	a, _ := newTestAPNS(t, ts)

	reg, err := LoadTokens(filepath.Join(t.TempDir(), "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	_ = reg.Register("live", EnvSandbox, "good-phone")
	_ = reg.Register("gone", EnvSandbox, "old-phone")

	n := &Notifier{APNS: a, Tokens: reg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	n.PermissionRequested("sess-1", "req-1", "Run ls", []string{"allow_once", "reject_once"})

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		sent := len(*pushes)
		mu.Unlock()
		if sent == 2 && len(reg.List()) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pushes = %d, remaining tokens = %d; want 2 and 1", sent, len(reg.List()))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if reg.List()[0].Token != "live" {
		t.Errorf("surviving token = %q, want live", reg.List()[0].Token)
	}

	// Payload shape: title routed, ids and options present, no tool contents.
	mu.Lock()
	defer mu.Unlock()
	var body map[string]any
	_ = json.Unmarshal((*pushes)[0].body, &body)
	if body["sessionId"] != "sess-1" || body["requestId"] != "req-1" {
		t.Errorf("payload routing ids = %v", body)
	}
	aps, _ := body["aps"].(map[string]any)
	alert, _ := aps["alert"].(map[string]any)
	if alert["body"] != "Run ls" {
		t.Errorf("alert body = %v", alert["body"])
	}
}
