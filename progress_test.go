package main

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPartTracker_EmptyTotalIsZero(t *testing.T) {
	pt := newPartTracker(5, MinPartSize)
	if got := pt.total(); got != 0 {
		t.Fatalf("empty total = %d, want 0", got)
	}
}

func TestPartTracker_Sum(t *testing.T) {
	pt := newPartTracker(3, MinPartSize)
	pt.parts[0].Store(100)
	pt.parts[1].Store(200)
	pt.parts[2].Store(300)
	if got := pt.total(); got != 600 {
		t.Fatalf("total = %d, want 600", got)
	}
}

func TestPartTracker_ConcurrentWrites(t *testing.T) {
	pt := newPartTracker(4, MinPartSize)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				pt.parts[idx].Add(1)
			}
		}(i)
	}
	wg.Wait()
	if got := pt.total(); got != 4000 {
		t.Fatalf("total = %d, want 4000", got)
	}
}

func TestHumanSize(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{1, "1 B"},
		{1023, "1023 B"},
		{1024, "1.00 KiB"},
		{1536, "1.50 KiB"},
		{1024 * 1024, "1.00 MiB"},
		{5 * 1024 * 1024, "5.00 MiB"},
		{1536 * 1024 * 1024, "1.50 GiB"},           // 1.5 GiB, clean integer
		{(2*1024 + 102) * 1024 * 1024, "2.10 GiB"}, // 2.0996... GiB, rounds to "2.10"
	}
	for _, tc := range cases {
		if got := humanSize(tc.in); got != tc.want {
			t.Errorf("humanSize(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPlainProgress_EmitsOnTenPercentIncrements(t *testing.T) {
	var buf bytes.Buffer
	pp := newPlainProgress(&buf, "video.mp4")
	pt := newPartTracker(10, MinPartSize)
	pp.Start(1000, pt)

	type step struct {
		fill int64
		want string // substring expected on the emitted line, or "" for none
	}
	steps := []step{
		{0, ""},      // 0% — no emission, starting line already printed
		{50, ""},     // 5% — no emission
		{100, "10%"}, // crosses 10%
		{150, ""},    // still 15%
		{200, "20%"},
		{500, "50%"},
		{999, ""},
		{1000, "100%"},
	}
	for _, s := range steps {
		// Only slot 0 matters; reset the rest to keep total clean.
		pt.parts[0].Store(s.fill)
		pp.renderOnce()
	}
	pp.Finish(true)

	out := buf.String()
	if !strings.Contains(out, "starting upload: video.mp4") {
		t.Errorf("missing starting line in:\n%s", out)
	}
	for _, s := range steps {
		if s.want == "" {
			continue
		}
		if !strings.Contains(out, s.want) {
			t.Errorf("expected %q in output, got:\n%s", s.want, out)
		}
	}
	if !strings.Contains(out, "done:") {
		t.Errorf("missing done line in:\n%s", out)
	}
}

func TestBarProgress_RenderFormat(t *testing.T) {
	// Use COLUMNS=120 so the bar has room for stats even with 4 parts.
	t.Setenv("COLUMNS", "120")

	var buf bytes.Buffer
	bp := newBarProgress(&buf, "video.mp4")
	total := int64(4 * MinPartSize)
	pt := newPartTracker(4, MinPartSize)
	bp.total = total
	bp.tracker = pt
	bp.startedAt = time.Now()

	// Mark first two parts as complete = 50%
	pt.parts[0].Store(MinPartSize)
	pt.parts[1].Store(MinPartSize)
	bp.renderOnce()

	out := buf.String()
	if !strings.Contains(out, "video.mp4") {
		t.Errorf("missing filename in:\n%s", out)
	}
	if !strings.Contains(out, "50%") {
		t.Errorf("missing percent in:\n%s", out)
	}
	if !strings.Contains(out, "\r") {
		t.Errorf("expected carriage return in:\n%s", out)
	}
	if !strings.Contains(out, "/") {
		t.Errorf("expected byte-count separator '/' in:\n%s", out)
	}
}

func TestBarProgress_FilenameTruncation(t *testing.T) {
	var buf bytes.Buffer
	long := "a-very-very-very-long-filename-for-testing-2025.mp4"
	bp := newBarProgress(&buf, long)
	pt := newPartTracker(1, MinPartSize)
	bp.total = 1
	bp.tracker = pt
	bp.startedAt = time.Now()

	bp.renderOnce()
	out := buf.String()
	if strings.Contains(out, long) {
		t.Errorf("expected name to be truncated, got full: %q", out)
	}
	if !strings.Contains(out, "…") {
		t.Errorf("expected ellipsis in truncated name, got: %q", out)
	}
}
