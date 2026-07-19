// Package proto defines the Wingman wire protocol (v1) spoken between the
// phone app and wingmand. See docs/PROTOCOL.md for the specification.
package proto

import (
	"encoding/json"
	"time"
)

// Version is the current protocol version.
const Version = 1

// Envelope wraps every protocol message.
type Envelope struct {
	V         int             `json:"v"`
	ID        string          `json:"id,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Seq       uint64          `json:"seq,omitempty"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// Command types (phone → daemon).
const (
	CmdSessionList    = "session.list"
	CmdSessionCreate  = "session.create"
	CmdSessionPrompt  = "session.prompt"
	CmdSessionApprove = "session.approve"
	CmdSessionCancel  = "session.cancel"
	CmdSessionWatch   = "session.watch"
	CmdSessionUnwatch = "session.unwatch"
	CmdPairRequest    = "pair.request"
)

// Event types (daemon → phone).
const (
	TypeRes               = "res"
	EvtSessionState       = "session.state"
	EvtTranscriptDelta    = "transcript.delta"
	EvtPermissionRequest  = "permission.request"
	EvtPermissionResolved = "permission.resolved"
	EvtTurnEnded          = "turn.ended"
)

// SessionCreate starts a new Copilot session.
type SessionCreate struct {
	Cwd    string `json:"cwd"`
	Prompt string `json:"prompt,omitempty"`
}

// SessionPrompt sends a follow-up prompt to a running session.
type SessionPrompt struct {
	Text string `json:"text"`
}

// SessionApprove answers a pending permission request.
type SessionApprove struct {
	RequestID string `json:"requestId"`
	OptionID  string `json:"optionId"`
}

// SessionWatch subscribes to a session's event stream from a sequence number.
type SessionWatch struct {
	FromSeq uint64 `json:"fromSeq"`
}

// PairRequest is the first message an unpaired device sends over a freshly
// established secure channel.
type PairRequest struct {
	Token      string `json:"token"`
	DeviceName string `json:"deviceName"`
}

// Result is the payload of every "res" reply.
type Result struct {
	OK    bool            `json:"ok"`
	Error string          `json:"error,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// SessionInfo describes a session in list/create replies.
type SessionInfo struct {
	ID        string    `json:"id"`
	Cwd       string    `json:"cwd"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
}

// SessionList is the data of a session.list reply.
type SessionList struct {
	Sessions []SessionInfo `json:"sessions"`
}

// SessionState reports a session status change.
type SessionState struct {
	Status string `json:"status"`
}

// TranscriptDelta carries one ACP session update, losslessly.
type TranscriptDelta struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

// PermissionOption mirrors an ACP permission option.
type PermissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

// PermissionRequest asks the phone to authorize a tool call.
type PermissionRequest struct {
	RequestID string             `json:"requestId"`
	Title     string             `json:"title,omitempty"`
	ToolCall  json.RawMessage    `json:"toolCall,omitempty"`
	Options   []PermissionOption `json:"options"`
}

// PermissionResolved records the outcome of a permission request.
type PermissionResolved struct {
	RequestID  string `json:"requestId"`
	OptionID   string `json:"optionId,omitempty"`
	ResolvedBy string `json:"resolvedBy"` // "phone" | "timeout" | "cancel"
}

// TurnEnded reports the end of a prompt turn.
type TurnEnded struct {
	StopReason string `json:"stopReason"`
}

// Marshal is a convenience for building payloads.
func Marshal(v any) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}
