package handler

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/CoverOnes/ocr-sidecar/internal/platform/health"
	"github.com/CoverOnes/ocr-sidecar/internal/platform/httpx"
	"github.com/gin-gonic/gin"
)

// NewRouter builds and returns the configured Gin engine for the OCR sidecar.
// It reads OCR_SERVICE_TOKEN from the environment at construction time.
// If the env var is non-empty, POST /ocr requires the caller to present the
// matching value in the X-Ocr-Service-Token header (constant-time compare).
// /healthz and /readyz are always unauthenticated.
func NewRouter() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)

	r := gin.New()
	r.SetTrustedProxies(nil) //nolint:errcheck // nil proxy list disables proxy trust; gin docs confirm error is always nil for nil argument

	r.Use(func(c *gin.Context) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("panic recovered", "panic", rec)
				c.AbortWithStatus(500)
			}
		}()
		c.Next()
	})

	r.Use(accessLogger())

	h := health.NewHandler()
	r.GET("/healthz", h.Liveness)
	r.GET("/readyz", h.Readiness)

	ocrH := NewOCRHandler()
	r.POST("/ocr", ocrAuthMiddleware(), ocrH.Handle)

	return r
}

// ocrAuthMiddleware returns a Gin middleware that enforces the OCR_SERVICE_TOKEN
// shared secret. It is loaded once at server start (not per-request) to avoid
// repeated os.Getenv calls in the hot path.
//
// Behaviour:
//   - Token non-empty → require X-Ocr-Service-Token header to match; 401 otherwise.
//   - Token empty     → dev fallback: allow all, emit a slog.Warn once at startup.
func ocrAuthMiddleware() gin.HandlerFunc {
	token := os.Getenv("OCR_SERVICE_TOKEN")
	if token == "" {
		slog.Warn("ocr-sidecar running WITHOUT auth (OCR_SERVICE_TOKEN unset) — dev only")
		return func(c *gin.Context) { c.Next() }
	}

	tokenBytes := []byte(token)

	return func(c *gin.Context) {
		got := c.GetHeader("X-Ocr-Service-Token")
		// subtle.ConstantTimeCompare prevents timing-based token enumeration.
		if subtle.ConstantTimeCompare([]byte(got), tokenBytes) != 1 {
			httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "missing or invalid X-Ocr-Service-Token")
			c.Abort()
			return
		}
		c.Next()
	}
}

// accessLogger returns a minimal slog-based access-log middleware.
func accessLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		if path == "/healthz" || path == "/readyz" {
			c.Next()
			return
		}

		start := time.Now()
		c.Next()
		slog.Info(
			"http",
			"method", c.Request.Method,
			"path", path,
			"status", c.Writer.Status(),
			"latency_ms", time.Since(start).Milliseconds(),
		)
	}
}
