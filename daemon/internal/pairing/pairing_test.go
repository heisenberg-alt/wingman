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

func TestRegistryRemoveAndList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	r, err := LoadRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	keyA, _ := securechan.GenerateKey()
	keyB, _ := securechan.GenerateKey()
	if err := r.Add("phone-a", keyA.Public); err != nil {
		t.Fatal(err)
	}
	if err := r.Add("phone-b", keyB.Public); err != nil {
		t.Fatal(err)
	}
	if got := len(r.List()); got != 2 {
		t.Fatalf("List = %d devices, want 2", got)
	}

	removed, err := r.Remove(keyA.Public)
	if err != nil || !removed {
		t.Fatalf("Remove = %v, %v; want true, nil", removed, err)
	}
	if r.IsAuthorized(keyA.Public) {
		t.Error("removed device still authorized")
	}
	if !r.IsAuthorized(keyB.Public) {
		t.Error("unrelated device lost authorization")
	}

	// Revocation persists across reloads.
	r2, err := LoadRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if r2.IsAuthorized(keyA.Public) {
		t.Error("removed device authorized after reload")
	}

	removed, err = r2.RemoveByName("phone-b")
	if err != nil || !removed {
		t.Fatalf("RemoveByName = %v, %v; want true, nil", removed, err)
	}
	if removed, _ = r2.RemoveByName("phone-b"); removed {
		t.Error("second RemoveByName reported a removal")
	}
	if r2.Count() != 0 {
		t.Errorf("Count = %d, want 0", r2.Count())
	}
}
