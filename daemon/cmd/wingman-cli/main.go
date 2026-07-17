// wingman-cli is a terminal test client for wingmand. It stands in for the
// phone app: it creates or watches sessions, streams the transcript, and
// answers permission requests interactively.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/coder/websocket"

	"github.com/heisenberg-alt/wingman/daemon/internal/proto"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "list":
		cmdList(os.Args[2:])
	case "run":
		cmdRun(os.Args[2:])
	case "watch":
		cmdWatch(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `wingman-cli — test client for wingmand

Usage:
  wingman-cli list  [--addr ws://127.0.0.1:7420/ws]
  wingman-cli run   [--addr ...] --cwd DIR [--prompt TEXT]
  wingman-cli watch [--addr ...] --session ID [--from-seq N]`)
}

type conn struct {
	ws     *websocket.Conn
	nextID atomic.Int64
	// replies delivers "res" envelopes by correlation id.
	replies chan proto.Envelope
	// events delivers seq-numbered session events.
	events chan proto.Envelope
}

func dial(ctx context.Context, addr string) (*conn, error) {
	ws, _, err := websocket.Dial(ctx, addr, nil)
	if err != nil {
		return nil, err
	}
	ws.SetReadLimit(16 << 20)
	c := &conn{ws: ws, replies: make(chan proto.Envelope, 16), events: make(chan proto.Envelope, 256)}
	go func() {
		defer close(c.events)
		for {
			_, data, err := ws.Read(ctx)
			if err != nil {
				return
			}
			var env proto.Envelope
			if json.Unmarshal(data, &env) != nil {
				continue
			}
			if env.Type == proto.TypeRes {
				c.replies <- env
			} else {
				c.events <- env
			}
		}
	}()
	return c, nil
}

func (c *conn) call(ctx context.Context, msgType, sessionID string, payload any) (proto.Result, error) {
	id := strconv.FormatInt(c.nextID.Add(1), 10)
	env := proto.Envelope{V: proto.Version, ID: id, SessionID: sessionID, Type: msgType}
	if payload != nil {
		env.Payload = proto.Marshal(payload)
	}
	data, _ := json.Marshal(env)
	if err := c.ws.Write(ctx, websocket.MessageText, data); err != nil {
		return proto.Result{}, err
	}
	for {
		select {
		case <-ctx.Done():
			return proto.Result{}, ctx.Err()
		case reply, ok := <-c.replies:
			if !ok {
				return proto.Result{}, fmt.Errorf("connection closed")
			}
			if reply.ID != id {
				continue
			}
			var res proto.Result
			if err := json.Unmarshal(reply.Payload, &res); err != nil {
				return proto.Result{}, err
			}
			if !res.OK {
				return res, fmt.Errorf("%s: %s", msgType, res.Error)
			}
			return res, nil
		}
	}
}

func cmdList(args []string) {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	addr := fs.String("addr", "ws://127.0.0.1:7420/ws", "wingmand WebSocket address")
	_ = fs.Parse(args)

	ctx := context.Background()
	c, err := dial(ctx, *addr)
	fatalIf(err)
	res, err := c.call(ctx, proto.CmdSessionList, "", nil)
	fatalIf(err)

	var list proto.SessionList
	fatalIf(json.Unmarshal(res.Data, &list))
	if len(list.Sessions) == 0 {
		fmt.Println("no sessions")
		return
	}
	for _, s := range list.Sessions {
		fmt.Printf("%s  %-20s  %s  %s\n", s.ID, s.Status, s.CreatedAt.Local().Format("15:04:05"), s.Cwd)
	}
}

func cmdRun(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	addr := fs.String("addr", "ws://127.0.0.1:7420/ws", "wingmand WebSocket address")
	cwd := fs.String("cwd", "", "working directory for the session (required)")
	prompt := fs.String("prompt", "", "initial prompt")
	_ = fs.Parse(args)
	if *cwd == "" {
		fatalIf(fmt.Errorf("--cwd is required"))
	}

	ctx := context.Background()
	c, err := dial(ctx, *addr)
	fatalIf(err)

	res, err := c.call(ctx, proto.CmdSessionCreate, "", proto.SessionCreate{Cwd: *cwd, Prompt: *prompt})
	fatalIf(err)
	var info proto.SessionInfo
	fatalIf(json.Unmarshal(res.Data, &info))
	fmt.Printf("● session %s created in %s\n", info.ID, info.Cwd)

	_, err = c.call(ctx, proto.CmdSessionWatch, info.ID, proto.SessionWatch{FromSeq: 0})
	fatalIf(err)
	stream(ctx, c, info.ID)
}

