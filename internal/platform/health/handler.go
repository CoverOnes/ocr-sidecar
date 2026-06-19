// Package health provides liveness and readiness probe handlers for the OCR sidecar.
package health

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Handler implements /healthz and /readyz probes.
type Handler struct{}

// NewHandler returns a Handler.
func NewHandler() *Handler {
	return &Handler{}
}

// Liveness handles GET /healthz — always 200 when the process is running.
func (h *Handler) Liveness(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// Readiness handles GET /readyz — checks tesseract is available.
// On failure a generic message is returned; OS/path details are not exposed to
// unauthenticated callers to avoid leaking information about the runtime environment.
func (h *Handler) Readiness(c *gin.Context) {
	if err := checkTesseract(); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not ready", "error": "tesseract unavailable"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
