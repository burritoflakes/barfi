package main

import "testing"

func TestCalcPartSize(t *testing.T) {
	cases := []struct {
		name     string
		fileSize int64
		want     int64
	}{
		{"1 MiB → clamped to MinPartSize", 1 * 1024 * 1024, MinPartSize},
		{"exactly MinPartSize → MinPartSize", MinPartSize, MinPartSize},
		{"10 MiB → file size (fits in one part)", 10 * 1024 * 1024, 10 * 1024 * 1024},
		{"50 MiB → file size (fits in one part)", 50 * 1024 * 1024, 50 * 1024 * 1024},
		{"exactly MaxPartSize → MaxPartSize", MaxPartSize, MaxPartSize},
		{"200 MiB → MaxPartSize (default 100 MB)", 200 * 1024 * 1024, MaxPartSize},
		{"1 GiB → MaxPartSize", 1024 * 1024 * 1024, MaxPartSize},
		{"MaxFileSize (1 TiB) → MaxPartSize", MaxFileSize, MaxPartSize},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := calcPartSize(tc.fileSize)
			if got != tc.want {
				t.Fatalf("calcPartSize(%d) = %d, want %d", tc.fileSize, got, tc.want)
			}
		})
	}
}

func TestParseSize(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"100", 100, false},
		{"100B", 100, false},
		{"100b", 100, false},
		{"5MB", 5_000_000, false},
		{"5mb", 5_000_000, false},
		{"5MiB", 5 * 1024 * 1024, false},
		{"25MiB", 25 * 1024 * 1024, false},
		{"1GB", 1_000_000_000, false},
		{"1GiB", 1 * 1024 * 1024 * 1024, false},
		{"1KB", 1000, false},
		{"1KiB", 1024, false},
		{"0.5GB", 0, true},
		{"-5MB", 0, true},
		{"garbage", 0, true},
		{"", 0, true},
		{"MB", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseSize(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseSize(%q) = %d, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSize(%q) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("parseSize(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}
