// Package httpx provides HTTP response helpers for the OCR sidecar.
package httpx

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// OK writes a 200 JSON response.
func OK(c *gin.Context, body any) {
	c.JSON(http.StatusOK, body)
}

// ErrCode writes an error JSON response with the given status code.
func ErrCode(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"code": code, "message": message})
}