func cmdWatch(args []string) {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	addr := fs.String("addr", "ws://127.0.0.1:7420/ws", "wingmand WebSocket address")
	sessionID := fs.String("session", "", "session id (required)")
	fromSeq := fs.Uint64("from-seq", 0, "replay events after this sequence number")
	_ = fs.Parse(args)
	if *sessionID == "" {
		fatalIf(fmt.Errorf("--session is required"))
	}

	ctx := context.Background()
	c, err := dial(ctx, *addr)
	fatalIf(err)
	_, err = c.call(ctx, proto.CmdSessionWatch, *sessionID, proto.SessionWatch{FromSeq: *fromSeq})
	fatalIf(err)
	stream(ctx, c, *sessionID)
}

// stream renders session events and answers permission requests interactively.
func stream(ctx context.Context, c *conn, sessionID string) {
	stdin := bufio.NewReader(os.Stdin)
	for env := range c.events {
		switch env.Type {
		case proto.EvtSessionState:
			var p proto.SessionState
			_ = json.Unmarshal(env.Payload, &p)
			fmt.Printf("\n── state: %s ──\n", p.Status)
			if p.Status == "done" || p.Status == "error" {
				return
			}

		case proto.EvtTranscriptDelta:
			renderDelta(env.Payload)

		case proto.EvtPermissionRequest:
			var p proto.PermissionRequest
			_ = json.Unmarshal(env.Payload, &p)
			fmt.Printf("\n🔐 permission requested: %s\n", p.Title)
			for i, o := range p.Options {
				fmt.Printf("  [%d] %s (%s)\n", i+1, o.Name, o.Kind)
			}
			fmt.Print("choose option number: ")
			line, _ := stdin.ReadString('\n')
			idx, err := strconv.Atoi(strings.TrimSpace(line))
			if err != nil || idx < 1 || idx > len(p.Options) {
				idx = len(p.Options) // fall back to the last option (typically reject)
			}
			_, err = c.call(ctx, proto.CmdSessionApprove, sessionID, proto.SessionApprove{
				RequestID: p.RequestID,
				OptionID:  p.Options[idx-1].OptionID,
			})
			if err != nil {
				fmt.Fprintln(os.Stderr, "approve failed:", err)
			}

		case proto.EvtPermissionResolved:
			var p proto.PermissionResolved
			_ = json.Unmarshal(env.Payload, &p)
			fmt.Printf("   ↳ resolved by %s\n", p.ResolvedBy)

		case proto.EvtTurnEnded:
			var p proto.TurnEnded
			_ = json.Unmarshal(env.Payload, &p)
			fmt.Printf("\n■ turn ended: %s\n", p.StopReason)
		}
	}
}

func renderDelta(payload json.RawMessage) {
	var d proto.TranscriptDelta
	if json.Unmarshal(payload, &d) != nil {
		return
	}
	switch d.Kind {
	case "agent_message_chunk", "agent_thought_chunk":
		var chunk struct {
			Content struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(d.Data, &chunk) == nil && chunk.Content.Type == "text" {
			if d.Kind == "agent_thought_chunk" {
				fmt.Print("\033[2m", chunk.Content.Text, "\033[0m")
			} else {
				fmt.Print(chunk.Content.Text)
			}
		}
	case "tool_call":
		var tc struct {
			Title string `json:"title"`
		}
		_ = json.Unmarshal(d.Data, &tc)
		fmt.Printf("\n🔧 %s\n", tc.Title)
	case "tool_call_update", "plan", "available_commands_update":
		// quiet in the CLI renderer
	default:
		fmt.Printf("\n[%s]\n", d.Kind)
	}
}

func fatalIf(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
