package handler

import "testing"

func TestExtractFields(t *testing.T) {
	tests := []struct {
		name       string
		text       string
		wantName   string
		wantID     string
		wantConf   float64
	}{
		{
			name:     "both name and id found",
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
			name:     "name only",
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
