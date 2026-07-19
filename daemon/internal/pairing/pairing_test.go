package pairing

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/heisenberg-alt/wingman/daemon/internal/securechan"
)

func TestLoadOrCreateKeyRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys", "static.json")

	created, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(created.Private) == 0 || len(created.Public) == 0 {
		t.Fatal("generated key is empty")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file mode = %o, want 600", perm)
	}

	loaded, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(created.Private, loaded.Private) || !bytes.Equal(created.Public, loaded.Public) {
		t.Error("reloaded key differs from created key")
	}
}

func TestRegistryAddAuthorizePersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	key, _ := securechan.GenerateKey()

	r, err := LoadRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if r.IsAuthorized(key.Public) {
		t.Fatal("empty registry authorized a key")
	}

	if err := r.Add("phone-1", key.Public); err != nil {
		t.Fatal(err)
	}
	if !r.IsAuthorized(key.Public) {
		t.Fatal("added key not authorized")
	}

	// Duplicate add is a no-op.
	if err := r.Add("phone-1-again", key.Public); err != nil {
		t.Fatal(err)
	}
	if r.Count() != 1 {
		t.Errorf("count = %d after duplicate add, want 1", r.Count())
	}

	// Persistence across reload.
	r2, err := LoadRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if !r2.IsAuthorized(key.Public) {
		t.Error("reloaded registry lost the device")
	}

	other, _ := securechan.GenerateKey()
	if r2.IsAuthorized(other.Public) {
		t.Error("unknown key authorized")
	}
}

func TestTokensSingleUseAndExpiry(t *testing.T) {
	tok := &Tokens{}

	if tok.Redeem("anything") {
		t.Fatal("redeem succeeded before issue")
	}

	issued := tok.Issue(time.Minute)
	if tok.Redeem("wrong-token") {
		t.Fatal("wrong token redeemed")
	}
	if !tok.Redeem(issued) {
		t.Fatal("valid token rejected")
	}
	if tok.Redeem(issued) {
		t.Fatal("token redeemed twice")
	}

	expired := tok.Issue(10 * time.Millisecond)
	time.Sleep(30 * time.Millisecond)
	if tok.Redeem(expired) {
		t.Fatal("expired token redeemed")
	}

	// Issuing a new token invalidates the previous one.
	first := tok.Issue(time.Minute)
	_ = tok.Issue(time.Minute)
	if tok.Redeem(first) {
		t.Fatal("superseded token redeemed")
	}
}

func TestRoomIsStablePerKey(t *testing.T) {
	a, _ := securechan.GenerateKey()
	b, _ := securechan.GenerateKey()

	if Room(a.Public) != Room(a.Public) {
		t.Error("room not stable for the same key")
	}
	if Room(a.Public) == Room(b.Public) {
		t.Error("different keys mapped to the same room")
	}
	if len(Room(a.Public)) == 0 {
		t.Error("empty room id")
	}
}
