// Command rawbbit-mcp-server serves the read-only Rawbbit ClickHouse
// analytics tools over MCP streamable HTTP.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
	_ "time/tzdata" // DateTime columns need IANA zones even on scratch images

	"github.com/mirlan-irokez/rawbbit/mcp-server/internal/datasets"
	"github.com/mirlan-irokez/rawbbit/mcp-server/internal/server"
	"github.com/mirlan-irokez/rawbbit/mcp-server/internal/settings"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run())
}

func run() int {
	cfg, err := settings.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "RuntimeError: Invalid MCP server settings: %s\n", err)
		return 1
	}
	log, err := server.NewLogger(os.Stderr, cfg.LogLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ValueError: %s\n", err)
		return 1
	}
	reg, err := datasets.Load(cfg.MCPDatasetsFile, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "DatasetConfigurationError: %s\n", err)
		return 1
	}
	if cfg.AuthMode() == settings.AuthNone {
		log.Warning("auth_disabled reason=no_jwt_verifier_configured")
	}
	app := server.New(cfg, reg, server.DefaultGatewayFactory("rawbbit-mcp-server/"+version), log)

	log.Info("startup env=%s table=%s host=%s port=%d path=%s auth_mode=%s auth_enabled=%s enabled_datasets=%d",
		cfg.Env, cfg.TableRef(), cfg.MCPHost, cfg.MCPPort, cfg.MCPPath, cfg.AuthMode(),
		pyBool(cfg.AuthMode() != settings.AuthNone), len(reg.Datasets))
	log.Server("Started server process [%d]", os.Getpid())
	for i := 0; i < app.Endpoints(); i++ {
		log.Info("StreamableHTTP session manager started")
	}

	addr := net.JoinHostPort(cfg.MCPHost, strconv.FormatInt(cfg.MCPPort, 10))
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		// uvicorn reports bind failures like this and exits with status 3.
		errno := syscall.EADDRINUSE
		_ = errors.As(err, &errno)
		fmt.Fprintf(os.Stderr, "ERROR:    [Errno %d] error while attempting to bind on address ('%s', %d): [errno %d] %s\n",
			int(errno), cfg.MCPHost, cfg.MCPPort, int(errno), errno.Error())
		return 3
	}
	srv := &http.Server{Handler: app, ReadHeaderTimeout: 30 * time.Second}
	log.Server("Application startup complete.")
	log.Server("Listening on http://%s (Press CTRL+C to quit)", addr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(listener) }()
	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "ERROR:    %s\n", err)
			return 1
		}
	case <-ctx.Done():
		log.Server("Shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		log.Server("Finished server process [%d]", os.Getpid())
	}
	return 0
}

func pyBool(b bool) string {
	if b {
		return "True"
	}
	return "False"
}
