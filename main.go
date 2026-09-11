// Command browser-fetch renders web pages using a real, user-launched
// Chrome and serves the rendered HTML over a small authenticated HTTP API.
//
// It exists because bot protection (WAFs, JS challenges, captcha gates)
// fingerprints TLS and HTTP behaviour, so no header tweaking makes a
// server-side HTTP client look like a browser. A genuine browser on a
// residential connection, with a persistent profile you can log into by hand,
// passes those checks for the right reasons.
//
// Intended deployment: a Linux VM with a desktop, Chrome started as
//
//	google-chrome --user-data-dir="$HOME/agent-profile" --remote-debugging-port=9222
//
// then this gateway attached to it, listening on the VM's private interface.
// See README.md.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/slim-bean/browser-fetch/internal/browser"
	"github.com/slim-bean/browser-fetch/internal/config"
	"github.com/slim-bean/browser-fetch/internal/logx"
	"github.com/slim-bean/browser-fetch/internal/server"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "browser-fetch:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		return err
	}

	log := logx.New(cfg.LogFormat, cfg.LogLevel)
	log.Info("starting",
		"addr", cfg.Addr,
		"chrome", cfg.ChromeURL,
		"max_tabs", cfg.MaxTabs,
		"host_gap", cfg.HostGap,
		"host_jitter", cfg.HostJitter,
		"debug", cfg.Debug,
		"assist_timeout", cfg.AssistTimeout,
		"auth", cfg.Token != "",
	)
	if cfg.Token == "" {
		log.Warn("running without a bearer token: anyone who can reach this port can drive your browser")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mgr := browser.New(ctx, browser.Options{
		ChromeURL:           cfg.ChromeURL,
		MaxTabs:             cfg.MaxTabs,
		NavTimeout:          cfg.NavTimeout,
		ChallengeWait:       cfg.ChallengeWait,
		AssistTimeout:       cfg.AssistTimeout,
		ChallengeRetries:    cfg.ChallengeRetries,
		ChallengeRetryDelay: cfg.ChallengeRetryDelay,
		BlockMedia:          cfg.BlockMedia,
		Logger:              log,
	})
	defer mgr.Close()

	// Probe once at startup for a clear message, but do not fail: Chrome may
	// still be starting, and /healthz reports the live state.
	probeCtx, cancelProbe := context.WithTimeout(ctx, 5*time.Second)
	if err := mgr.Probe(probeCtx); err != nil {
		log.Warn("cannot reach Chrome DevTools yet", "url", cfg.ChromeURL, "err", err)
	} else {
		log.Info("attached to Chrome", "version", mgr.Health().Version)
	}
	cancelProbe()

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           server.New(cfg, log, mgr).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Long enough for assist mode, where a human clicks a challenge.
		WriteTimeout: cfg.RequestTimeout + cfg.AssistTimeout + 30*time.Second,
		IdleTimeout:  90 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutCtx)
}
