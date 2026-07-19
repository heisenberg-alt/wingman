// fakecopilot is a minimal stand-in for `copilot --acp --stdio` used in
// tests. It speaks JSON-RPC 2.0 as NDJSON on stdio and implements just enough
// of the ACP surface: initialize, session/new, and session/prompt with
// streaming updates. Prompts containing "NEEDPERM" trigger a blocking
// session/request_permission round trip.
package main

import (
	"bufio"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

type inMessage struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
}

type outMessage struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  any              `json:"params,omitempty"`
	Result  any              `json:"result,omitempty"`
}

var (
	writeMu sync.Mutex
	nextID  atomic.Int64

	pendingMu sync.Mutex
	pending   = map[string]chan inMessage{}
)

func send(m outMessage) {
	data, _ := json.Marshal(m)
	writeMu.Lock()
	defer writeMu.Unlock()
	_, _ = os.Stdout.Write(append(data, '\n'))
}

func respond(id *json.RawMessage, result any) {
	send(outMessage{JSONRPC: "2.0", ID: id, Result: result})
}

func notify(method string, params any) {
	send(outMessage{JSONRPC: "2.0", Method: method, Params: params})
}

// request performs a server->client JSON-RPC request and blocks for the reply.
func request(method string, params any) inMessage {
	raw := json.RawMessage(strconv.FormatInt(nextID.Add(1), 10))
	ch := make(chan inMessage, 1)
	pendingMu.Lock()
	pending[string(raw)] = ch
	pendingMu.Unlock()
	send(outMessage{JSONRPC: "2.0", ID: &raw, Method: method, Params: params})
	return <-ch
}

func update(sessionID string, upd any) {
	notify("session/update", map[string]any{"sessionId": sessionID, "update": upd})
}

func textChunk(text string) map[string]any {
	return map[string]any{
		"sessionUpdate": "agent_message_chunk",
		"content":       map[string]any{"type": "text", "text": text},
	}
}

func handle(m inMessage) {
	switch m.Method {
	case "initialize":
		respond(m.ID, map[string]any{
			"protocolVersion": 1,
			"agentInfo":       map[string]any{"name": "FakeCopilot", "version": "0.0.1"},
		})

	case "session/new":
		respond(m.ID, map[string]any{"sessionId": "fake-session-1"})

	case "session/prompt":
		var p struct {
			SessionID string `json:"sessionId"`
			Prompt    []struct {
				Text string `json:"text"`
			} `json:"prompt"`
		}
		_ = json.Unmarshal(m.Params, &p)
		text := ""
		if len(p.Prompt) > 0 {
			text = p.Prompt[0].Text
		}

		update(p.SessionID, textChunk("working on: "+text))

		if strings.Contains(text, "NEEDPERM") {
			res := request("session/request_permission", map[string]any{
				"sessionId": p.SessionID,
				"toolCall":  map[string]any{"title": "Create file"},
				"options": []map[string]any{
					{"optionId": "allow_once", "name": "Allow once", "kind": "allow_once"},
					{"optionId": "reject_once", "name": "Deny", "kind": "reject_once"},
				},
			})
			var outcome struct {
				Outcome struct {
					Outcome  string `json:"outcome"`
					OptionID string `json:"optionId"`
				} `json:"outcome"`
			}
			_ = json.Unmarshal(res.Result, &outcome)
			update(p.SessionID, textChunk("permission:"+outcome.Outcome.Outcome+":"+outcome.Outcome.OptionID))
		}

		update(p.SessionID, textChunk("done"))
		respond(m.ID, map[string]any{"stopReason": "end_turn"})

	case "session/cancel":
		// Notification; nothing to do.

	default:
		if m.ID != nil {
			send(outMessage{JSONRPC: "2.0", ID: m.ID, Result: map[string]any{}})
		}
	}
}

func main() {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := make([]byte, len(sc.Bytes()))
		copy(line, sc.Bytes())
		var m inMessage
		if json.Unmarshal(line, &m) != nil {
			continue
		}
		if m.Method == "" && m.ID != nil {
			pendingMu.Lock()
			ch := pending[string(*m.ID)]
			delete(pending, string(*m.ID))
			pendingMu.Unlock()
			if ch != nil {
				ch <- m
			}
			continue
		}
		go handle(m)
	}
}
