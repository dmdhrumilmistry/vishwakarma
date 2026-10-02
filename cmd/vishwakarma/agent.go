package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/dmdhrumilmistry/vishwakarma/internal/macos"
)

// agent runs the Mac host agent: `vishwakarma agent --public-host mac.lan`.
// The token comes from VK_AGENT_TOKEN or --token-file, never a flag, so it
// does not show up in the process list.
func agent(args []string) error {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	backend := fs.String("backend", "tart", "tart (real macOS VMs on this Mac) or simulator (no guests, for testing on Linux)")
	listen := fs.String("listen", ":8484", "agent API address")
	name := fs.String("name", "", "host name shown in the console (default: hostname)")
	publicHost := fs.String("public-host", "", "address users connect to for forwarded ports (this Mac's LAN IP or DNS name)")
	tokenFile := fs.String("token-file", "", "file holding the agent token (default: $VK_AGENT_TOKEN)")
	home, _ := os.UserHomeDir()
	state := fs.String("state", filepath.Join(home, ".vishwakarma", "agent.json"), "state file")
	bind := fs.String("bind", "0.0.0.0", "address forwarded ports listen on")
	portMin := fs.Int("port-min", 20000, "first host port for forwarding")
	portMax := fs.Int("port-max", 20999, "last host port for forwarding")
	maxRunning := fs.Int("max-running", 0, "VMs running at once (default: 2 for tart, Apple's limit)")
	allow := fs.String("allow-images", "", "comma-separated image prefixes to allow (default: any)")
	tartBin := fs.String("tart", "tart", "tart executable")
	tlsCert := fs.String("tls-cert", "", "serve the API over TLS with this certificate")
	tlsKey := fs.String("tls-key", "", "TLS private key")
	_ = fs.Parse(args)

	token := os.Getenv("VK_AGENT_TOKEN")
	if *tokenFile != "" {
		b, err := os.ReadFile(*tokenFile)
		if err != nil {
			return err
		}
		token = strings.TrimSpace(string(b))
	}
	if *publicHost == "" {
		return errors.New("--public-host is required: the address users reach this host's forwarded ports on")
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	var be macos.Backend
	switch *backend {
	case "tart":
		be = macos.NewTart(*tartBin)
	case "simulator":
		be = macos.NewSimulator()
	default:
		return fmt.Errorf("unknown backend %q", *backend)
	}
	var images []string
	for _, p := range strings.Split(*allow, ",") {
		if p = strings.TrimSpace(p); p != "" {
			images = append(images, p)
		}
	}
	a, err := macos.NewAgent(macos.AgentConfig{
		Name: *name, PublicHost: *publicHost, Token: token, StatePath: *state, BindAddr: *bind,
		PortMin: *portMin, PortMax: *portMax, MaxRunning: *maxRunning, AllowImages: images, Version: version,
	}, be, log)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go a.Run(ctx)

	hs := &http.Server{Addr: *listen, Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() {
		log.Info("agent listening", "addr", *listen, "backend", be.Name(), "publicHost", *publicHost, "tls", *tlsCert != "")
		if *tlsCert != "" {
			errc <- hs.ListenAndServeTLS(*tlsCert, *tlsKey)
		} else {
			errc <- hs.ListenAndServe()
		}
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return hs.Shutdown(sctx)
}
