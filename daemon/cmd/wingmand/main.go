// wingmand is the Wingman daemon: it drives GitHub Copilot CLI sessions over
// ACP and exposes them to paired phones through the Wingman wire protocol.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/heisenberg-alt/wingman/daemon/internal/acp"
	"github.com/heisenberg-alt/wingman/daemon/internal/session"
	"github.com/heisenberg-alt/wingman/daemon/internal/transport"
)

const version = "0.1.0-dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		cmdServe(os.Args[2:])
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
  wingmand serve   [--listen 127.0.0.1:7420] [--copilot copilot] [--perm-timeout 5m]
  wingmand doctor  [--copilot copilot]
  wingmand version`)
}

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:7420", "address for the loopback WebSocket listener")
	copilotPath := fs.String("copilot", "copilot", "path to the copilot binary")
	permTimeout := fs.Duration("perm-timeout", 5*time.Minute, "fail-safe deny timeout for permission requests")
	_ = fs.Parse(args)

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	mgr := session.NewManager(session.Config{
		CopilotPath:       *copilotPath,
		PermissionTimeout: *permTimeout,
		Logger:            logger,
	})
	defer mgr.CloseAll()

	srv := &transport.Server{Manager: mgr}
	httpSrv := &http.Server{Addr: *listen, Handler: srv.Handler()}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	logger.Info("wingmand listening", "addr", *listen, "version", version)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("serve failed", "err", err)
		os.Exit(1)
	}
	logger.Info("wingmand stopped")
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
		fmt.Fprintln(os.Stderr, "✗ failed to spawn copilot --acp:", err)
		os.Exit(1)
	}
	defer client.Close()

	res, err := client.Initialize(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "✗ ACP initialize failed:", err)
		os.Exit(1)
	}

	fmt.Println("✓ ACP handshake OK")
	if res.AgentInfo != nil {
		fmt.Printf("  agent:            %s %s\n", res.AgentInfo.Name, res.AgentInfo.Version)
	}
	fmt.Printf("  protocol version: %d\n", res.ProtocolVersion)
	if len(res.AgentCapabilities) > 0 {
		fmt.Printf("  capabilities:     %s\n", res.AgentCapabilities)
	}
}
