package handler

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
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
