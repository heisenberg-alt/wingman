package session

import (
	"encoding/json"
	"sync"
	"time"
)

// Event is one seq-numbered, replayable session event.
type Event struct {
	Seq     uint64          `json:"seq"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
	Time    time.Time       `json:"time"`
}

// Log is an in-memory, append-only event log with live subscriptions.
// A persistent (SQLite) implementation replaces this in Phase 2.
type Log struct {
	mu      sync.Mutex
	events  []Event
	nextSub int
	subs    map[int]chan Event
}

// NewLog creates an empty log.
func NewLog() *Log {
	return &Log{subs: make(map[int]chan Event)}
}

// Append adds an event, assigns its seq, and fans it out to subscribers.
// Fanout happens under the lock with non-blocking sends, guaranteeing that
// live delivery order matches seq order; slow subscribers drop events and
// recover via replay.
func (l *Log) Append(evtType string, payload any) Event {
	data, _ := json.Marshal(payload)

	l.mu.Lock()
	defer l.mu.Unlock()
	evt := Event{
		Seq:     uint64(len(l.events)) + 1,
		Type:    evtType,
		Payload: data,
		Time:    time.Now().UTC(),
	}
	l.events = append(l.events, evt)
	for _, ch := range l.subs {
		select {
		case ch <- evt:
		default: // slow subscriber: drop; they recover via replay
		}
	}
	return evt
}

// Since returns all events with seq > fromSeq.
func (l *Log) Since(fromSeq uint64) []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	if fromSeq >= uint64(len(l.events)) {
		return nil
	}
	out := make([]Event, len(l.events)-int(fromSeq))
	copy(out, l.events[fromSeq:])
	return out
}

// Subscribe returns a channel of live events and a cancel func.
func (l *Log) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 256)
	l.mu.Lock()
	id := l.nextSub
	l.nextSub++
	l.subs[id] = ch
	l.mu.Unlock()

	return ch, func() {
		l.mu.Lock()
		delete(l.subs, id)
		l.mu.Unlock()
	}
}
