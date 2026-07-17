package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
)

// ProtocolVersion is the ACP protocol version this client speaks.
const ProtocolVersion = 1

type rpcMessage struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *RPCError        `json:"error,omitempty"`
}

// RPCError is a JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("acp: rpc error %d: %s", e.Code, e.Message)
}

// NotificationHandler receives agent→client notifications (e.g. session/update).
type NotificationHandler func(method string, params json.RawMessage)

// RequestHandler serves agent→client requests (e.g. session/request_permission).
// It may block; each request is served on its own goroutine.
type RequestHandler func(ctx context.Context, method string, params json.RawMessage) (any, error)

// Options configures a spawned ACP subprocess.
type Options struct {
	// Command is the copilot binary; defaults to "copilot".
	Command string
	// Dir is the working directory for the subprocess.
	Dir string
	// OnNotification and OnRequest wire agent→client traffic.
	OnNotification NotificationHandler
	OnRequest      RequestHandler
}

// Client is a JSON-RPC 2.0 NDJSON client bound to one ACP subprocess.
type Client struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	nextID atomic.Int64

	writeMu sync.Mutex

	mu      sync.Mutex
	pending map[int64]chan *rpcMessage
	closed  bool

	onNotification NotificationHandler
	onRequest      RequestHandler

	done chan struct{}
	err  error
}

// Spawn starts `copilot --acp --stdio` and begins the read loop.
func Spawn(ctx context.Context, opts Options) (*Client, error) {
	bin := opts.Command
	if bin == "" {
		bin = "copilot"
	}
	cmd := exec.CommandContext(ctx, bin, "--acp", "--stdio")
	cmd.Dir = opts.Dir
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("acp: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("acp: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("acp: start %s: %w", bin, err)
	}

	c := &Client{
		cmd:            cmd,
		stdin:          stdin,
		pending:        make(map[int64]chan *rpcMessage),
		onNotification: opts.OnNotification,
		onRequest:      opts.OnRequest,
		done:           make(chan struct{}),
	}
	go c.readLoop(stdout)
	return c, nil
}

// Done is closed when the subprocess's stdout closes.
func (c *Client) Done() <-chan struct{} { return c.done }

// Close terminates the subprocess. Closing stdin asks the ACP server to shut
// down; the process is killed if it lingers.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()

	_ = c.stdin.Close()
	_ = c.cmd.Process.Kill()
	_, _ = c.cmd.Process.Wait()
	return nil
}

// Call performs a JSON-RPC request and unmarshals the result into out (if non-nil).
func (c *Client) Call(ctx context.Context, method string, params any, out any) error {
	id := c.nextID.Add(1)
	ch := make(chan *rpcMessage, 1)

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errors.New("acp: client closed")
	}
	c.pending[id] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	rawID := json.RawMessage(fmt.Sprintf("%d", id))
	if err := c.write(rpcMessage{JSONRPC: "2.0", ID: &rawID, Method: method, Params: marshal(params)}); err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return fmt.Errorf("acp: connection closed during %s: %w", method, c.err)
	case msg := <-ch:
		if msg.Error != nil {
			return msg.Error
		}
		if out != nil && msg.Result != nil {
			return json.Unmarshal(msg.Result, out)
		}
		return nil
	}
}

// Notify sends a JSON-RPC notification (no response expected).
func (c *Client) Notify(method string, params any) error {
	return c.write(rpcMessage{JSONRPC: "2.0", Method: method, Params: marshal(params)})
}

// Initialize performs the ACP handshake.
func (c *Client) Initialize(ctx context.Context) (*InitializeResult, error) {
	var res InitializeResult
	err := c.Call(ctx, "initialize", InitializeParams{
		ProtocolVersion:    ProtocolVersion,
		ClientCapabilities: map[string]any{},
	}, &res)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// NewSession creates an ACP session rooted at cwd.
func (c *Client) NewSession(ctx context.Context, cwd string) (string, error) {
	var res NewSessionResult
	err := c.Call(ctx, "session/new", NewSessionParams{Cwd: cwd, McpServers: []any{}}, &res)
	if err != nil {
		return "", err
	}
	return res.SessionID, nil
}

// Prompt sends a user turn and blocks until the turn ends.
func (c *Client) Prompt(ctx context.Context, sessionID, text string) (*PromptResult, error) {
	var res PromptResult
	err := c.Call(ctx, "session/prompt", PromptParams{
		SessionID: sessionID,
		Prompt:    []ContentBlock{{Type: "text", Text: text}},
	}, &res)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// Cancel interrupts the current turn.
func (c *Client) Cancel(sessionID string) error {
	return c.Notify("session/cancel", CancelParams{SessionID: sessionID})
}

func (c *Client) write(msg rpcMessage) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("acp: marshal: %w", err)
	}
	data = append(data, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.stdin.Write(data)
	return err
}

func (c *Client) readLoop(r io.Reader) {
	defer close(c.done)
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			c.dispatch(line)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				c.err = err
			} else {
				c.err = io.EOF
			}
			return
		}
	}
}

func (c *Client) dispatch(line []byte) {
	var msg rpcMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		return // ignore malformed lines
	}

	switch {
	case msg.Method == "" && msg.ID != nil:
		// Response to one of our requests.
		var id int64
		if err := json.Unmarshal(*msg.ID, &id); err != nil {
			return
		}
		c.mu.Lock()
		ch := c.pending[id]
		c.mu.Unlock()
		if ch != nil {
			ch <- &msg
		}

	case msg.Method != "" && msg.ID != nil:
		// Agent→client request; serve on its own goroutine as handlers may block.
		go c.serveRequest(&msg)

	case msg.Method != "":
		// Notification.
		if c.onNotification != nil {
			c.onNotification(msg.Method, msg.Params)
		}
	}
}

func (c *Client) serveRequest(msg *rpcMessage) {
	var result any
	var rpcErr *RPCError

	if c.onRequest == nil {
		rpcErr = &RPCError{Code: -32601, Message: "method not found: " + msg.Method}
	} else {
		res, err := c.onRequest(context.Background(), msg.Method, msg.Params)
		if err != nil {
			var re *RPCError
			if errors.As(err, &re) {
				rpcErr = re
			} else {
				rpcErr = &RPCError{Code: -32603, Message: err.Error()}
			}
		} else {
			result = res
		}
	}

	reply := rpcMessage{JSONRPC: "2.0", ID: msg.ID}
	if rpcErr != nil {
		reply.Error = rpcErr
	} else {
		reply.Result = marshal(result)
	}
	_ = c.write(reply)
}

func marshal(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return data
}
