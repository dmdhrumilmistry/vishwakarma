// Command vishwakarma serves the sandbox API and web console.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/dmdhrumilmistry/vishwakarma/internal/api"
	"github.com/dmdhrumilmistry/vishwakarma/internal/auth"
	"github.com/dmdhrumilmistry/vishwakarma/internal/config"
	"github.com/dmdhrumilmistry/vishwakarma/internal/kube"
	"github.com/dmdhrumilmistry/vishwakarma/internal/macos"
	"github.com/dmdhrumilmistry/vishwakarma/internal/sandbox"
	"github.com/dmdhrumilmistry/vishwakarma/internal/web"
)

var version = "dev"

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "serve":
		if err := serve(); err != nil {
			fmt.Fprintln(os.Stderr, "vishwakarma:", err)
			os.Exit(1)
		}
	case "agent":
		if err := agent(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "vishwakarma agent:", err)
			os.Exit(1)
		}
	case "check-config":
		// Validates the policy file without touching the cluster.
		path := "/etc/vishwakarma/config.yaml"
		if len(os.Args) > 2 {
			path = os.Args[2]
		}
		raw, err := os.ReadFile(path)
		if err == nil {
			_, err = config.ParsePolicy(raw)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("ok")
	case "version", "--version", "-v":
		fmt.Println(version)
	default:
		fmt.Fprintf(os.Stderr, "usage: vishwakarma [serve|agent [flags]|check-config [file]|version]\n")
		os.Exit(2)
	}
}

// blockAPIServer denies sandboxes egress to the API server endpoints.
func blockAPIServer(clients *kube.Clients, p *config.Policy, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	addrs, err := clients.APIServerAddresses(ctx)
	if err != nil {
		log.Warn("could not read the API server endpoints; sandboxes may reach the API server unless blockCIDRs covers it", "err", err)
		return
	}
	for _, a := range addrs {
		ip := net.ParseIP(a)
		if ip == nil || ip.To4() == nil {
			continue // the egress rule allows 0.0.0.0/0 only, so IPv6 needs no exception
		}
		cidr := a + "/32"
		if !slices.Contains(p.Network.BlockCIDRs, cidr) {
			p.Network.BlockCIDRs = append(p.Network.BlockCIDRs, cidr)
		}
	}
	log.Info("sandbox egress blocked", "cidrs", p.Network.BlockCIDRs)
}

func serve() error {
	if version != "dev" {
		// The Android screen sidecar is released with the server.
		config.DefaultAndroidScreenImage = "docker.io/dmdhrumilmistry/vishwakarma-android-screen:" + strings.TrimPrefix(version, "v")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	level := slog.LevelInfo
	_ = level.UnmarshalText([]byte(cfg.LogLevel))
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	clients, err := kube.New(cfg.Kubeconfig)
	if err != nil {
		return err
	}
	if cfg.Policy.Network.Isolate && cfg.Policy.Network.BlockAPIServer {
		blockAPIServer(clients, &cfg.Policy, log)
	}
	mgr := sandbox.NewManager(clients.Kube, clients.Dynamic, &cfg.Policy, clients.KubeVirtServed, log)
	if pool := macos.NewPool(cfg.Policy.MacOS.Agents, cfg.MacOSToken); pool != nil {
		mgr.SetMacOS(pool)
		log.Info("macOS hosts configured", "agents", len(cfg.Policy.MacOS.Agents))
	}
	authn := auth.New(cfg.Auth)
	srv := api.New(mgr, authn, clients, version, log)

	mux := http.NewServeMux()
	srv.Routes(mux)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := clients.Ready(ctx, cfg.Policy.Namespace); err != nil {
			http.Error(w, "not ready: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("/", web.Handler())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go mgr.RunReaper(ctx, time.Minute)

	hs := &http.Server{
		Addr:              cfg.Listen,
		Handler:           web.SecurityHeaders(mux),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Listen, "version", version, "namespace", cfg.Policy.Namespace,
			"auth", cfg.Auth.Mode, "vms", mgr.VMsAvailable(ctx))
		errc <- hs.ListenAndServe()
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return hs.Shutdown(sctx)
}
