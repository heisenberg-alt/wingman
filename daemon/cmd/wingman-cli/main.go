// wingman-cli is a terminal test client for wingmand. It stands in for the
// phone app: it pairs with a daemon, creates or watches sessions over the
// secure channel, streams transcripts, and answers permission requests.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/coder/websocket"
	"github.com/flynn/noise"

	"github.com/heisenberg-alt/wingman/daemon/internal/pairing"
	"github.com/heisenberg-alt/wingman/daemon/internal/proto"
	"github.com/heisenberg-alt/wingman/daemon/internal/securechan"
	"github.com/heisenberg-alt/wingman/daemon/internal/wsconn"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "pair":
		cmdPair(os.Args[2:])
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
  wingman-cli pair  --payload JSON [--name NAME] [--via lan|relay]
  wingman-cli list  [--addr ws://127.0.0.1:7420/ws | --secure [--via lan|relay]]
  wingman-cli run   [connection flags] --cwd DIR [--prompt TEXT]
  wingman-cli watch [connection flags] --session ID [--from-seq N]

With --secure, the client connects through the daemon's Noise-secured external
listener or the relay, using the identity saved by "pair". Without it, the
plaintext loopback listener at --addr is used.`)
}

// config is the client's persisted identity and daemon contact info.
type config struct {
	Private   []byte `json:"private"`
	Public    []byte `json:"public"`
	DaemonPub []byte `json:"daemonPub"`
	Lan       string `json:"lan,omitempty"`
	Relay     string `json:"relay,omitempty"`
	Room      string `json:"room"`
}

func configPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".wingman-cli.json"
	}
	return filepath.Join(home, ".wingman-cli.json")
}

func loadConfig() (*config, error) {
	data, err := os.ReadFile(configPath())
	if err != nil {
		return nil, fmt.Errorf("no pairing config, run \"wingman-cli pair\" first: %w", err)
	}
	var cfg config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *config) save() error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath(), data, 0o600)
}

func (c *config) key() noise.DHKey {
	return noise.DHKey{Private: c.Private, Public: c.Public}
}

// connFlags holds the connection options shared by all commands.
type connFlags struct {
	addr   *string
	secure *bool
	via    *string
}

func newFlagSet(name string) (*flag.FlagSet, *connFlags) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	cf := &connFlags{
		addr:   fs.String("addr", "ws://127.0.0.1:7420/ws", "loopback WebSocket address"),
		secure: fs.Bool("secure", false, "connect through the Noise-secured channel"),
		via:    fs.String("via", "lan", "secure path: lan or relay"),
	}
	return fs, cf
}

// connect dials according to the connection flags.
func (cf *connFlags) connect(ctx context.Context) (*conn, error) {
	if !*cf.secure {
		mc, err := dialWS(ctx, *cf.addr)
		if err != nil {
			return nil, err
		}
		return newConn(ctx, mc), nil
	}

	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	sc, err := dialSecure(ctx, cfg, *cf.via)
	if err != nil {
		return nil, err
	}
	return newConn(ctx, sc), nil
}

func dialWS(ctx context.Context, addr string) (securechan.MessageConn, error) {
	ws, _, err := websocket.Dial(ctx, addr, nil)
	if err != nil {
		return nil, err
	}
	return wsconn.New(ws), nil
}

// dialSecure connects via LAN or relay and completes the Noise handshake,
// pinning the daemon's public key.
func dialSecure(ctx context.Context, cfg *config, via string) (securechan.MessageConn, error) {
	var raw securechan.MessageConn
	var err error
	switch via {
	case "lan":
		if cfg.Lan == "" {
			return nil, fmt.Errorf("no LAN address in pairing config")
		}
		raw, err = dialWS(ctx, "ws://"+cfg.Lan+"/ws")
	case "relay":
		if cfg.Relay == "" {
			return nil, fmt.Errorf("no relay URL in pairing config")
		}
		raw, err = dialWS(ctx, cfg.Relay+"/v1/join?room="+url.QueryEscape(cfg.Room))
	default:
		return nil, fmt.Errorf("unknown --via %q (want lan or relay)", via)
	}
	if err != nil {
		return nil, err
	}
	return securechan.Initiate(ctx, raw, cfg.key(), cfg.DaemonPub)
}

// conn multiplexes replies and events over one protocol connection.
type conn struct {
	mc     securechan.MessageConn
	nextID atomic.Int64
	// replies delivers "res" envelopes; events delivers session events.
	replies chan proto.Envelope
	events  chan proto.Envelope
}

func newConn(ctx context.Context, mc securechan.MessageConn) *conn {
	c := &conn{mc: mc, replies: make(chan proto.Envelope, 16), events: make(chan proto.Envelope, 256)}
	go func() {
		defer close(c.events)
		for {
			data, err := mc.Read(ctx)
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
	return c
}

func (c *conn) call(ctx context.Context, msgType, sessionID string, payload any) (proto.Result, error) {
	id := strconv.FormatInt(c.nextID.Add(1), 10)
	env := proto.Envelope{V: proto.Version, ID: id, SessionID: sessionID, Type: msgType}
	if payload != nil {
		env.Payload = proto.Marshal(payload)
	}
	data, _ := json.Marshal(env)
	if err := c.mc.Write(ctx, data); err != nil {
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

func cmdPair(args []string) {
	fs := flag.NewFlagSet("pair", flag.ExitOnError)
	payloadJSON := fs.String("payload", "", "pairing payload JSON from \"wingmand pair --json\" (required)")
	name := fs.String("name", "wingman-cli", "device name to register")
	via := fs.String("via", "lan", "pairing path: lan or relay")
	_ = fs.Parse(args)
	if *payloadJSON == "" {
		fatalIf(fmt.Errorf("--payload is required"))
	}

	var payload pairing.Payload
	fatalIf(json.Unmarshal([]byte(*payloadJSON), &payload))

	key, err := securechan.GenerateKey()
	fatalIf(err)
	cfg := &config{
		Private:   key.Private,
		Public:    key.Public,
		DaemonPub: payload.Pub,
		Lan:       payload.Lan,
		Relay:     payload.Relay,
		Room:      payload.Room,
	}

	ctx := context.Background()
	sc, err := dialSecure(ctx, cfg, *via)
	fatalIf(err)
	c := newConn(ctx, sc)

	_, err = c.call(ctx, proto.CmdPairRequest, "", proto.PairRequest{Token: payload.Token, DeviceName: *name})
	fatalIf(err)
	fatalIf(cfg.save())
	fmt.Printf("paired as %q; config saved to %s\n", *name, configPath())
}

func cmdList(args []string) {
	fs, cf := newFlagSet("list")
	_ = fs.Parse(args)

	ctx := context.Background()
	c, err := cf.connect(ctx)
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
	fs, cf := newFlagSet("run")
	cwd := fs.String("cwd", "", "working directory for the session (required)")
	prompt := fs.String("prompt", "", "initial prompt")
	_ = fs.Parse(args)
	if *cwd == "" {
		fatalIf(fmt.Errorf("--cwd is required"))
	}

	ctx := context.Background()
	c, err := cf.connect(ctx)
	fatalIf(err)

	res, err := c.call(ctx, proto.CmdSessionCreate, "", proto.SessionCreate{Cwd: *cwd, Prompt: *prompt})
	fatalIf(err)
	var info proto.SessionInfo
	fatalIf(json.Unmarshal(res.Data, &info))
	fmt.Printf("session %s created in %s\n", info.ID, info.Cwd)

	_, err = c.call(ctx, proto.CmdSessionWatch, info.ID, proto.SessionWatch{FromSeq: 0})
	fatalIf(err)
	stream(ctx, c, info.ID, true)
}

func cmdWatch(args []string) {
	fs, cf := newFlagSet("watch")
	sessionID := fs.String("session", "", "session id (required)")
	fromSeq := fs.Uint64("from-seq", 0, "replay events after this sequence number")
	_ = fs.Parse(args)
	if *sessionID == "" {
		fatalIf(fmt.Errorf("--session is required"))
	}

	ctx := context.Background()
	c, err := cf.connect(ctx)
	fatalIf(err)
	_, err = c.call(ctx, proto.CmdSessionWatch, *sessionID, proto.SessionWatch{FromSeq: *fromSeq})
	fatalIf(err)
	stream(ctx, c, *sessionID, false)
}

// stream renders session events and answers permission requests interactively.
// When exitOnTurnEnd is set it returns after the first completed turn.
func stream(ctx context.Context, c *conn, sessionID string, exitOnTurnEnd bool) {
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
			fmt.Printf("\npermission requested: %s\n", p.Title)
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
			fmt.Printf("   resolved by %s\n", p.ResolvedBy)

		case proto.EvtTurnEnded:
			var p proto.TurnEnded
			_ = json.Unmarshal(env.Payload, &p)
			fmt.Printf("\nturn ended: %s\n", p.StopReason)
			if exitOnTurnEnd {
				return
			}
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
		fmt.Printf("\n[tool] %s\n", tc.Title)
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
