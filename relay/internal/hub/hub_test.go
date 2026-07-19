package hub

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func newHubServer(t *testing.T) *httptest.Server {
	t.Helper()
	return newHubServerWith(t, Config{})
}

func newHubServerWith(t *testing.T, cfg Config) *httptest.Server {
	t.Helper()
	if cfg.PerIPPerMinute == 0 {
		cfg.PerIPPerMinute = 10000 // don't rate-limit tests
	}
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), cfg)
	hts := httptest.NewServer(h.Handler())
	t.Cleanup(hts.Close)
	return hts
}

func dialWS(t *testing.T, hts *httptest.Server, path string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(hts.URL, "http") + path
	conn, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", path, err)
	}
	return conn
}

func TestHostAndJoinPipeBothWays(t *testing.T) {
	hts := newHubServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	host := dialWS(t, hts, "/v1/host?room=r1")
	client := dialWS(t, hts, "/v1/join?room=r1")

	if err := client.Write(ctx, websocket.MessageBinary, []byte("from-client")); err != nil {
		t.Fatal(err)
	}
	_, data, err := host.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "from-client" {
		t.Fatalf("host received %q", data)
	}

	if err := host.Write(ctx, websocket.MessageBinary, []byte("from-host")); err != nil {
		t.Fatal(err)
	}
	_, data, err = client.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "from-host" {
		t.Fatalf("client received %q", data)
	}

	_ = client.Close(websocket.StatusNormalClosure, "")
	_ = host.Close(websocket.StatusNormalClosure, "")
}

func TestJoinWithoutHostFails(t *testing.T) {
	hts := newHubServer(t)
	url := "ws" + strings.TrimPrefix(hts.URL, "http") + "/v1/join?room=empty"
	if _, _, err := websocket.Dial(context.Background(), url, nil); err == nil {
		t.Fatal("join without host succeeded")
	}
}

func TestMissingRoomParamRejected(t *testing.T) {
	hts := newHubServer(t)
	for _, path := range []string{"/v1/host", "/v1/join"} {
		url := "ws" + strings.TrimPrefix(hts.URL, "http") + path
		if _, _, err := websocket.Dial(context.Background(), url, nil); err == nil {
			t.Errorf("%s without room succeeded", path)
		}
	}
}

func TestTokenAuth(t *testing.T) {
	hts := newHubServerWith(t, Config{Token: "sekrit"})
	base := "ws" + strings.TrimPrefix(hts.URL, "http")

	// Without or with a wrong token: rejected.
	for _, url := range []string{
		base + "/v1/host?room=r9",
		base + "/v1/host?room=r9&token=wrong",
		base + "/v1/join?room=r9&token=wrong",
	} {
		if _, _, err := websocket.Dial(context.Background(), url, nil); err == nil {
			t.Errorf("dial without valid token succeeded: %s", url)
		}
	}

	// With the token: host connects and a client can join.
	host, _, err := websocket.Dial(context.Background(), base+"/v1/host?room=r9&token=sekrit", nil)
	if err != nil {
		t.Fatalf("host with token: %v", err)
	}
	client, _, err := websocket.Dial(context.Background(), base+"/v1/join?room=r9&token=sekrit", nil)
	if err != nil {
		t.Fatalf("join with token: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Write(ctx, websocket.MessageBinary, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	if _, data, err := host.Read(ctx); err != nil || string(data) != "hi" {
		t.Fatalf("piped data = %q, %v", data, err)
	}
}

func TestRateLimiterBlocksFloods(t *testing.T) {
	hts := newHubServerWith(t, Config{PerIPPerMinute: 3})
	base := "ws" + strings.TrimPrefix(hts.URL, "http")

	blocked := false
	for i := 0; i < 10; i++ {
		conn, _, err := websocket.Dial(context.Background(), base+"/v1/host?room=flood", nil)
		if err != nil {
			blocked = true
			break
		}
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}
	if !blocked {
		t.Error("10 rapid connections were never rate limited at 3/min")
	}
}

func TestReconnectingHostReplacesStaleOne(t *testing.T) {
	hts := newHubServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stale := dialWS(t, hts, "/v1/host?room=r2")
	fresh := dialWS(t, hts, "/v1/host?room=r2")

	// The stale host's connection is torn down by the replacement.
	readCtx, readCancel := context.WithTimeout(ctx, 5*time.Second)
	defer readCancel()
	if _, _, err := stale.Read(readCtx); err == nil {
		t.Fatal("stale host connection still alive after replacement")
	}

	// A client joining now reaches the fresh host.
	client := dialWS(t, hts, "/v1/join?room=r2")
	if err := client.Write(ctx, websocket.MessageBinary, []byte("ping")); err != nil {
		t.Fatal(err)
	}
	_, data, err := fresh.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "ping" {
		t.Fatalf("fresh host received %q", data)
	}
}

func TestClientLeaveTearsDownHostForRedial(t *testing.T) {
	hts := newHubServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	host := dialWS(t, hts, "/v1/host?room=r3")
	client := dialWS(t, hts, "/v1/join?room=r3")
	_ = client.Close(websocket.StatusNormalClosure, "")

	// Host read fails once the client leaves, prompting the daemon to redial.
	readCtx, readCancel := context.WithTimeout(ctx, 5*time.Second)
	defer readCancel()
	if _, _, err := host.Read(readCtx); err == nil {
		t.Fatal("host connection still alive after client left")
	}

	// The room is free again for a new host + client cycle.
	host2 := dialWS(t, hts, "/v1/host?room=r3")
	client2 := dialWS(t, hts, "/v1/join?room=r3")
	if err := client2.Write(ctx, websocket.MessageBinary, []byte("again")); err != nil {
		t.Fatal(err)
	}
	if _, data, err := host2.Read(ctx); err != nil || string(data) != "again" {
		t.Fatalf("second cycle failed: %q %v", data, err)
	}
}
