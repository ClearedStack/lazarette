// Command lazarette serves a cluster-private OCI image store from a directory
// and runs scheduled garbage collection to retain the last 5 signed digests per
// app.
package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/ClearedStack/lazarette/internal/registry"
	"github.com/ClearedStack/lazarette/internal/store"
)

const version = "v0.1.0-draft"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stderr, nil))
}

func run(ctx context.Context, args []string, stderr io.Writer, ready chan<- string) int {
	logger := log.New(stderr, "lazarette ", log.LstdFlags|log.LUTC)

	fs := flag.NewFlagSet("lazarette", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", ":5000", "address to listen on")
	data := fs.String("data", "", "directory for blobs and manifests (required; the only place written to)")
	idle := fs.Duration("idle-timeout", registry.DefaultIdleTimeout, "how long a request body may stall before E_TIMEOUT")
	gcInterval := fs.Duration("gc-interval", 24*time.Hour, "how often garbage collection runs (0 disables scheduled runs)")
	gcGrace := fs.Duration("gc-grace", time.Hour, "how long unsigned pushes and uploads are spared by garbage collection")
	showVersion := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		logger.Printf("version %s", version)
		return 0
	}
	if *data == "" || fs.NArg() != 0 || *idle <= 0 || *gcInterval < 0 || *gcGrace < 0 {
		logger.Print("refusing to start: -data is required, durations must be valid, and no extra arguments are accepted")
		return 2
	}

	st, err := store.Open(*data)
	if err != nil {
		logger.Printf("refusing to start: storage directory is not usable: %v", err)
		return 1
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		logger.Printf("refusing to start: listen failed: %v", err)
		return 1
	}

	srv := &http.Server{
		Handler:           &registry.Handler{Store: st, IdleTimeout: *idle, Log: logger},
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          logger,
	}

	var wg sync.WaitGroup
	gcCtx, stopGC := context.WithCancel(ctx)
	defer stopGC()
	if *gcInterval > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(*gcInterval)
			defer ticker.Stop()
			for {
				select {
				case <-gcCtx.Done():
					return
				case now := <-ticker.C:
					stats, err := st.CollectGarbage(now.UTC(), *gcGrace)
					if err != nil {
						logger.Printf("garbage collection failed: %v", err)
						continue
					}
					logger.Printf("garbage collection: manifests=%d blobs=%d uploads=%d bytes=%d",
						stats.ManifestsRemoved, stats.BlobsRemoved, stats.UploadsRemoved, stats.BytesFreed)
				}
			}
		}()
	}

	failed := make(chan error, 1)
	go func() { failed <- srv.Serve(ln) }()
	logger.Printf("serving the OCI registry API (%s)", version)
	if ready != nil {
		ready <- ln.Addr().String()
	}

	code := 0
	select {
	case err := <-failed:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Printf("stopped: %v", err)
			code = 1
		}
	case <-ctx.Done():
	}

	stopGC()
	shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Printf("shutdown was not clean: %v", err)
		code = 1
	}
	wg.Wait()
	return code
}
