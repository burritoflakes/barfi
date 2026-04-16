package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"sync/atomic"
	"time"
)

// partTracker holds one atomic byte-counter per upload part.
// Workers bump their own slot; the progress UI sums the slice at its
// own cadence. On retry, a worker can reset its slot to 0 via Store(0)
// before reopening the part — this makes retried bytes roll back cleanly.
type partTracker struct {
	parts    []atomic.Int64
	partSize int64 // the actual part size used by the uploader (not total/n)
}

func newPartTracker(n int, partSize int64) *partTracker {
	return &partTracker{parts: make([]atomic.Int64, n), partSize: partSize}
}

// total returns the sum of all slots. Safe to call concurrently with writes.
func (pt *partTracker) total() int64 {
	var sum int64
	for i := range pt.parts {
		sum += pt.parts[i].Load()
	}
	return sum
}

// humanSize formats a byte count using binary (1024-based) units.
// Values under 1 KiB use no decimal places; larger values use two decimals.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	suffix := [...]string{"KiB", "MiB", "GiB", "TiB", "PiB"}[exp]
	return fmt.Sprintf("%.2f %s", float64(n)/float64(div), suffix)
}

// Progress is the interface the main package uses to drive upload feedback.
// Implementations: noopProgress (--quiet mode), plainProgress (non-TTY), barProgress (TTY).
type Progress interface {
	// Start begins rendering. total is the full file size in bytes.
	// tracker is polled by the implementation on its own ticker.
	Start(total int64, tracker *partTracker)
	// Finish stops rendering. success reports whether the upload completed OK.
	Finish(success bool)
}

// noopProgress does nothing. Used when --quiet is set.
type noopProgress struct{}

func (noopProgress) Start(int64, *partTracker) {}
func (noopProgress) Finish(bool)               {}

// plainProgress writes one line per 10% increment. Used when stderr is
// not a TTY (CI logs, pipes, ssh with a dumb term). Line-buffered output.
type plainProgress struct {
	w           io.Writer
	name        string
	total       int64
	tracker     *partTracker
	startedAt   time.Time
	lastPercent int
	done        chan struct{}
	doneAck     chan struct{}
}

func newPlainProgress(w io.Writer, name string) *plainProgress {
	return &plainProgress{
		w:           w,
		name:        name,
		lastPercent: -1,
		done:        make(chan struct{}),
		doneAck:     make(chan struct{}),
	}
}

func (p *plainProgress) Start(total int64, tracker *partTracker) {
	p.total = total
	p.tracker = tracker
	p.startedAt = time.Now()
	_, _ = fmt.Fprintf(p.w, "starting upload: %s (%s)\n", p.name, humanSize(total))
	go p.loop()
}

func (p *plainProgress) loop() {
	defer close(p.doneAck)
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			p.renderOnce()
		case <-p.done:
			return
		}
	}
}

// renderOnce is split out so tests can drive progress deterministically.
func (p *plainProgress) renderOnce() {
	if p.total <= 0 {
		return
	}
	uploaded := p.tracker.total()
	percent := int(uploaded * 100 / p.total)
	if percent > 100 {
		percent = 100
	}
	bucket := percent / 10
	if p.lastPercent >= 0 && p.lastPercent/10 == bucket {
		return
	}
	if p.lastPercent < 0 && bucket == 0 {
		// First call at 0% — don't emit; starting line already printed.
		return
	}
	p.lastPercent = percent
	elapsed := time.Since(p.startedAt).Seconds()
	var speed float64
	if elapsed > 0 {
		speed = float64(uploaded) / elapsed
	}
	_, _ = fmt.Fprintf(p.w, "progress: %3d%% %s / %s  %s/s\n",
		percent, humanSize(uploaded), humanSize(p.total), humanSize(int64(speed)))
}

func (p *plainProgress) Finish(success bool) {
	close(p.done)
	<-p.doneAck
	if success {
		elapsed := time.Since(p.startedAt).Round(time.Second)
		_, _ = fmt.Fprintf(p.w, "done: %s uploaded in %s\n", p.name, elapsed)
	}
}

// barProgress renders a single-line progress bar on a TTY writer using
// carriage-return refresh. Not safe for non-TTY writers — use plainProgress.
// speedSample is a (time, bytes) snapshot for computing current speed.
type speedSample struct {
	t     time.Time
	bytes int64
}

