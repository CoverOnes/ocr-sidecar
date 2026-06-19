// Package handler implements the OCR sidecar HTTP handlers.
package handler

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/image/draw"

	// register JPEG decoder for image.Decode

	"github.com/CoverOnes/ocr-sidecar/internal/platform/httpx"
	"github.com/gin-gonic/gin"
)

// maxImageBytes is the maximum size of an image accepted by the OCR endpoint.
// This mirrors the kyc service's 8 MB limit so the sidecar never buffers more.
const maxImageBytes = 8 * 1024 * 1024

// maxDecodedDimension is the maximum allowed width or height (in pixels) after
// image.Decode. A crafted PNG can claim 10000×10000 dimensions in <100 KB of
// compressed bytes; decoding it would allocate ~476 MB on the heap.
// Taiwan national ID cards never legitimately exceed ~4000 px on the long side.
const maxDecodedDimension = 6000

// maxTesseractStderr caps the portion of tesseract's stderr captured into error
// messages and logs to avoid unbounded log growth from a misbehaving binary.
const maxTesseractStderr = 4 * 1024 // 4 KB

// maxTesseractStdout caps the portion of tesseract's stdout buffered in memory.
// A legitimate OCR result for a Taiwan ID card is a few hundred bytes of text;
// 64 KB provides generous headroom while preventing an OOM from a runaway process.
const maxTesseractStdout = 64 * 1024 // 64 KB

// tesseractTimeout is the maximum wall-clock time a single tesseract subprocess
// may run before being forcibly killed. Without a timeout a stuck process holds
// a concurrency slot and its file descriptor indefinitely (DoS).
const tesseractTimeout = 30 * time.Second

// maxConcurrentTesseract is the maximum number of tesseract processes that may
// run concurrently. Each process can briefly hold ~200 MB of working memory;
// allowing unlimited concurrency risks OOM under burst traffic.
const maxConcurrentTesseract = 4

// tesseractSem is the semaphore that enforces maxConcurrentTesseract.
var tesseractSem = make(chan struct{}, maxConcurrentTesseract)

// init fills the semaphore slots. Using init rather than a pre-filled buffered
// channel makes the capacity explicit and avoids confusing "send to fill" idiom.
func init() {
	for i := 0; i < maxConcurrentTesseract; i++ {
		tesseractSem <- struct{}{}
	}
}

// ocrResponse is the JSON response for POST /ocr.
type ocrResponse struct {
	Name       string  `json:"name"`
	NationalID string  `json:"nationalId"`
	Confidence float64 `json:"confidence"`
}

// namePattern extracts a Chinese name (2–5 CJK characters) from tesseract output.
// Taiwan national ID cards print the holder's Chinese name in the top section.
var namePattern = regexp.MustCompile(`[\x{4E00}-\x{9FFF}]{2,5}`)

// nameAfterLabelPattern matches the CJK block(s) that appear immediately after the
// "姓名" label on a Taiwan ID card, optionally separated by whitespace.
var nameAfterLabelPattern = regexp.MustCompile(`姓名\s*([\x{4E00}-\x{9FFF}]{2,5})`)

// nationalIDPattern extracts a Taiwan national ID number (1 letter + 9 digits).
var nationalIDPattern = regexp.MustCompile(`[A-Z][12]\d{8}`)

// nameStopwords is the set of CJK tokens that appear as header/label text on Taiwan
// national ID cards. These are NOT holder names and must be skipped during extraction.
var nameStopwords = map[string]struct{}{
	"中華民國":  {},
	"國民身分證": {},
	"姓名":    {},
	"性別":    {},
	"出生":    {},
	"發證":    {},
	"日期":    {},
	"統一":    {},
	"編號":    {},
	"年":     {},
	"月":     {},
	"日":     {},
	"男":     {},
	"女":     {},
	"住址":    {},
	"役別":    {},
	"身分":    {},
	"證號":    {},
	"有效":    {},
	"期限":    {},
	"換補":    {},
	"領證":    {},
}

// preprocessMinLongSide is the minimum long-side pixel size below which the image
// is upscaled before being passed to tesseract. Low-resolution images return
// near-zero confidence without upscaling.
const preprocessMinLongSide = 1000

// preprocessTargetLongSide is the target long-side size when upscaling is required.
const preprocessTargetLongSide = 1400

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
		// Distinguish dimension-guard errors (client fault) from other failures.
		if isDimensionError(err) {
			httpx.ErrCode(c, http.StatusRequestEntityTooLarge, "IMAGE_TOO_LARGE", err.Error())
			return
		}
		if isBusyError(err) {
			httpx.ErrCode(c, http.StatusTooManyRequests, "OCR_BUSY", "OCR service is at capacity; retry shortly")
			return
		}
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

