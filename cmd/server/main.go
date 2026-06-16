// Package main is the entry point for the OCR sidecar service.
// The sidecar wraps tesseract-ocr and exposes POST /ocr, returning extracted
// name, national ID, and a confidence score from ID-card images.
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
	"strconv"
	"syscall"
	"time"

	"github.com/CoverOnes/ocr-sidecar/internal/handler"
)

const (
	defaultPort       = 8085
	shutdownTimeout   = 10 * time.Second
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 30 * time.Second
)

func main() {
	healthcheck := flag.Bool("healthcheck", false, "run a liveness probe and exit")
	flag.Parse()

	port := getPort()

	if *healthcheck {
		os.Exit(runHealthcheck(port))
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	r := handler.NewRouter()

	srv := &http.Server{
		Addr:              net.JoinHostPort("", strconv.Itoa(port)),
		Handler:           r,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
	}

	slog.Info("ocr-sidecar starting", "port", port)

	// Run server in background so we can handle signals for graceful shutdown.
	errCh := make(chan error, 1)

	go func() {
		if listenErr := srv.ListenAndServe(); listenErr != nil && !errors.Is(listenErr, http.ErrServerClosed) {
			errCh <- listenErr
		}

		close(errCh)
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case listenErr := <-errCh:
		if listenErr != nil {
			slog.Error("server error", "err", listenErr)
			os.Exit(1)
		}
	case <-quit:
		slog.Info("shutting down gracefully")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if shutdownErr := srv.Shutdown(shutdownCtx); shutdownErr != nil {
		slog.Error("shutdown error", "err", shutdownErr)
		os.Exit(1)
	}

	slog.Info("ocr-sidecar stopped")
}

// getPort reads OCR_PORT from the environment, falling back to defaultPort.
func getPort() int {
	if v := os.Getenv("OCR_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 && p <= 65535 {
			return p
		}
	}

	return defaultPort
}

// runHealthcheck performs a GET /healthz against the running server and returns
// 0 on success, 1 on failure. Used by Docker HEALTHCHECK and compose.
func runHealthcheck(port int) int {
	url := fmt.Sprintf("http://localhost:%d/healthz", port)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: build request: %v\n", err)
		return 1
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}

	defer resp.Body.Close() //nolint:errcheck // best-effort close on health probe response

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: status %d\n", resp.StatusCode)
		return 1
	}

	return 0
}
