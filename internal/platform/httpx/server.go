package httpx

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/thulasiram/oto/internal/platform/config"
)

// NewServer builds the HTTP server from configuration. Every timeout is explicit:
// an unbounded server is an outage waiting for a slow client.
func NewServer(cfg config.HTTPConfig, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              cfg.Addr,
		Handler:           h,
		ReadTimeout:       cfg.ReadTimeout,
		ReadHeaderTimeout: cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    1 << 20,
	}
}

// Serve runs srv until ctx is cancelled, then drains in-flight requests within
// cfg.ShutdownTimeout. It returns nil on a clean shutdown.
func Serve(ctx context.Context, srv *http.Server, cfg config.HTTPConfig) error {
	errCh := make(chan error, 1)

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		_ = srv.Close()
		return fmt.Errorf("httpx: graceful shutdown: %w", err)
	}
	return <-errCh
}