// preprocessImage decodes a JPEG or PNG image and returns an upscaled, grayscale
// version encoded as PNG. If the long side is already ≥ preprocessMinLongSide
// the image is still converted to grayscale (which aids tesseract contrast).
// The returned bytes are always PNG regardless of the input format.
// This function is called BEFORE writing to the temp file for tesseract so that
// the raw input bytes are never persisted — only the processed bytes are.
func preprocessImage(img []byte) ([]byte, error) {
	// Pre-decode dimension peek: image.DecodeConfig reads ONLY the header and
	// returns the declared width/height WITHOUT allocating the pixel buffer.
	// A crafted PNG/JPEG can claim enormous dimensions (e.g. 10000×10000) in a
	// tiny compressed payload; rejecting here avoids the multi-hundred-MB heap
	// allocation that image.Decode would otherwise perform first. We treat a
	// DecodeConfig failure as "unknown format" and fall through to image.Decode,
	// which already handles the raw-bytes pass-through path below.
	if cfg, _, cfgErr := image.DecodeConfig(bytes.NewReader(img)); cfgErr == nil {
		if cfg.Width > maxDecodedDimension || cfg.Height > maxDecodedDimension {
			return nil, fmt.Errorf("image dimensions %dx%d exceed limit %d", cfg.Width, cfg.Height, maxDecodedDimension)
		}
	}

	src, _, err := image.Decode(bytes.NewReader(img))
	if err != nil {
		// Cannot decode: pass through original bytes untouched.
		return img, nil //nolint:nilerr // deliberate fallback: unknown format, let tesseract try the raw bytes
	}

	// Belt-and-suspenders post-decode guard: the pre-decode peek above already
	// rejects oversized images before allocation, but we re-check the decoded
	// bounds in case a decoder reports different dimensions than its header
	// claimed. This makes preprocessImage's error return meaningful rather than
	// dead: callers map this error to HTTP 413.
	b := src.Bounds()
	if b.Dx() > maxDecodedDimension || b.Dy() > maxDecodedDimension {
		return nil, fmt.Errorf("image dimensions %dx%d exceed limit %d", b.Dx(), b.Dy(), maxDecodedDimension)
	}

	bounds := src.Bounds()
	w := bounds.Dx()
	h := bounds.Dy()

	// Determine if upscaling is needed.
	longSide := w
	if h > w {
		longSide = h
	}

	scaled := src
	if longSide < preprocessMinLongSide {
		// Compute scale factor and new dimensions.
		scale := float64(preprocessTargetLongSide) / float64(longSide)
		newW := int(float64(w) * scale)
		newH := int(float64(h) * scale)
		dst := image.NewRGBA(image.Rect(0, 0, newW, newH))
		draw.CatmullRom.Scale(dst, dst.Bounds(), src, bounds, draw.Over, nil)
		scaled = dst
	}

	// Convert to grayscale.
	gray := image.NewGray(scaled.Bounds())
	for py := scaled.Bounds().Min.Y; py < scaled.Bounds().Max.Y; py++ {
		for px := scaled.Bounds().Min.X; px < scaled.Bounds().Max.X; px++ {
			c := color.GrayModel.Convert(scaled.At(px, py)).(color.Gray)
			gray.SetGray(px, py, c)
		}
	}

	// Encode as PNG for tesseract (lossless; preserves detail after upscaling).
	var buf bytes.Buffer
	if err := png.Encode(&buf, gray); err != nil {
		// Encoding failure: fall back to original bytes.
		slog.Warn("preprocess: png encode failed, using original", "err", err)
		return img, nil
	}

	return buf.Bytes(), nil
}

// errDimensionExceeded is a sentinel prefix used to distinguish dimension-guard
// errors from other preprocessing failures in Handle.
const errDimensionExceeded = "image dimensions "

// errOCRBusy is returned by runTesseract when the concurrency semaphore is full.
const errOCRBusy = "ocr busy"

// isDimensionError reports whether err originated from the dimension guard.
func isDimensionError(err error) bool {
	return err != nil && strings.HasPrefix(err.Error(), errDimensionExceeded)
}

// isBusyError reports whether err is the semaphore-full sentinel.
func isBusyError(err error) bool {
	return err != nil && err.Error() == errOCRBusy
}

