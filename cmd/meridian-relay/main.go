package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"meridian/internal"
	"meridian/internal/relay"
)

var appVersion = "v1.6.0"

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Println(appVersion)
		return
	}

	masterURL := strings.TrimSpace(os.Getenv("MASTER_URL"))
	if masterURL == "" {
		log.Fatal("MASTER_URL is required (e.g. https://panel.example.com)")
	}
	relayToken := strings.TrimSpace(os.Getenv("RELAY_TOKEN"))
	if relayToken == "" {
		log.Fatal("RELAY_TOKEN is required")
	}
	relayName := strings.TrimSpace(os.Getenv("RELAY_NAME"))
	if relayName == "" {
		relayName = "relay-default"
	}
	isp := strings.TrimSpace(os.Getenv("RELAY_ISP"))

	port := 9090
	if v := os.Getenv("PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			port = p
		}
	}

	bindAddr := strings.TrimSpace(os.Getenv("PANEL_BIND_ADDR"))
	if bindAddr == "" {
		bindAddr = "0.0.0.0"
	}
	if net.ParseIP(bindAddr) == nil {
		log.Fatalf("PANEL_BIND_ADDR must be an IP address, got %q", bindAddr)
	}
	addr := net.JoinHostPort(bindAddr, strconv.Itoa(port))

	// Relay uses no database — pass nil.
	pm := internal.NewProxyManager(nil)

	syncer := relay.NewSyncer(relay.Config{
		MasterURL:  masterURL,
		RelayToken: relayToken,
		RelayName:  relayName,
		ISP:        isp,
		Version:    appVersion,
	}, pm)

	// Initial synchronous sync: fetches sites + route_prefix from Master before
	// starting the HTTP server. If Master is unreachable, we log a warning and
	// continue — the background loop will retry every 30 s.
	log.Println("[relay] performing initial config sync...")
	syncer.Sync()
	routePrefix := syncer.RoutePrefix()
	log.Printf("[relay] route_prefix from master: %q", routePrefix)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go syncer.Run(ctx)

	// Minimal HTTP server: just proxy routing, no panel, no auth.
	// routePrefix is read from the syncer on every request so it updates
	// automatically after background syncs.
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		rp := syncer.RoutePrefix()
		path := r.URL.Path
		if rp != "" && (path == rp || strings.HasPrefix(path, rp+"/")) {
			r2 := r.Clone(r.Context())
			r2.URL.Path = strings.TrimPrefix(path, rp)
			if r2.URL.Path == "" {
				r2.URL.Path = "/"
			}
			if r.URL.RawPath != "" {
				r2.URL.RawPath = strings.TrimPrefix(r.URL.RawPath, rp)
				if r2.URL.RawPath == "" {
					r2.URL.RawPath = "/"
				}
			}
			if pm.TryServe(w, r2) {
				return
			}
		} else if rp == "" {
			if pm.TryServe(w, r) {
				return
			}
		}
		http.NotFound(w, r)
	})

	srv := &http.Server{
		Addr:           addr,
		Handler:        mux,
		ReadTimeout:    30 * time.Second,
		WriteTimeout:   0,
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 64 << 10,
	}

	log.Println("============================================================")
	log.Printf("  Meridian Relay %s", appVersion)
	log.Printf("  Node name:    %s", relayName)
	if isp != "" {
		log.Printf("  ISP:          %s", isp)
	}
	log.Printf("  Master:       %s", masterURL)
	log.Printf("  Listening on: http://%s", addr)
	log.Printf("  Route prefix: %s (from master)", routePrefix)
	log.Println("============================================================")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("relay server failed: %v", err)
		}
	}()

	<-sigCh
	log.Println("Received shutdown signal, stopping Meridian Relay...")

	go func() {
		<-sigCh
		log.Println("Forced shutdown")
		os.Exit(1)
	}()

	cancel() // triggers syncer.flushTraffic() via ctx.Done()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	pm.GracefulShutdown(shutdownCtx)
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("relay HTTP server shutdown error: %v", err)
	}

	log.Println("Meridian Relay stopped cleanly")
}
