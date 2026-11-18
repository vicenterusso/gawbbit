// Command collector-api is Rawbbit's public ingestion edge: it validates
// event batches, enriches them and publishes them to NATS JetStream.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/config"
	"github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/geoip"
	"github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/logx"
	"github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/publisher"
	"github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/server"
)

// Exit codes of the original uvicorn process.
const (
	exitConfigError   = 1
	exitStartupFailed = 3
)

// serverLog writes uvicorn-style process lines ("INFO:     ...") to stderr.
func serverLog(level, msg string) {
	fmt.Fprintf(os.Stderr, "%-9s %s\n", level+":", msg)
}

func pyBool(b bool) string {
	if b {
		return "True"
	}
	return "False"
}

func main() {
	host := flag.String("host", "0.0.0.0", "address to listen on")
	flag.Parse()
	os.Exit(run(*host))
}

func run(host string) int {
	settings, err := config.Load(os.Environ(), ".env")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitConfigError
	}
	level, err := logx.ParseLevel(settings.LogLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitConfigError
	}
	log := logx.New(os.Stderr, level)
	// UVICORN_WORKERS is accepted for compatibility and ignored: one Go
	// process serves requests concurrently.

	// Signals received during startup are handled once startup completes.
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)

	pid := os.Getpid()
	serverLog("INFO", fmt.Sprintf("Started server process [%d]", pid))
	serverLog("INFO", "Waiting for application startup.")
	log.Info("startup env=%s port=%d nats_url=%s stream=%s subject_prefix=%s max_request_bytes=%d max_events_per_request=%d max_inflight_publishes=%d cors_enabled=%s geoip_enabled=%s geoip_mmdb_path=%s",
		settings.Env, settings.Port, publisher.RedactedURL(settings.NATSURL), settings.NATSStream,
		settings.NATSSubjectPrefix, settings.MaxRequestBytes, settings.MaxEventsPerRequest,
		settings.NATSMaxInflightPublishes, pyBool(len(settings.CORSOriginsList()) > 0),
		pyBool(settings.GeoIPEnabled), settings.GeoIPMMDBPath)

	resolver := geoip.NewResolver(settings.GeoIPEnabled, settings.GeoIPMMDBPath, nil, log)
	resolver.Warmup()
	pub := publisher.New(publisher.Config{
		URL:                    settings.NATSURL,
		Stream:                 settings.NATSStream,
		SubjectPrefix:          settings.NATSSubjectPrefix,
		StreamMaxAgeSeconds:    settings.NATSStreamMaxAgeSeconds,
		DuplicateWindowSeconds: settings.NATSDuplicateWindowSeconds,
		MaxInflightPublishes:   settings.NATSMaxInflightPublishes,
	}, log)
	if err := pub.Connect(context.Background()); err != nil {
		serverLog("ERROR", fmt.Sprintf("%v", err))
		serverLog("ERROR", "Application startup failed. Exiting.")
		return exitStartupFailed
	}
	log.Info("nats_ready stream=%s", settings.NATSStream)

	forwardedAllowIPs, ok := os.LookupEnv("FORWARDED_ALLOW_IPS")
	if !ok {
		forwardedAllowIPs = "127.0.0.1"
	}
	handler := server.New(server.Options{
		Settings:          settings,
		Log:               log,
		AccessLog:         os.Stdout,
		Publisher:         pub,
		GeoIP:             resolver,
		ForwardedAllowIPs: forwardedAllowIPs,
	})

	addr := net.JoinHostPort(host, strconv.FormatInt(settings.Port, 10))
	listener, err := listen(host, settings.Port)
	if err != nil {
		serverLog("ERROR", fmt.Sprintf("error while attempting to bind on address %s: %v", addr, err))
		resolver.Close()
		pub.Close()
		return exitConfigError
	}
	srv := &http.Server{
		Handler: handler,
		// uvicorn's keep-alive timeout.
		IdleTimeout: 5 * time.Second,
	}
	serverLog("INFO", "Application startup complete.")
	serverLog("INFO", fmt.Sprintf("Listening on http://%s (Press CTRL+C to quit)", addr))

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(listener) }()

	var received os.Signal
	select {
	case err := <-serveErr:
		serverLog("ERROR", err.Error())
		return exitConfigError
	case received = <-signals:
	}

	serverLog("INFO", "Shutting down")
	// Like uvicorn, wait for in-flight requests without a deadline; a second
	// SIGINT stops waiting.
	done := make(chan error, 1)
	go func() { done <- srv.Shutdown(context.Background()) }()
	for waiting := true; waiting; {
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				serverLog("ERROR", err.Error())
			}
			waiting = false
		case sig := <-signals:
			if sig == syscall.SIGINT {
				_ = srv.Close()
			}
		}
	}
	serverLog("INFO", "Waiting for application shutdown.")
	resolver.Close()
	pub.Close()
	log.Info("shutdown_complete")
	serverLog("INFO", "Application shutdown complete.")
	serverLog("INFO", fmt.Sprintf("Finished server process [%d]", pid))
	reraise(received)
	return 0
}

// reraise terminates the process with the signal that stopped it, as
// uvicorn does after a graceful shutdown, so the exit status reports it.
func reraise(sig os.Signal) {
	s, ok := sig.(syscall.Signal)
	if !ok {
		return
	}
	signal.Reset(s)
	if err := syscall.Kill(os.Getpid(), s); err == nil {
		time.Sleep(time.Second) // the default action terminates us meanwhile
	}
}

// listen binds like uvicorn: an IPv4 host listens on IPv4 only.
func listen(host string, port int64) (net.Listener, error) {
	if port < 0 || port > 65535 {
		return nil, fmt.Errorf("port must be 0-65535")
	}
	network := "tcp"
	if ip := net.ParseIP(host); ip != nil {
		network = "tcp6"
		if ip.To4() != nil {
			network = "tcp4"
		}
	}
	return net.Listen(network, net.JoinHostPort(host, strconv.FormatInt(port, 10)))
}
