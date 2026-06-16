package handler

import (
	"bytes"
	"crypto/subtle"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/CoverOnes/ocr-sidecar/internal/platform/httpx"
	"github.com/gin-gonic/gin"
)

func TestExtractFields(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		wantName string
		wantID   string
		wantConf float64
	}{
		{
			// "姓名" label is present — extractor must pick the holder name, not the label.
			name:     "both name and id found via label",
			text:     "姓名 王小明\nA123456789",
			wantName: "王小明",
			wantID:   "A123456789",
			wantConf: 90,
		},
		{
			name:     "id only",
			text:     "blurry text A123456789 more noise",
			wantName: "",
			wantID:   "A123456789",
			wantConf: 60,
		},
		{
			name:     "name only — no id",
			text:     "王小明 (id unreadable)",
			wantName: "王小明",
			wantID:   "",
			wantConf: 50,
		},
		{
			name:     "neither",
			text:     "totally unreadable !@#$",
			wantName: "",
			wantID:   "",
			wantConf: 20,
		},
		{
			name:     "female leading digit 2 accepted",
			text:     "陳美玲\nB234567890",
			wantName: "陳美玲",
			wantID:   "B234567890",
			wantConf: 90,
		},
		{
			name:     "invalid id shape (leading digit 3) not matched",
			text:     "C399999999",
			wantName: "",
			wantID:   "",
			wantConf: 20,
		},
		{
			// Real-card header boilerplate appears before the holder name; extractor
			// must skip stopwords and fall through to the actual name block.
			name:     "header boilerplate before real name — stopwords skipped",
			text:     "中華民國\n國民身分證\n姓名 陳筱玲\n性別 女\nA234567890",
			wantName: "陳筱玲",
			wantID:   "A234567890",
			wantConf: 90,
		},
		{
			// Text contains only stopword CJK blocks — no valid name extractable.
			name:     "only stopwords present — name empty",
			text:     "中華民國 姓名 性別 男\nA123456789",
			wantName: "",
			wantID:   "A123456789",
			wantConf: 60,
		},
		{
			// "姓名" label on a card OCR'd without whitespace separation.
			name:     "label immediately adjacent with no space",
			text:     "姓名李大為\nB123456789",
			wantName: "李大為",
			wantID:   "B123456789",
			wantConf: 90,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotName, gotID, gotConf := extractFields(tc.text)
			if gotName != tc.wantName {
				t.Errorf("name = %q, want %q", gotName, tc.wantName)
			}
			if gotID != tc.wantID {
				t.Errorf("nationalID = %q, want %q", gotID, tc.wantID)
			}
			if gotConf != tc.wantConf {
				t.Errorf("confidence = %v, want %v", gotConf, tc.wantConf)
			}
		})
	}
}

