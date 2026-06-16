package handler

import (
	"log/slog"
	"time"

	"github.com/CoverOnes/ocr-sidecar/internal/platform/health"
	"github.com/gin-gonic/gin"
)

// NewRouter builds and returns the configured Gin engine for the OCR sidecar.
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
	r.POST("/ocr", ocrH.Handle)

	return r
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
