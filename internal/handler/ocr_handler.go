// Package handler implements the OCR sidecar HTTP handlers.
package handler

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/CoverOnes/ocr-sidecar/internal/platform/httpx"
	"github.com/gin-gonic/gin"
)

// maxImageBytes is the maximum size of an image accepted by the OCR endpoint.
// This mirrors the kyc service's 8 MB limit so the sidecar never buffers more.
const maxImageBytes = 8 * 1024 * 1024

// ocrResponse is the JSON response for POST /ocr.
type ocrResponse struct {
	Name       string  `json:"name"`
	NationalID string  `json:"nationalId"`
	Confidence float64 `json:"confidence"`
}

// namePattern extracts a Chinese name (2–5 CJK characters) from tesseract output.
// Taiwan national ID cards print the holder's Chinese name in the top section.
var namePattern = regexp.MustCompile(`[\x{4E00}-\x{9FFF}]{2,5}`)

// nationalIDPattern extracts a Taiwan national ID number (1 letter + 9 digits).
var nationalIDPattern = regexp.MustCompile(`[A-Z][12]\d{8}`)

// OCRHandler handles POST /ocr.
type OCRHandler struct{}

// NewOCRHandler returns an OCRHandler.
func NewOCRHandler() *OCRHandler {
	return &OCRHandler{}
}

// Handle processes a POST /ocr request.
// Accepts: multipart/form-data with a single "image" field (JPEG or PNG, ≤ 8 MB).
// Returns: {name, nationalId, confidence} or an error code.
//
// The image is held in bounded memory and discarded after OCR — it is NEVER
// persisted to disk or stored anywhere beyond the temporary tesseract stdin pipe.
// If a temp file is required by tesseract CLI, it is written to os.TempDir()
// and deleted immediately via defer.
func (h *OCRHandler) Handle(c *gin.Context) {
	// Bound the request body BEFORE any read to defend against DoS.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxImageBytes)

	imgBytes, contentType, err := readImage(c)
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "INVALID_IMAGE", err.Error())
		return
	}

	// Validate content type against the magic-byte allowlist.
	if err := validateImageType(imgBytes, contentType); err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "INVALID_IMAGE_TYPE", err.Error())
		return
	}

	text, err := runTesseract(imgBytes)
	if err != nil {
		slog.Warn("tesseract OCR failed", "err", err)
		httpx.ErrCode(c, http.StatusUnprocessableEntity, "OCR_FAILED", "OCR processing failed")
		return
	}

	name, nationalID, confidence := extractFields(text)

	httpx.OK(c, ocrResponse{
		Name:       name,
		NationalID: nationalID,
		Confidence: confidence,
	})
}

// readImage reads the image bytes from multipart "image" field or raw body.
func readImage(c *gin.Context) ([]byte, string, error) {
	contentType := c.GetHeader("Content-Type")

	// If multipart: parse the "image" field.
	if strings.HasPrefix(contentType, "multipart/") {
		file, header, err := c.Request.FormFile("image")
		if err != nil {
			return nil, "", fmt.Errorf("missing image field: %w", err)
		}
		defer file.Close() //nolint:errcheck // close on read-only file handle; error not actionable

		if header.Size > maxImageBytes {
			return nil, "", fmt.Errorf("image exceeds %d bytes", maxImageBytes)
		}

		data, err := io.ReadAll(io.LimitReader(file, maxImageBytes))
		if err != nil {
			return nil, "", fmt.Errorf("read image: %w", err)
		}

		return data, header.Header.Get("Content-Type"), nil
	}

	// Otherwise treat the raw body as the image (Content-Type = image/jpeg or image/png).
	data, err := io.ReadAll(io.LimitReader(c.Request.Body, maxImageBytes))
	if err != nil {
		return nil, "", fmt.Errorf("read body: %w", err)
	}

	return data, contentType, nil
}

// validateImageType checks the magic bytes of img against the JPEG/PNG allowlist.
// Magic bytes: JPEG = FF D8 FF; PNG = 89 50 4E 47 0D 0A 1A 0A.
func validateImageType(img []byte, _ string) error {
	if len(img) < 4 {
		return fmt.Errorf("image too small to validate type")
	}

	isJPEG := img[0] == 0xFF && img[1] == 0xD8 && img[2] == 0xFF
	isPNG := img[0] == 0x89 && img[1] == 0x50 && img[2] == 0x4E && img[3] == 0x47

	if !isJPEG && !isPNG {
		return fmt.Errorf("image must be JPEG or PNG (got unknown format)")
	}

	return nil
}

// runTesseract writes img to a temporary file, invokes tesseract with
// chi_tra+eng language packs, and returns the OCR text output.
// The temp file is deleted immediately after the process exits.
func runTesseract(img []byte) (string, error) {
	// Write image to a temp file (tesseract CLI needs a file path).
	tmpFile, err := os.CreateTemp("", "ocr-*.img")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}

	tmpPath := tmpFile.Name()

	defer func() {
		// Always remove the temp file — image is never persisted beyond this scope.
		if removeErr := os.Remove(tmpPath); removeErr != nil {
			slog.Warn("failed to remove OCR temp file", "path", tmpPath, "err", removeErr)
		}
	}()

	if _, err := tmpFile.Write(img); err != nil {
		tmpFile.Close() //nolint:errcheck // best-effort close before returning error
		return "", fmt.Errorf("write temp file: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		return "", fmt.Errorf("close temp file: %w", err)
	}

	// Output goes to stdout via `-` target; tesseract writes to stdout when output = stdout.
	//nolint:gosec // G204: tmpPath is generated by os.CreateTemp with a fixed pattern prefix; not user-controlled
	cmd := exec.Command(
		"tesseract",
		filepath.Clean(tmpPath),
		"stdout",
		"-l", "chi_tra+eng",
		"--psm", "6",
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("tesseract: %w (stderr: %s)", err, stderr.String())
	}

	return stdout.String(), nil
}

// extractFields attempts to parse a Taiwan ID-card name and national ID from
// raw tesseract output. Returns (name, nationalId, confidence) where confidence
// is a heuristic: 0–100 based on field detection success.
func extractFields(text string) (name, nationalID string, confidence float64) {
	idMatches := nationalIDPattern.FindAllString(text, -1)
	if len(idMatches) > 0 {
		nationalID = idMatches[0]
	}

	nameMatches := namePattern.FindAllString(text, -1)
	if len(nameMatches) > 0 {
		name = nameMatches[0]
	}

	// Heuristic confidence: both fields found = 90; only ID = 60; only name = 50; neither = 20.
	switch {
	case nationalID != "" && name != "":
		confidence = 90
	case nationalID != "":
		confidence = 60
	case name != "":
		confidence = 50
	default:
		confidence = 20
	}

	return name, nationalID, confidence
}
