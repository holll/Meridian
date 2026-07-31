package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"meridian/internal"
	"meridian/web"
)

// appVersion is overridable at build time via -ldflags "-X main.appVersion=vX.Y.Z".
var appVersion = "v1.6.0"

func main() {
	if handled, err := internal.RunCommandLine(os.Args[1:], os.Stdin, os.Stdout, appVersion); handled {
		if err != nil {
			fmt.Fprintf(os.Stderr, "meridian: %v\n", err)
			os.Exit(1)
		}
		return
	}

	port := 9090
	dbPath := "meridian.db"
	if internal.JWTSecretEphemeral {
		log.Printf("JWT_SECRET not set; generated an ephemeral signing secret for this process. Set JWT_SECRET explicitly for stable sessions.")
	}

	if v := os.Getenv("PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			port = p
		}
	}
	if v := os.Getenv("DB_PATH"); v != "" {
		dbPath = v
	}

	routePrefix := "/s"
	if v := os.Getenv("ROUTE_PREFIX"); v != "" {
		routePrefix = v
	}
	if validated, err := internal.ValidateRoutePrefix(routePrefix); err != nil {
		log.Fatalf("invalid route prefix: %v", err)
	} else {
		routePrefix = validated
	}

	// Command line args
	for i, arg := range os.Args[1:] {
		switch arg {
		case "--port", "-p":
			if i+1 < len(os.Args)-1 {
				if p, err := strconv.Atoi(os.Args[i+2]); err == nil {
					port = p
				}
			}
		case "--db":
			if i+1 < len(os.Args)-1 {
				dbPath = os.Args[i+2]
			}
		}
	}

	db, err := internal.OpenDB(dbPath)
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer db.Close()

	pm := internal.NewProxyManager(db)
	loadedSiteCount, err := pm.StartAllEnabled()
	if err != nil {
		log.Fatalf("failed to load sites: %v", err)
	}

	// Traffic flush goroutine with context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				pm.FlushTraffic()
			case <-ctx.Done():
				return
			}
		}
	}()

	setupToken := ""
	userCount, err := db.UserCount()
	if err != nil {
		log.Fatalf("failed to count users: %v", err)
	}
	if userCount == 0 {
		setupToken = strings.TrimSpace(os.Getenv("SETUP_TOKEN"))
		if setupToken == "" {
			setupToken, err = internal.GenerateSetupToken()
			if err != nil {
				log.Fatalf("failed to generate initial setup token: %v", err)
			}
		}
		log.Printf("Initial setup token: %s", setupToken)
	}

	trustedProxies, err := internal.ParseTrustedProxyCIDRs(os.Getenv("TRUSTED_PROXY_CIDRS"))
	if err != nil {
		log.Fatalf("invalid trusted proxy configuration: %v", err)
	}

	relayToken := strings.TrimSpace(os.Getenv("RELAY_TOKEN"))
	if relayToken == "" {
		log.Printf("RELAY_TOKEN not set; relay API is disabled. Set RELAY_TOKEN to enable relay nodes.")
	}

	app := &internal.App{
		DB:             db,
		PM:             pm,
		SetupToken:     setupToken,
		RoutePrefix:    routePrefix,
		RelayToken:     relayToken,
		TrustedProxies: trustedProxies,
	}

	staticFS, err := fs.Sub(web.StaticFiles, "static")
	if err != nil {
		log.Fatalf("failed to initialize embedded files: %v", err)
	}

	accessLogPath := os.Getenv("ACCESS_LOG")
	if accessLogPath == "" {
		accessLogPath = "access.log"
	}
	accessLogFile, accessLogWriter, err := internal.OpenAccessLog(accessLogPath)
	if err != nil {
		log.Fatalf("failed to open access log: %v", err)
	}
	if accessLogFile != nil {
		defer accessLogFile.Close()
		log.Printf("Access log: %s (+ stdout)", accessLogPath)
	}

	router := internal.SetupRouter(app, pm, staticFS, accessLogWriter)

	addr, err := internal.PanelListenAddress(os.Getenv("PANEL_BIND_ADDR"), port)
	if err != nil {
		log.Fatalf("invalid panel listen address: %v", err)
	}
	srv := &http.Server{
		Addr:           addr,
		Handler:        router,
		ReadTimeout:    30 * time.Second,
		WriteTimeout:   0, // no write timeout for streaming
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 64 << 10,
	}

	log.Println("============================================================")
	log.Printf("  Meridian Master %s", appVersion)
	log.Printf("  Listening on: http://%s", addr)
	log.Printf("  Route prefix: %s", routePrefix)
	log.Printf("  Sites loaded: %d (%d running)", loadedSiteCount, pm.GetRunningCount())
	if relayToken != "" {
		log.Println("  Relay API:    enabled (RELAY_TOKEN set)")
	}
	log.Println("============================================================")

	// Signal handling for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server failed: %v", err)
		}
	}()

	<-sigCh
	log.Println("Received shutdown signal, stopping Meridian Master...")

	go func() {
		<-sigCh
		log.Println("Forced shutdown")
		os.Exit(1)
	}()

	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	pm.GracefulShutdown(shutdownCtx)
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
	}

	log.Println("Meridian Master stopped cleanly")
}