// speedWindow is the duration over which current speed is computed.
const speedWindow = 2 * time.Second

// speedRingSize is how many samples we keep. At 10 Hz render, 20 samples = 2s.
const speedRingSize = 20

type barProgress struct {
	w               io.Writer
	name            string
	total           int64
	tracker         *partTracker
	startedAt       time.Time
	done            chan struct{}
	doneAck         chan struct{}
	lastRenderWidth int // terminal width at the time of the last render

	speedRing [speedRingSize]speedSample
	ringIdx   int
	ringCount int
}

func newBarProgress(w io.Writer, name string) *barProgress {
	return &barProgress{
		w:       w,
		name:    name,
		done:    make(chan struct{}),
		doneAck: make(chan struct{}),
	}
}

// currentSpeed returns the upload speed in bytes/sec computed over the
// most recent ~2 seconds of samples. Falls back to 0 if not enough data.
func (p *barProgress) currentSpeed(now time.Time, uploaded int64) float64 {
	// Record this sample.
	p.speedRing[p.ringIdx] = speedSample{t: now, bytes: uploaded}
	p.ringIdx = (p.ringIdx + 1) % speedRingSize
	if p.ringCount < speedRingSize {
		p.ringCount++
	}

	// Find the oldest sample within the window.
	oldest := p.speedRing[p.ringIdx%speedRingSize]
	for i := 0; i < p.ringCount; i++ {
		idx := (p.ringIdx - p.ringCount + i + speedRingSize) % speedRingSize
		s := p.speedRing[idx]
		if now.Sub(s.t) <= speedWindow {
			oldest = s
			break
		}
	}

	dt := now.Sub(oldest.t).Seconds()
	if dt < 0.1 {
		return 0 // not enough data yet
	}
	db := uploaded - oldest.bytes
	if db < 0 {
		db = 0
	}
	return float64(db) / dt
}

func termWidth() int {
	// Try ioctl first — gets the real terminal width without $COLUMNS.
	if w := termWidthFd(2); w > 0 { // fd 2 = stderr
		return w
	}
	if s := os.Getenv("COLUMNS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 20 {
			return n
		}
	}
	return 80
}

func (p *barProgress) Start(total int64, tracker *partTracker) {
	p.total = total
	p.tracker = tracker
	p.startedAt = time.Now()
	go p.loop()
}

func (p *barProgress) loop() {
	defer close(p.doneAck)
	t := time.NewTicker(100 * time.Millisecond) // 10 Hz
	defer t.Stop()
	for {
		select {
		case <-t.C:
			p.renderOnce()
		case <-p.done:
			p.renderOnce()
			_, _ = fmt.Fprintln(p.w)
			return
		}
	}
}

// truncateName shortens s to at most max runes using a middle ellipsis.
func truncateName(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	keepLeft := (max - 1) / 2
	keepRight := max - 1 - keepLeft
	return string(runes[:keepLeft]) + "…" + string(runes[len(runes)-keepRight:])
}

