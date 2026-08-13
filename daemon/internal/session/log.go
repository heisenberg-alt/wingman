package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
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

// Log is an append-only event log with live subscriptions. It is in-memory
// by default; OpenLog additionally persists every event as one JSON line so
// the log survives daemon restarts (see ADR-0005).
type Log struct {
	mu      sync.Mutex
	events  []Event
	nextSub int
	subs    map[int]chan Event
	file    *os.File // nil for in-memory logs
}

// NewLog creates an empty in-memory log.
func NewLog() *Log {
	return &Log{subs: make(map[int]chan Event)}
}

// OpenLog opens a persistent log at path, replaying any existing events into
// memory and appending future events to the file. A torn final line (crash
// mid-write) is discarded and truncated away so later appends stay readable.
func OpenLog(path string) (*Log, error) {
	l := NewLog()

	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("read event log: %w", err)
	}
	off := 0
	for off < len(data) {
		nl := bytes.IndexByte(data[off:], '\n')
		if nl < 0 {
			break // torn final line without newline
		}
		var evt Event
		if json.Unmarshal(data[off:off+nl], &evt) != nil ||
			evt.Seq != uint64(len(l.events))+1 {
			break // malformed or misordered: keep only the valid prefix
		}
		l.events = append(l.events, evt)
		off += nl + 1
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open event log: %w", err)
	}
	if err := f.Truncate(int64(off)); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("truncate event log: %w", err)
	}
	if _, err := f.Seek(int64(off), 0); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("seek event log: %w", err)
	}
	l.file = f
	return l, nil
}

// Close releases the underlying file of a persistent log.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

// Append adds an event, assigns its seq, and fans it out to subscribers.
// Fanout happens under the lock with non-blocking sends, guaranteeing that
// live delivery order matches seq order; slow subscribers drop events and
// recover via replay. Persistent logs write the event through before fanout.
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
	if l.file != nil {
		line, _ := json.Marshal(evt)
		buf := append(line, '\n')
		if n, err := l.file.Write(buf); err != nil || n != len(buf) {
			_ = l.file.Close()
			l.file = nil
		}
	}
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
