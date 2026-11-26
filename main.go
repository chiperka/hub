// chiperka-hub — Chiperka Hub, a self-hosted Chiperka control panel.
//
// It speaks the same API as Chiperka Cloud towards the stock chiperka CLI
// (`chiperka test --cloud`) and chiperka-worker, stores everything in SQLite +
// files under the data directory, and serves the web UI.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"chiperka-hub/internal/cloud"
	"chiperka-hub/internal/server"
	"chiperka-hub/internal/store"
	"chiperka-hub/web"
)

var version = "dev"

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := strings.ToLower(os.Getenv(key))
	switch v {
	case "1", "true", "yes", "on", "enabled":
		return true
	case "0", "false", "no", "off", "disabled":
		return false
	}
	return def
}

func envInt(key string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return n
	}
	return def
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "cloud" {
		os.Exit(runCloudCommand(os.Args[2:]))
	}
	addr := flag.String("addr", env("CHIPERKA_ADDR", ":8080"), "listen address (CHIPERKA_ADDR)")
	dataDir := flag.String("data", env("CHIPERKA_DATA_DIR", "./data"), "data directory (CHIPERKA_DATA_DIR)")
	showVersion := flag.Bool("version", false, "print version")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	jobTimeout, err := time.ParseDuration(env("CHIPERKA_JOB_TIMEOUT", "300s"))
	if err != nil {
		log.Error("invalid CHIPERKA_JOB_TIMEOUT", "err", err)
		os.Exit(2)
	}
	authDisabled := !envBool("CHIPERKA_AUTH", true)

	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		log.Error("cannot create data dir", "dir", *dataDir, "err", err)
		os.Exit(1)
	}
	st, err := store.Open(filepath.Join(*dataDir, "chiperka.db"))
	if err != nil {
		log.Error("cannot open database", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	srv := server.New(server.Config{
		DataDir:       *dataDir,
		PublicURL:     os.Getenv("CHIPERKA_PUBLIC_URL"),
		AuthDisabled:  authDisabled,
		Version:       version,
		JobTimeout:    jobTimeout,
		RetentionDays: envInt("CHIPERKA_RETENTION_DAYS", 30),
		WorkerImage:   env("CHIPERKA_WORKER_IMAGE", "finie/chiperka-worker:latest"),

		WorkerJoinToken: os.Getenv("CHIPERKA_WORKER_JOIN_TOKEN"),
	}, st, web.FS())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := srv.EnsureAdmin(ctx, os.Getenv("CHIPERKA_ADMIN_EMAIL"), os.Getenv("CHIPERKA_ADMIN_PASSWORD")); err != nil {
		log.Error("cannot create admin user", "err", err)
		os.Exit(1)
	}

	// Cloud workers on demand: active only with provider credentials.
	providers := cloudProviders()
	// Cloud servers' tunnels end on a loopback listener that serves only the
	// worker protocol, not the UI/admin API.
	workerLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Error("cannot open the listener for cloud workers", "err", err)
		os.Exit(1)
	}
	workerSrv := &http.Server{Handler: srv.WorkerHandler(), ReadHeaderTimeout: 10 * time.Second}
	go workerSrv.Serve(workerLn)
	defer workerSrv.Close()

	scaler, err := cloud.New(ctx, cloud.Config{
		DataDir:     *dataDir,
		HubAddr:     workerLn.Addr().String(),
		WorkerImage: env("CHIPERKA_WORKER_IMAGE", "finie/chiperka-worker:latest"),
		TestTimeout: jobTimeout,
		Providers:   providers,
	}, st, srv.CloudHooks())
	if err != nil {
		log.Error("cannot start the cloud autoscaler", "err", err)
		os.Exit(1)
	}
	srv.SetCloud(scaler)

	httpSrv := &http.Server{Addr: *addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Error("listen failed", "addr", *addr, "err", err)
		os.Exit(1)
	}
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server", "err", err)
			stop()
		}
	}()
	log.Info("chiperka-hub listening", "addr", *addr, "data", *dataDir, "version", version, "auth", !authDisabled)
	if authDisabled {
		log.Warn("authentication is DISABLED (CHIPERKA_AUTH=disabled) — do not expose this instance publicly")
	}

	go srv.RunMonitor(ctx)
	if scaler.Configured() {
		log.Info("cloud autoscaler enabled", "providers", strings.Join(mapKeys(providers), ","))
		go scaler.Run(ctx)
	}

	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	httpSrv.Shutdown(shutdownCtx)
}

func mapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
