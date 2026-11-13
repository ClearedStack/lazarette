// Command lazarette serves a private OCI image store from a directory.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/clearedstack/lazarette/internal/registry"
	"github.com/clearedstack/lazarette/internal/store"
)

func main() {
	addr := flag.String("addr", ":5000", "address to listen on")
	data := flag.String("data", "", "directory for blobs and manifests (required; the only place written to)")
	idle := flag.Duration("idle-timeout", registry.DefaultIdleTimeout, "how long a request body may stall before E_TIMEOUT")
	flag.Parse()

	logger := log.New(os.Stderr, "lazarette ", log.LstdFlags|log.LUTC)
	if *data == "" || flag.NArg() != 0 {
		logger.Print("refusing to start: -data is required and no other arguments are accepted")
		os.Exit(2)
	}
	st, err := store.Open(*data)
	if err != nil {
		logger.Printf("refusing to start: storage directory is not usable: %v", err)
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           &registry.Handler{Store: st, IdleTimeout: *idle, Log: logger},
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          logger,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	failed := make(chan error, 1)
	go func() { failed <- srv.ListenAndServe() }()
	logger.Print("serving the OCI registry API")

	select {
	case err := <-failed:
		logger.Printf("stopped: %v", err)
		os.Exit(1)
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Printf("shutdown was not clean: %v", err)
		os.Exit(1)
	}
}