func (p *barProgress) renderOnce() {
	if p.total <= 0 {
		return
	}
	width := termWidth()
	totalParts := len(p.tracker.parts)
	uploaded := p.tracker.total()
	if uploaded > p.total {
		uploaded = p.total
	}
	percent := int(uploaded * 100 / p.total)

	now := time.Now()
	speed := p.currentSpeed(now, uploaded)

	name := truncateName(p.name, 15)

	// Build the stats suffix first so we know how much space the bar gets.
	stats := fmt.Sprintf("  %3d%%  %s / %s  %s/s",
		percent, humanSize(uploaded), humanSize(p.total), humanSize(int64(speed)))
	elapsed := now.Sub(p.startedAt).Seconds()
	if elapsed >= 1 && uploaded < p.total && speed > 0 {
		remaining := float64(p.total-uploaded) / speed
		stats += "  ETA " + formatETA(remaining)
	}

	// Layout: <name>  [<bar>]<stats>
	// The brackets [ ] consume 2 chars from the available space.
	barWidth := width - len([]rune(name)) - 2 - 2 - len([]rune(stats))
	if barWidth < 1 {
		barWidth = 0 // terminal too narrow for a bar — show stats only
	}

	partSize := p.tracker.partSize

	// Compute each part's expected byte size (all equal except the last).
	partExpected := func(i int) int64 {
		if i == totalParts-1 {
			rem := p.total - partSize*int64(i)
			if rem > 0 {
				return rem
			}
			return partSize
		}
		return partSize
	}

	// Build the bar string. May be empty if terminal is too narrow.
	var barStr string
	if barWidth > 0 {
		bar := make([]rune, 0, barWidth)

		if barWidth >= totalParts {
			// Allocate segment widths proportional to each part's byte size.
			allocated := 0
			for i := 0; i < totalParts; i++ {
				var segWidth int
				if i == totalParts-1 {
					segWidth = barWidth - allocated
				} else {
					segWidth = int(int64(barWidth) * partExpected(i) / p.total)
					if segWidth < 1 {
						segWidth = 1
					}
				}
				allocated += segWidth

				val := p.tracker.parts[i].Load()
				expected := partExpected(i)

				var filled int
				if val >= expected {
					filled = segWidth
				} else if val > 0 {
					filled = int(int64(segWidth) * val / expected)
					if filled == 0 {
						filled = 1
					}
				}

				for j := 0; j < filled; j++ {
					bar = append(bar, '#')
				}
				for j := filled; j < segWidth; j++ {
					bar = append(bar, '.')
				}
			}
		} else {
			// More parts than cells — each cell maps to a byte range.
			for i := 0; i < barWidth; i++ {
				byteStart := p.total * int64(i) / int64(barWidth)
				byteEnd := p.total * int64(i+1) / int64(barWidth)

				var upl, exp int64
				for j := 0; j < totalParts; j++ {
					pStart := partSize * int64(j)
					pEnd := pStart + partExpected(j)
					lo := max64(byteStart, pStart)
					hi := min64(byteEnd, pEnd)
					if lo >= hi {
						continue
					}
					chunk := hi - lo
					exp += chunk
					val := p.tracker.parts[j].Load()
					pUpl := val
					if pUpl > partExpected(j) {
						pUpl = partExpected(j)
					}
					partFrac := float64(pUpl) / float64(partExpected(j))
					upl += int64(float64(chunk) * partFrac)
				}

				if exp == 0 {
					bar = append(bar, '.')
				} else if upl >= exp {
					bar = append(bar, '#')
				} else if upl > 0 {
					bar = append(bar, ':')
				} else {
					bar = append(bar, '.')
				}
			}
		}
		barStr = string(bar)
	}

	// Assemble the line.
	var line string
	if barStr != "" {
		line = name + "  [" + barStr + "]" + stats
	} else {
		line = name + stats
	}

	// Hard-truncate to terminal width so the line never wraps.
	runes := []rune(line)
	if len(runes) > width {
		runes = runes[:width]
		line = string(runes)
	}

	// If the terminal shrank since the last render, the previous frame
	// now wraps across multiple lines. \r only returns to the start of
	// the bottom-most wrapped line, leaving stale content above. Move
	// the cursor up to the first wrapped line and clear everything below.
	if p.lastRenderWidth > width && width > 0 {
		prevLines := (p.lastRenderWidth + width - 1) / width
		if prevLines > 1 {
			_, _ = fmt.Fprintf(p.w, "\x1b[%dA", prevLines-1) // move up
		}
	}
	// \r to column 0, \x1b[J clears from cursor to end of screen,
	// then we write the new content (guaranteed ≤ width chars).
	_, _ = fmt.Fprintf(p.w, "\r\x1b[J%s", line)
	p.lastRenderWidth = width
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func formatETA(secs float64) string {
	s := int(secs + 0.5)
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	if s < 3600 {
		return fmt.Sprintf("%dm %ds", s/60, s%60)
	}
	return fmt.Sprintf("%dh %dm", s/3600, (s%3600)/60)
}

func (p *barProgress) Finish(success bool) {
	close(p.done)
	<-p.doneAck
}

// isTerminal reports whether f is attached to a character device (TTY).
// Pure stdlib — works on Linux, macOS, and Windows via os.ModeCharDevice.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// newProgress picks a Progress implementation based on the quiet flag and
// stderr's TTY status. Interactive UIs write to stderr.
func newProgress(quiet bool, name string) Progress {
	if quiet {
		return noopProgress{}
	}
	if isTerminal(os.Stderr) {
		return newBarProgress(os.Stderr, name)
	}
	return newPlainProgress(os.Stderr, name)
}
