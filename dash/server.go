// Package dash is the embedded HTTP dashboard served by `jot dash`. It
// exposes a four-panel operator cockpit backed by the same core/* packages
// the CLI and future TUI use. No external web server, no build step —
// templates and static assets are compiled into the binary via go:embed.
package dash

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"jot/core/audit"
	"jot/core/config"
	"jot/core/events"
	"jot/core/iiq"
)

// Server holds the per-process state shared across handlers: config,
// clients, and the event bus. It is constructed once in Run and passed to
// each handler via method receivers.
type Server struct {
	cfg config.Jot
	iiq *iiq.Client // may be nil if IIQ not configured; handlers check
	bus *events.Bus
}

// Options configures a Run. Zero-value Options gives the default developer
// experience: auto-open browser when the server is up. Callers running
// jot on a headless machine (or over SSH) set NoOpen to suppress that.
type Options struct {
	// NoOpen disables the automatic browser launch. Set by `--no-open` /
	// `--headless` on the CLI.
	NoOpen bool
}

// Run starts the dashboard. It blocks until the process receives SIGINT or
// SIGTERM, then gracefully shuts down the HTTP server and any background
// goroutines.
//
// The startup sequence is deliberately:
//   1. load config
//   2. build iiq client (optional)
//   3. net.Listen on the configured addr — this is the moment the port is
//      actually bound and accepting, so (4) can't race the first request
//   4. open the operator's browser (unless opts.NoOpen)
//   5. Serve(ln) in a goroutine, then wait for shutdown
//
// If net.Listen fails (port in use, permission denied) we bail out BEFORE
// launching the browser so the operator gets a clean error on the tty
// instead of a failed browser tab pointing at nothing.
func Run(opts Options) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	s := &Server{
		cfg: cfg,
		bus: events.NewBus(),
	}

	// IIQ is optional — if it isn't configured the dash still boots and
	// the ticket panel shows a "configure me" message. Mirrors what users
	// see if they haven't filled in config.json yet.
	if client, err := iiq.New(cfg); err == nil {
		s.iiq = client
	} else {
		fmt.Fprintf(os.Stderr, "[dash] IIQ disabled: %v\n", err)
	}

	// Bind the socket up front. Doing this here (instead of inside
	// ListenAndServe in a goroutine) gives us two things:
	//   - a clean error on the tty if the port is in use
	//   - a hard guarantee that the socket is accepting before we open
	//     the browser, so the first tab doesn't see "connection refused"
	ln, err := net.Listen("tcp", cfg.Dash.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.Dash.Addr, err)
	}
	// Use the actual bound addr, not cfg.Dash.Addr — supports the
	// "addr: 127.0.0.1:0" trick (ephemeral port) and anyone who wrote
	// "localhost" instead of "127.0.0.1".
	url := "http://" + ln.Addr().String()

	mux := s.routes()
	httpSrv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Bridge audit appends onto the events bus so the Activity panel can
	// refresh in real time. Only appends that happen IN THIS PROCESS fire
	// this hook — jot CLI commands in another terminal will still show up,
	// but via the panel's 10s polling fallback rather than instantly.
	audit.SetHook(func(e audit.Entry) {
		s.bus.Publish(events.Event{Topic: events.TopicAuditAppend, Payload: e})
	})
	defer audit.SetHook(nil)

	// Background pollers. They terminate when pollerCtx is cancelled.
	pollerCtx, cancelPollers := context.WithCancel(context.Background())
	defer cancelPollers()
	if s.iiq != nil {
		go s.pollIIQ(pollerCtx)
	}

	// Graceful shutdown on Ctrl-C / SIGTERM.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	fmt.Printf("[dash] listening on %s\n", url)

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	// Open the browser now that the socket is live. A browser failure
	// (headless box, missing xdg-open) must not take down the server — the
	// operator can still visit the printed URL manually.
	if !opts.NoOpen {
		if err := openURL(url); err != nil {
			fmt.Fprintf(os.Stderr, "[dash] could not open browser (%v) — visit %s\n", err, url)
		}
	}

	select {
	case err := <-errCh:
		return err
	case <-stop:
		fmt.Println("\n[dash] shutting down…")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}

// routes wires the HTTP mux. Keep this small — handler bodies live in
// sibling files.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub()))))

	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/events", s.handleSSE)

	// Health strip
	mux.HandleFunc("/health/strip", s.handleHealthStrip)

	// IIQ ticket panel
	mux.HandleFunc("/iiq/queue", s.handleQueue)
	mux.HandleFunc("/iiq/take/", s.handleTake)       // /iiq/take/{id}
	mux.HandleFunc("/iiq/resolve/", s.handleResolve) // /iiq/resolve/{id}

	// Recent Activity panel (audit log tail).
	mux.HandleFunc("/audit/tail", s.handleActivity)

	// Quick Actions panel
	mux.HandleFunc("/ad/quick", s.handleQuickActions)
	mux.HandleFunc("/ad/reset", s.handleADReset)
	mux.HandleFunc("/ad/unlock", s.handleADUnlock)
	mux.HandleFunc("/ad/provision", s.handleADProvision)

	return mux
}
