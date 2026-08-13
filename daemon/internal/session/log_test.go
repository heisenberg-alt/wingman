package session

import (
	"os"
	"testing"
	"time"
)

func TestAppendAssignsSequentialSeqs(t *testing.T) {
	l := NewLog()
	for i := 1; i <= 3; i++ {
		evt := l.Append("test.event", map[string]int{"i": i})
		if evt.Seq != uint64(i) {
			t.Fatalf("seq = %d, want %d", evt.Seq, i)
		}
	}
}

func TestSinceReplaysAfterSeq(t *testing.T) {
	l := NewLog()
	for i := 0; i < 5; i++ {
		l.Append("test.event", nil)
	}
	if got := len(l.Since(0)); got != 5 {
		t.Fatalf("Since(0) = %d events, want 5", got)
	}
	tail := l.Since(3)
	if len(tail) != 2 || tail[0].Seq != 4 || tail[1].Seq != 5 {
		t.Fatalf("Since(3) = %+v, want seqs 4,5", tail)
	}
	if got := l.Since(5); got != nil {
		t.Fatalf("Since(5) = %+v, want nil", got)
	}
	if got := l.Since(99); got != nil {
		t.Fatalf("Since(99) = %+v, want nil", got)
	}
}

func TestSubscribeDeliversLiveInOrder(t *testing.T) {
	l := NewLog()
	ch, cancel := l.Subscribe()
	defer cancel()

	for i := 0; i < 10; i++ {
		l.Append("test.event", nil)
	}
	for i := 1; i <= 10; i++ {
		select {
		case evt := <-ch:
			if evt.Seq != uint64(i) {
				t.Fatalf("live event seq = %d, want %d", evt.Seq, i)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for live event %d", i)
		}
	}
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	l := NewLog()
	ch, cancel := l.Subscribe()
	cancel()
	l.Append("test.event", nil)
	select {
	case evt := <-ch:
		t.Fatalf("received %+v after unsubscribe", evt)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSlowSubscriberDropsButLogRetains(t *testing.T) {
	l := NewLog()
	_, cancel := l.Subscribe() // never drained; must not block appends
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 500; i++ {
			l.Append("test.event", nil)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("appends blocked by slow subscriber")
	}
	if got := len(l.Since(0)); got != 500 {
		t.Fatalf("log retained %d events, want 500", got)
	}
}

func TestOpenLogPersistsAndReplays(t *testing.T) {
	path := t.TempDir() + "/log.jsonl"

	l1, err := OpenLog(path)
	if err != nil {
		t.Fatal(err)
	}
	l1.Append("a", map[string]int{"n": 1})
	l1.Append("b", map[string]int{"n": 2})
	if err := l1.Close(); err != nil {
		t.Fatal(err)
	}

	l2, err := OpenLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	events := l2.Since(0)
	if len(events) != 2 || events[0].Type != "a" || events[1].Type != "b" {
		t.Fatalf("replayed events = %+v", events)
	}
	// Appends continue the seq after a reload.
	if evt := l2.Append("c", nil); evt.Seq != 3 {
		t.Fatalf("seq after reload = %d, want 3", evt.Seq)
	}
}

func TestOpenLogDiscardsTornFinalLine(t *testing.T) {
	path := t.TempDir() + "/log.jsonl"

	l1, err := OpenLog(path)
	if err != nil {
		t.Fatal(err)
	}
	l1.Append("a", nil)
	_ = l1.Close()

	// Simulate a crash mid-write: a torn, newline-less final line.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"seq":2,"type":"tor`)
	_ = f.Close()

	l2, err := OpenLog(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(l2.Since(0)); got != 1 {
		t.Fatalf("events after torn line = %d, want 1", got)
	}
	// The torn bytes were truncated away, so new appends survive a reload.
	l2.Append("b", nil)
	_ = l2.Close()

	l3, err := OpenLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l3.Close()
	events := l3.Since(0)
	if len(events) != 2 || events[1].Type != "b" {
		t.Fatalf("events after torn-line recovery = %+v", events)
	}
}
