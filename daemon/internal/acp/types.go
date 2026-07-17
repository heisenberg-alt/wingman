// Package acp implements a minimal Agent Client Protocol (ACP) client that
// speaks JSON-RPC 2.0 as newline-delimited JSON over the stdio of a spawned
// `copilot --acp --stdio` subprocess.
package acp

import "encoding/json"

// InitializeParams is sent as the first request on a new connection.
type InitializeParams struct {
	ProtocolVersion    int            `json:"protocolVersion"`
	ClientCapabilities map[string]any `json:"clientCapabilities"`
}

// AgentInfo identifies the agent implementation.
type AgentInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title"`
	Version string `json:"version"`
}

// InitializeResult is the agent's handshake response.
type InitializeResult struct {
	ProtocolVersion   int             `json:"protocolVersion"`
	AgentCapabilities json.RawMessage `json:"agentCapabilities,omitempty"`
	AgentInfo         *AgentInfo      `json:"agentInfo,omitempty"`
}

// NewSessionParams creates a conversation session rooted at Cwd.
type NewSessionParams struct {
	Cwd        string `json:"cwd"`
	McpServers []any  `json:"mcpServers"`
}

// NewSessionResult carries the agent-assigned session id.
type NewSessionResult struct {
	SessionID string `json:"sessionId"`
}

// ContentBlock is a single piece of prompt or response content.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// PromptParams sends a user turn to the agent.
type PromptParams struct {
	SessionID string         `json:"sessionId"`
	Prompt    []ContentBlock `json:"prompt"`
}

// PromptResult ends a turn with a stop reason (e.g. "end_turn").
type PromptResult struct {
	StopReason string `json:"stopReason"`
}

// CancelParams is the session/cancel notification payload.
type CancelParams struct {
	SessionID string `json:"sessionId"`
}

// SessionNotification is the params of a session/update notification.
type SessionNotification struct {
	SessionID string          `json:"sessionId"`
	Update    json.RawMessage `json:"update"`
}

// UpdateKind extracts the discriminator of a session update object.
type UpdateKind struct {
	SessionUpdate string `json:"sessionUpdate"`
}

// PermissionOption is one choice offered in a permission request.
type PermissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

// RequestPermissionParams is the agent→client session/request_permission call.
type RequestPermissionParams struct {
	SessionID string             `json:"sessionId"`
	ToolCall  json.RawMessage    `json:"toolCall"`
	Options   []PermissionOption `json:"options"`
}

// PermissionOutcome is the client's decision.
type PermissionOutcome struct {
	Outcome  string `json:"outcome"` // "selected" | "cancelled"
	OptionID string `json:"optionId,omitempty"`
}

// RequestPermissionResult wraps the outcome for the JSON-RPC response.
type RequestPermissionResult struct {
	Outcome PermissionOutcome `json:"outcome"`
}
