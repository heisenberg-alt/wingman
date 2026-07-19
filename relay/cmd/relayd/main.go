// relayd is the Wingman relay: a zero-knowledge rendezvous service that pipes
// end-to-end encrypted frames between daemons and phones.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/heisenberg-alt/wingman/relay/internal/hub"
)

const version = "0.1.0-dev"

func main() {
	listen := flag.String("listen", ":8443", "listen address")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	h := hub.New(logger)
	srv := &http.Server{Addr: *listen, Handler: h.Handler()}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	logger.Info("relayd listening", "addr", *listen, "version", version)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("serve failed", "err", err)
		os.Exit(1)
	}
}