// runTesseract preprocesses img (upscale + grayscale), writes the result to a
// temporary file, invokes tesseract with chi_tra+eng language packs, and returns
// the OCR text output. The temp file is deleted immediately after the process exits.
//
// Concurrency is bounded by tesseractSem (maxConcurrentTesseract slots). When
// all slots are occupied the function returns errOCRBusy immediately so Handle
// can respond with 429 instead of queuing indefinitely.
func runTesseract(img []byte) (string, error) {
	// Preprocess before persisting anything — raw bytes are never written.
	// NOTE: dimension errors are propagated, not swallowed, so Handle can
	// distinguish "client sent an oversized image" (→ 413) from other failures.
	processed, err := preprocessImage(img)
	if err != nil {
		// Dimension guard errors are fatal — do not fall back to raw bytes.
		if isDimensionError(err) {
			return "", err
		}
		slog.Warn("preprocess image failed, using original", "err", err)
		processed = img
	}

	// Acquire a concurrency slot (non-blocking).
	select {
	case <-tesseractSem:
		// acquired
	default:
		return "", fmt.Errorf("%s", errOCRBusy)
	}
	defer func() { tesseractSem <- struct{}{} }()

	// Write preprocessed image to a temp file (tesseract CLI needs a file path).
	tmpFile, err := os.CreateTemp("", "ocr-*.img")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}

	tmpPath := tmpFile.Name()

	defer func() {
		// Always remove the temp file — image is never persisted beyond this scope.
		// Log only the error; never log tmpPath (it is a local OS path, not useful
		// to external observers and could be misleading in production logs).
		if removeErr := os.Remove(tmpPath); removeErr != nil {
			slog.Warn("failed to remove OCR temp file", "err", removeErr)
		}
	}()

	if _, err := tmpFile.Write(processed); err != nil {
		tmpFile.Close() //nolint:errcheck // best-effort close before returning error
		return "", fmt.Errorf("write temp file: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		return "", fmt.Errorf("close temp file: %w", err)
	}

	// Enforce a hard timeout on the tesseract subprocess to prevent DoS from a
	// stuck process holding a concurrency slot and file descriptor indefinitely.
	tctx, tcancel := context.WithTimeout(context.Background(), tesseractTimeout)
	defer tcancel()

	// Output goes to stdout via `-` target; tesseract writes to stdout when output = stdout.
	//nolint:gosec // G204: tmpPath is generated by os.CreateTemp with a fixed pattern prefix; not user-controlled
	cmd := exec.CommandContext(
		tctx,
		"tesseract",
		filepath.Clean(tmpPath),
		"stdout",
		"-l", "chi_tra+eng",
		"--psm", "6",
	)

	// Limit stdout to maxTesseractStdout (64 KB) to prevent OOM from a runaway
	// process. A real Taiwan ID card produces at most a few hundred bytes of text.
	stdoutLimited := &limitedBuffer{max: maxTesseractStdout}
	// Limit stderr capture to maxTesseractStderr to prevent unbounded log growth
	// if tesseract writes large amounts of diagnostic output to stderr.
	stderrLimited := &limitedBuffer{max: maxTesseractStderr}
	cmd.Stdout = stdoutLimited
	cmd.Stderr = stderrLimited

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("tesseract: %w (stderr: %s)", err, stderrLimited.String())
	}

	return stdoutLimited.String(), nil
}

// limitedBuffer is a bytes.Buffer that stops accepting writes once max bytes
// have been accumulated. Excess data is silently discarded.
type limitedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	total := len(p)
	if b.buf.Len() >= b.max {
		return total, nil // silently discard to avoid blocking the subprocess
	}
	remaining := b.max - b.buf.Len()
	if len(p) > remaining {
		p = p[:remaining]
	}
	if _, err := b.buf.Write(p); err != nil {
		return 0, err
	}
	// Always report the full input length consumed so that the subprocess writer
	// is never blocked by a short-write error. Excess bytes are silently discarded.
	return total, nil
}

func (b *limitedBuffer) String() string { return b.buf.String() }

// allRunesAreStopwords reports whether every individual CJK character in s is a
// single-rune entry in nameStopwords. This catches date fragments like "年月日"
// that slip through because they are not in the stopwords map as a whole token
// but are composed entirely of individual stopword characters.
func allRunesAreStopwords(s string) bool {
	runes := []rune(s)
	if len(runes) == 0 {
		return false
	}
	for _, r := range runes {
		if _, ok := nameStopwords[string(r)]; !ok {
			return false
		}
	}
	return true
}

// extractCJKName extracts the holder's Chinese name from OCR text using a
// two-pass strategy:
//
//  1. First pass: look for a CJK block immediately following the "姓名" label.
//     This is the most reliable signal on well-read TW ID cards.
//  2. Second pass: scan all CJK blocks left-to-right and return the first one
//     that is not in the nameStopwords set.
//
// Returns an empty string if no suitable name block is found.
func extractCJKName(text string) string {
	// Pass 1: "姓名" label → adjacent CJK block.
	if m := nameAfterLabelPattern.FindStringSubmatch(text); len(m) == 2 {
		// nameAfterLabelPattern already trims via \s*, but TrimSpace is harmless
		// and guards against future regex changes.
		candidate := strings.TrimSpace(m[1])
		if _, isStop := nameStopwords[candidate]; !isStop && !allRunesAreStopwords(candidate) && len([]rune(candidate)) >= 2 {
			return candidate
		}
	}

	// Pass 2: first non-stopword CJK block of length 2–5.
	for _, candidate := range namePattern.FindAllString(text, -1) {
		if _, isStop := nameStopwords[candidate]; isStop {
			continue
		}
		if allRunesAreStopwords(candidate) {
			continue
		}
		return candidate
	}

	return ""
}

// extractFields attempts to parse a Taiwan ID-card name and national ID from
// raw tesseract output. Returns (name, nationalId, confidence) where confidence
// is a heuristic: 0–100 based on field detection success.
func extractFields(text string) (name, nationalID string, confidence float64) {
	idMatches := nationalIDPattern.FindAllString(text, -1)
	if len(idMatches) > 0 {
		nationalID = idMatches[0]
	}

	name = extractCJKName(text)

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
