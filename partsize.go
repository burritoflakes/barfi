package main

import (
	"fmt"
	"strconv"
	"strings"
)

// BUS protocol constants, mirrored from the server's bus/protocol.go.
// These are part of the wire protocol — changing them here without a
// coordinated server change would break uploads.
const (
	MinPartSize = 5 * 1024 * 1024           // 5 MB
	MaxPartSize = 100 * 1024 * 1024         // 100 MB
	MaxParts    = 10000                     // hard cap on parts per upload
	MaxFileSize = 1024 * 1024 * 1024 * 1024 // 1 TB
)

// BUS protocol header names.
const (
	headerUploadLength     = "Upload-Length"
	headerUploadPartNumber = "Upload-Part-Number"
)

// maxRetries bounds the per-part retry loop.
const maxRetries = 5

// calcPartSize returns the BUS part size to use for a given file size.
// Defaults to MaxPartSize (100 MB) — matching the JS client — to minimize
// HTTP request overhead. Only uses a smaller size when the file itself is
// smaller than 100 MB, clamped down to MinPartSize (5 MB).
func calcPartSize(fileSize int64) int64 {
	if fileSize <= MaxPartSize {
		// File fits in one part — use the file size itself, clamped to min.
		if fileSize < MinPartSize {
			return MinPartSize
		}
		return fileSize
	}
	return MaxPartSize
}

// parseSize converts a human-readable size string to bytes.
// Accepts bare integers (bytes), decimal suffixes (KB/MB/GB), and
// binary suffixes (KiB/MiB/GiB). Case-insensitive. No floating point.
func parseSize(s string) (int64, error) {
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	// Find the first non-digit to split number from suffix.
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("size %q must start with digits", s)
	}
	n, err := strconv.ParseInt(s[:i], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("size %q: %w", s, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("size %q: negative not allowed", s)
	}
	suffix := strings.ToUpper(strings.TrimSpace(s[i:]))
	var mult int64
	switch suffix {
	case "", "B":
		mult = 1
	case "KB":
		mult = 1000
	case "KIB":
		mult = 1024
	case "MB":
		mult = 1000 * 1000
	case "MIB":
		mult = 1024 * 1024
	case "GB":
		mult = 1000 * 1000 * 1000
	case "GIB":
		mult = 1024 * 1024 * 1024
	default:
		return 0, fmt.Errorf("size %q: unknown suffix %q", s, suffix)
	}
	return n * mult, nil
}
