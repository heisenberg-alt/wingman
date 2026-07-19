// wingmand is the Wingman daemon: it drives GitHub Copilot CLI sessions over
// ACP and exposes them to paired phones through the Wingman wire protocol,
// end-to-end encrypted over LAN or a relay.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mdp/qrterminal/v3"

	"github.com/heisenberg-alt/wingman/daemon/internal/acp"
	"github.com/heisenberg-alt/wingman/daemon/internal/pairing"
	"github.com/heisenberg-alt/wingman/daemon/internal/session"
	"github.com/heisenberg-alt/wingman/daemon/internal/transport"
)

const version = "0.2.0-dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		cmdServe(os.Args[2:])
	case "pair":
		cmdPair(os.Args[2:])
	case "doctor":
		cmdDoctor(os.Args[2:])
	case "version":
		fmt.Println("wingmand", version)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `wingmand — Wingman daemon for GitHub Copilot CLI

Usage:
  wingmand serve   [--listen 127.0.0.1:7420] [--external :7421] [--relay URL]
                   [--home ~/.wingman] [--copilot copilot] [--perm-timeout 5m]
  wingmand pair    [--addr http://127.0.0.1:7420] [--json]
  wingmand doctor  [--copilot copilot]
  wingmand version`)
}

func defaultHome() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".wingman"
	}
	return filepath.Join(home, ".wingman")
}

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:7420", "loopback listener (protocol + admin)")
	external := fs.String("external", "", "external Noise-secured listener, e.g. :7421 (disabled if empty)")
	relayURL := fs.String("relay", "", "relay base URL, e.g. wss://relay.example.com (disabled if empty)")
	relayToken := fs.String("relay-token", os.Getenv("WINGMAN_RELAY_TOKEN"), "bearer token for the relay (if it requires auth)")
	home := fs.String("home", defaultHome(), "state directory for keys and paired devices")
	copilotPath := fs.String("copilot", "copilot", "path to the copilot binary")
	permTimeout := fs.Duration("perm-timeout", 5*time.Minute, "fail-safe deny timeout for permission requests")
	_ = fs.Parse(args)

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	static, err := pairing.LoadOrCreateKey(filepath.Join(*home, "keys", "static.json"))
	if err != nil {
		logger.Error("load static key", "err", err)
		os.Exit(1)
	}
	registry, err := pairing.LoadRegistry(filepath.Join(*home, "devices.json"))
	if err != nil {
		logger.Error("load device registry", "err", err)
		os.Exit(1)
	}
	tokens := &pairing.Tokens{}

	mgr := session.NewManager(session.Config{
		CopilotPath:       *copilotPath,
		PermissionTimeout: *permTimeout,
		Logger:            logger,
	})
	defer mgr.CloseAll()

	srv := &transport.Server{Manager: mgr}
	secure := &transport.SecureServer{
		Server:   srv,
		Static:   static,
		Registry: registry,
		Tokens:   tokens,
		Logger:   logger,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Loopback listener: protocol + admin endpoints.
	room := pairing.Room(static.Public)
	adminMux := http.NewServeMux()
	adminMux.Handle("/", srv.Handler())
	adminMux.HandleFunc("POST /pair", func(w http.ResponseWriter, r *http.Request) {
		payload := pairing.Payload{
			V:          1,
			Pub:        static.Public,
			Relay:      *relayURL,
			Room:       room,
			Token:      tokens.Issue(10 * time.Minute),
			RelayToken: *relayToken,
		}
		if *external != "" {
			payload.Lan = lanAddr(*external)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	})
	loopback := &http.Server{Addr: *listen, Handler: adminMux}
	go shutdownOnDone(ctx, loopback)

	// External Noise-secured listener.
	if *external != "" {
		ext := &http.Server{Addr: *external, Handler: secure.Handler()}
		go shutdownOnDone(ctx, ext)
		go func() {
			logger.Info("external listener up", "addr", *external)
			if err := ext.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Error("external listener failed", "err", err)
			}
		}()
	}

	// Relay host connection.
	if *relayURL != "" {
		go transport.RunRelayHost(ctx, *relayURL, room, *relayToken, secure)
	}

	logger.Info("wingmand listening", "addr", *listen, "version", version,
		"paired_devices", registry.Count(), "room", room)
	if err := loopback.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("serve failed", "err", err)
		os.Exit(1)
	}
	logger.Info("wingmand stopped")
}

func shutdownOnDone(ctx context.Context, srv *http.Server) {
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

// lanAddr combines the machine's LAN IP with the external listener's port,
// skipping loopback, down, and virtual (VPN/tunnel) interfaces.
func lanAddr(external string) string {
	_, port, err := net.SplitHostPort(external)
	if err != nil {
		return ""
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		name := strings.ToLower(iface.Name)
		if strings.HasPrefix(name, "utun") || strings.HasPrefix(name, "tun") ||
			strings.HasPrefix(name, "tap") || strings.HasPrefix(name, "awdl") ||
			strings.HasPrefix(name, "llw") || strings.HasPrefix(name, "bridge") {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok || ipnet.IP.IsLoopback() {
				continue
			}
			if ip4 := ipnet.IP.To4(); ip4 != nil {
				return net.JoinHostPort(ip4.String(), port)
			}
		}
	}
	return ""
}

// cmdPair asks the running daemon for a pairing payload and renders it as a
// QR code for the phone to scan.
func cmdPair(args []string) {
	fs := flag.NewFlagSet("pair", flag.ExitOnError)
	addr := fs.String("addr", "http://127.0.0.1:7420", "daemon admin address")
	jsonOnly := fs.Bool("json", false, "print the raw payload JSON only")
	_ = fs.Parse(args)

	resp, err := http.Post(strings.TrimRight(*addr, "/")+"/pair", "application/json", nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: is wingmand running?", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "error: pair request failed (%s): %s\n", resp.Status, body)
		os.Exit(1)
	}
	payload := strings.TrimSpace(string(body))

	if *jsonOnly {
		fmt.Println(payload)
		return
	}
	fmt.Println("Scan this QR code with the Wingman app (token valid for 10 minutes):")
	fmt.Println()
	qrterminal.GenerateHalfBlock(payload, qrterminal.L, os.Stdout)
	fmt.Println()
	fmt.Println(payload)
}

// cmdDoctor probes the installed copilot binary's ACP server.
func cmdDoctor(args []string) {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	copilotPath := fs.String("copilot", "copilot", "path to the copilot binary")
	_ = fs.Parse(args)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	client, err := acp.Spawn(ctx, acp.Options{Command: *copilotPath})
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAIL: spawn copilot --acp:", err)
		os.Exit(1)
	}
	defer client.Close()

	res, err := client.Initialize(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAIL: ACP initialize:", err)
		os.Exit(1)
	}

	fmt.Println("OK: ACP handshake")
	if res.AgentInfo != nil {
		fmt.Printf("  agent:            %s %s\n", res.AgentInfo.Name, res.AgentInfo.Version)
	}
	fmt.Printf("  protocol version: %d\n", res.ProtocolVersion)
	if len(res.AgentCapabilities) > 0 {
		fmt.Printf("  capabilities:     %s\n", res.AgentCapabilities)
	}
}