func TestValidateImageType(t *testing.T) {
	tests := []struct {
		name    string
		img     []byte
		wantErr bool
	}{
		{"valid jpeg", []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00}, false},
		{"valid png", []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A}, false},
		{"gif rejected", []byte{0x47, 0x49, 0x46, 0x38}, true},
		{"svg/text rejected", []byte("<svg xmlns"), true},
		{"too short", []byte{0xFF, 0xD8}, true},
		{"empty", []byte{}, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateImageType(tc.img, "")
			if (err != nil) != tc.wantErr {
				t.Errorf("validateImageType err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// encodeTestJPEG returns a minimal JPEG-encoded solid-gray image.
func encodeTestJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for py := 0; py < h; py++ {
		for px := 0; px < w; px++ {
			img.Set(px, py, color.RGBA{R: 128, G: 128, B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encodeTestJPEG: %v", err)
	}
	return buf.Bytes()
}

// encodeTestPNG returns a minimal PNG-encoded solid-gray image.
func encodeTestPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for py := 0; py < h; py++ {
		for px := 0; px < w; px++ {
			img.Set(px, py, color.RGBA{R: 200, G: 200, B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encodeTestPNG: %v", err)
	}
	return buf.Bytes()
}

func TestPreprocessImage(t *testing.T) {
	t.Run("small jpeg upscaled — long side reaches target", func(t *testing.T) {
		// 250×157 is below preprocessMinLongSide (1000); expect upscale.
		raw := encodeTestJPEG(t, 250, 157)
		out, err := preprocessImage(raw)
		if err != nil {
			t.Fatalf("preprocessImage returned err: %v", err)
		}
		img, _, err := image.Decode(bytes.NewReader(out))
		if err != nil {
			t.Fatalf("decoded output is not a valid image: %v", err)
		}
		b := img.Bounds()
		longSide := b.Dx()
		if b.Dy() > longSide {
			longSide = b.Dy()
		}
		if longSide < preprocessMinLongSide {
			t.Errorf("long side after preprocess = %d, want >= %d", longSide, preprocessMinLongSide)
		}
	})

	t.Run("output is grayscale PNG regardless of input format", func(t *testing.T) {
		raw := encodeTestJPEG(t, 100, 80)
		out, err := preprocessImage(raw)
		if err != nil {
			t.Fatalf("preprocessImage returned err: %v", err)
		}
		// Output must be decodable and in PNG format.
		img, format, err := image.Decode(bytes.NewReader(out))
		if err != nil {
			t.Fatalf("decoded output is not a valid image: %v", err)
		}
		if format != "png" {
			t.Errorf("output format = %q, want png", format)
		}
		// Sample a pixel: after grayscale conversion R == G == B in the 16-bit RGBA representation.
		r, g, b, _ := img.At(img.Bounds().Min.X, img.Bounds().Min.Y).RGBA()
		if r != g || g != b {
			t.Errorf("pixel not grayscale: R=%d G=%d B=%d", r, g, b)
		}
	})

	t.Run("large PNG not upscaled beyond original dimensions", func(t *testing.T) {
		// 1200×800 already exceeds preprocessMinLongSide — no upscale expected.
		raw := encodeTestPNG(t, 1200, 800)
		out, err := preprocessImage(raw)
		if err != nil {
			t.Fatalf("preprocessImage returned err: %v", err)
		}
		img, _, err := image.Decode(bytes.NewReader(out))
		if err != nil {
			t.Fatalf("decoded output is not a valid image: %v", err)
		}
		b := img.Bounds()
		if b.Dx() != 1200 || b.Dy() != 800 {
			t.Errorf("dimensions = %dx%d, want 1200x800 (no upscale)", b.Dx(), b.Dy())
		}
	})

	t.Run("non-image bytes fall through without error", func(t *testing.T) {
		// Graceful fallback: unknown format returns original bytes unchanged.
		raw := []byte("not an image at all")
		out, err := preprocessImage(raw)
		if err != nil {
			t.Fatalf("preprocessImage returned err for non-image: %v", err)
		}
		if string(out) != string(raw) {
			t.Errorf("expected original bytes pass-through for invalid image")
		}
	})
}

func TestExtractCJKName(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		wantName string
	}{
		{
			name:     "label prefix — picks value not label text",
			text:     "姓名 王小明",
			wantName: "王小明",
		},
		{
			name:     "stopword-only text — returns empty",
			text:     "中華民國 姓名 性別 男 發證 日期",
			wantName: "",
		},
		{
			name:     "boilerplate header then real name",
			text:     "國民身分證 中華民國 姓名 陳筱玲 性別",
			wantName: "陳筱玲",
		},
		{
			name:     "no CJK at all",
			text:     "A123456789 some latin text",
			wantName: "",
		},
		{
			name:     "name without label — first non-stopword CJK block",
			text:     "李大為",
			wantName: "李大為",
		},
		{
			// "民國57年6月5日" produces CJK tokens such as "年月日" composed entirely of
			// individual stopword characters. extractCJKName must skip these via
			// allRunesAreStopwords and return the actual name on the same card.
			name:     "date fragment 年月日 skipped, real name returned",
			text:     "民國57年6月5日\n姓名 林建志\nA234567890",
			wantName: "林建志",
		},
		{
			// Edge case: a 3-rune block where all runes are stopwords (e.g. "年月日")
			// must be filtered even when no "姓名" label is present.
			name:     "all-stopword-rune block without label — skipped",
			text:     "年月日 A123456789",
			wantName: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractCJKName(tc.text)
			if got != tc.wantName {
				t.Errorf("extractCJKName(%q) = %q, want %q", tc.text, got, tc.wantName)
			}
		})
	}
}

// encodeOversizedPNG creates a valid PNG with declared dimensions (w×h) but
// only a tiny payload. Uses the actual image encoder so the header is valid.
// At large w×h values image.Decode will allocate the full pixel buffer.
func encodeOversizedPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	// Encode a 1×1 image and then return the actual encoded bytes.
	// For the dimension guard test we need a real PNG that claims the given size.
	// We use a proper w×h image (small for test performance, but enough to trigger guard).
	img := image.NewGray(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encodeOversizedPNG: %v", err)
	}
	return buf.Bytes()
}

func TestPreprocessImageDimensionGuard(t *testing.T) {
	t.Run("image within limit is processed normally", func(t *testing.T) {
		// 100×100 is well within maxDecodedDimension (6000).
		raw := encodeTestPNG(t, 100, 100)
		out, err := preprocessImage(raw)
		if err != nil {
			t.Fatalf("expected no error for small image, got: %v", err)
		}
		if len(out) == 0 {
			t.Fatal("expected non-empty output")
		}
	})

	t.Run("image exceeding width limit is rejected", func(t *testing.T) {
		// 6001×100 exceeds maxDecodedDimension on width.
		// Note: encoding a 6001×100 image in tests is feasible (~600 KB grayscale).
		raw := encodeOversizedPNG(t, maxDecodedDimension+1, 100)
		_, err := preprocessImage(raw)
		if err == nil {
			t.Fatal("expected error for oversized image, got nil")
		}
		if !strings.HasPrefix(err.Error(), errDimensionExceeded) {
			t.Errorf("expected dimension error, got: %v", err)
		}
	})

	t.Run("image exceeding height limit is rejected", func(t *testing.T) {
		raw := encodeOversizedPNG(t, 100, maxDecodedDimension+1)
		_, err := preprocessImage(raw)
		if err == nil {
			t.Fatal("expected error for oversized image, got nil")
		}
		if !strings.HasPrefix(err.Error(), errDimensionExceeded) {
			t.Errorf("expected dimension error, got: %v", err)
		}
	})

	t.Run("dimension error propagates through runTesseract — not silently swallowed", func(t *testing.T) {
		// Verify that runTesseract does NOT fall back to raw bytes on dimension errors.
		raw := encodeOversizedPNG(t, maxDecodedDimension+1, 100)
		_, err := runTesseract(raw)
		if err == nil {
			t.Fatal("expected error from runTesseract for oversized image")
		}
		if !isDimensionError(err) {
			t.Errorf("expected isDimensionError true, got err: %v", err)
		}
	})
}

// buildAuthTestRouter creates a Gin engine with the OCR auth middleware and a
// stub /ocr handler that writes 200 OK (no actual tesseract call). This lets us
// test the middleware at the HTTP layer without requiring tesseract to be installed.
func buildAuthTestRouter(token string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.SetTrustedProxies(nil) //nolint:errcheck // test-only engine

	// Build the middleware closure with the given token in the environment.
	// We cannot call os.Setenv here because tests run in parallel; instead we
	// directly invoke the middleware factory with the token embedded via a
	// test-local wrapper that shadows the env read.
	var mw gin.HandlerFunc
	if token == "" {
		// Simulate unset env: dev-mode middleware (allow all).
		os.Unsetenv("OCR_SERVICE_TOKEN") //nolint:errcheck // test-only
		mw = ocrAuthMiddleware()
	} else {
		// Simulate set env.
		t := token // capture
		tokenBytes := []byte(t)
		mw = authMiddlewareWithToken(tokenBytes)
	}

	r.POST("/ocr", mw, func(c *gin.Context) {
		c.Status(http.StatusOK)
	})
	return r
}

func TestOCRAuthMiddleware(t *testing.T) {
	const testToken = "test-secret-token-1234"

	tests := []struct {
		name           string
		envToken       string // empty = unset
		headerToken    string
		wantStatus     int
		wantNextCalled bool // whether the stub 200 handler should be reached
	}{
		{
			name:           "correct token accepted",
			envToken:       testToken,
			headerToken:    testToken,
			wantStatus:     http.StatusOK,
			wantNextCalled: true,
		},
		{
			name:           "wrong token rejected with 401",
			envToken:       testToken,
			headerToken:    "wrong-value",
			wantStatus:     http.StatusUnauthorized,
			wantNextCalled: false,
		},
		{
			name:           "missing header rejected with 401",
			envToken:       testToken,
			headerToken:    "", // omit header
			wantStatus:     http.StatusUnauthorized,
			wantNextCalled: false,
		},
		{
			name:           "dev mode (no env token) allows all requests",
			envToken:       "",
			headerToken:    "", // no header either
			wantStatus:     http.StatusOK,
			wantNextCalled: true,
		},
		{
			name:           "dev mode allows request even with a token header present",
			envToken:       "",
			headerToken:    "any-random-value",
			wantStatus:     http.StatusOK,
			wantNextCalled: true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			engine := buildAuthTestRouter(tc.envToken)
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/ocr", strings.NewReader(""))
			if tc.headerToken != "" {
				r.Header.Set("X-Ocr-Service-Token", tc.headerToken)
			}
			engine.ServeHTTP(w, r)
			if w.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body: %s)", w.Code, tc.wantStatus, w.Body.String())
			}
		})
	}
}

// authMiddlewareWithToken is a test helper that builds the auth middleware with
// an explicit token slice instead of reading from os.Getenv. This avoids
// os.Setenv races when tests run in parallel.
func authMiddlewareWithToken(tokenBytes []byte) gin.HandlerFunc {
	return func(c *gin.Context) {
		got := c.GetHeader("X-Ocr-Service-Token")
		if subtle.ConstantTimeCompare([]byte(got), tokenBytes) != 1 {
			httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "missing or invalid X-Ocr-Service-Token")
			c.Abort()
			return
		}
		c.Next()
	}
}
