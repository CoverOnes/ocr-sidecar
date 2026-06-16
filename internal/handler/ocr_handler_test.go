package handler

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

// forgedHeaderPNG builds a tiny but structurally-valid PNG consisting of only
// the 8-byte signature and a single IHDR chunk that DECLARES the given
// dimensions. The IHDR CRC is computed correctly so image.DecodeConfig parses
// it successfully. There is NO IDAT pixel data, so the payload stays tiny —
// this is exactly the "claim 10000×10000 in <100 KB" attack the pre-decode
// peek must reject before any pixel buffer is allocated.
func forgedHeaderPNG(t *testing.T, w, h uint32) []byte {
	t.Helper()

	var buf bytes.Buffer
	// PNG signature.
	buf.Write([]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A})

	// IHDR data: width(4) height(4) bitDepth(1) colorType(1) compression(1)
	// filter(1) interlace(1) = 13 bytes.
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], w)
	binary.BigEndian.PutUint32(ihdr[4:8], h)
	ihdr[8] = 8 // bit depth
	ihdr[9] = 0 // color type: grayscale
	ihdr[10] = 0
	ihdr[11] = 0
	ihdr[12] = 0

	// Length (of data only).
	length := make([]byte, 4)
	binary.BigEndian.PutUint32(length, uint32(len(ihdr)))
	buf.Write(length)

	// Type + data, then CRC over (type + data).
	chunk := append([]byte("IHDR"), ihdr...)
	buf.Write(chunk)
	crc := make([]byte, 4)
	binary.BigEndian.PutUint32(crc, crc32.ChecksumIEEE(chunk))
	buf.Write(crc)

	return buf.Bytes()
}

func TestPreprocessImagePreDecodePeek(t *testing.T) {
	t.Run("forged oversized header rejected pre-decode without allocating pixels", func(t *testing.T) {
		// Header claims 20000×20000 (~1.6 GB if decoded as 32-bit RGBA) but the
		// payload is only a few dozen bytes — no IDAT. If the guard ran only
		// AFTER image.Decode, this would either OOM or fail to decode; the
		// pre-decode peek must reject it cleanly with the dimension error.
		raw := forgedHeaderPNG(t, 20000, 20000)
		if len(raw) > 1024 {
			t.Fatalf("forged payload unexpectedly large (%d bytes); peek test premise broken", len(raw))
		}
		_, err := preprocessImage(raw)
		if err == nil {
			t.Fatal("expected dimension error for forged oversized header, got nil")
		}
		if !strings.HasPrefix(err.Error(), errDimensionExceeded) {
			t.Errorf("expected dimension error, got: %v", err)
		}
		// The reported dimensions must come from the forged header (20000), proving
		// rejection happened at the header-peek stage, not from a decoded buffer.
		if !strings.Contains(err.Error(), "20000x20000") {
			t.Errorf("error should report forged header dims 20000x20000, got: %v", err)
		}
	})

	t.Run("forged header within limit falls through to decode (no IDAT -> decode fails -> raw passthrough)", func(t *testing.T) {
		// A header-only PNG within the dimension limit passes the peek, then
		// image.Decode fails (no pixel data) and the function returns the raw
		// bytes unchanged — confirming the peek does not reject legitimate sizes.
		raw := forgedHeaderPNG(t, 100, 100)
		out, err := preprocessImage(raw)
		if err != nil {
			t.Fatalf("expected no dimension error for in-limit header, got: %v", err)
		}
		if string(out) != string(raw) {
			t.Errorf("expected raw passthrough when decode fails after a valid in-limit header")
		}
	})
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

// buildAuthTestRouter creates a Gin engine wired with the PRODUCTION
// ocrAuthMiddleware() and a stub /ocr handler that writes 200 OK (no actual
// tesseract call). This lets us test the real middleware at the HTTP layer
// without requiring tesseract to be installed.
//
// ocrAuthMiddleware() reads OCR_SERVICE_TOKEN from the environment at
// construction time, so the env MUST be set via t.Setenv BEFORE this function
// builds the engine. t.Setenv auto-restores the prior value at test cleanup and
// fails fast if the test is parallel — these tests are intentionally serial.
func buildAuthTestRouter(t *testing.T, token string) *gin.Engine {
	t.Helper()

	// Drive the real middleware: set (or clear) the env the production factory reads.
	if token == "" {
		// Empty value exercises the dev-mode (allow-all) branch of ocrAuthMiddleware.
		t.Setenv("OCR_SERVICE_TOKEN", "")
	} else {
		t.Setenv("OCR_SERVICE_TOKEN", token)
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.SetTrustedProxies(nil) //nolint:errcheck // test-only engine

	r.POST("/ocr", ocrAuthMiddleware(), func(c *gin.Context) {
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
		t.Run(tc.name, func(t *testing.T) {
			engine := buildAuthTestRouter(t, tc.envToken)
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
