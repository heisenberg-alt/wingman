// Package wsconn adapts a WebSocket connection to the securechan.MessageConn
// interface, shared by the daemon transports and the test client.
package wsconn

import (
	"context"

	"github.com/coder/websocket"
)

// Conn wraps a *websocket.Conn as a message-oriented transport.
type Conn struct {
	ws *websocket.Conn
}

// New creates the adapter and raises the read limit for large transcripts.
func New(ws *websocket.Conn) *Conn {
	ws.SetReadLimit(16 << 20)
	return &Conn{ws: ws}
}

// Read returns the next message payload, regardless of frame type.
func (c *Conn) Read(ctx context.Context) ([]byte, error) {
	_, data, err := c.ws.Read(ctx)
	return data, err
}

// Write sends data as a binary message.
func (c *Conn) Write(ctx context.Context, data []byte) error {
	return c.ws.Write(ctx, websocket.MessageBinary, data)
}

// Close closes the WebSocket with a normal status.
func (c *Conn) Close() error {
	return c.ws.Close(websocket.StatusNormalClosure, "")
}
