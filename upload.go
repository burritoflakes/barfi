package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Typed sentinel errors.
var (
	errExpired      = errors.New("upload session expired")
	errPartTooLarge = errors.New("part exceeds server size limit")
)

// serverError wraps a non-retryable 4xx response for main.go's error formatter.
type serverError struct {
	status  int
	message string
}

func (e *serverError) Error() string {
	return fmt.Sprintf("server returned %d: %s", e.status, e.message)
}

// Uploader performs a single BUS upload from a local file. It is configured
// by main.go and its run method is the whole public API.
type Uploader struct {
	// Inputs (set by main before calling run).
	file        *os.File
	fileSize    int64
	fileName    string
	server      string // base URL, no trailing slash
	token       string
	locationId  string
	parentId    string
	guestLinkId string
	note        string // raw, run() base64-encodes before sending
	partSize    int64
	workers     int

	httpClient *http.Client
	progress   Progress

	// Runtime state, set during run().
	uploadId  string
	location  string
	expiresAt time.Time
	tracker   *partTracker
}

// sleepBackoff is indirected through a variable so tests can stub it to
// avoid real 1/2/4/8/16 second waits. The default blocks on ctx.Done().
var sleepBackoff = func(ctx context.Context, attempt int) error {
	d := time.Duration(1<<uint(attempt-1)) * time.Second
	if d > 16*time.Second {
		d = 16 * time.Second
	}
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// validate checks the Uploader's inputs before any HTTP work begins.
// Runs once at the top of run().
func (u *Uploader) validate() error {
	if u.file == nil {
		return fmt.Errorf("no file")
	}
	if u.fileSize <= 0 {
		return fmt.Errorf("file is empty")
	}
	if u.fileSize > MaxFileSize {
		return fmt.Errorf("file size %d exceeds protocol limit %d (1 TB)", u.fileSize, MaxFileSize)
	}
	if u.server == "" {
		return fmt.Errorf("server URL not set")
	}
	if u.parentId != "" && u.guestLinkId != "" {
		return fmt.Errorf("--parent-id and --guest-upload-link-id are mutually exclusive")
	}
	if u.parentId != "" && u.token == "" {
		return fmt.Errorf("--parent-id requires --token")
	}
	if u.workers < 1 {
		return fmt.Errorf("workers must be >= 1")
	}
	if u.partSize < MinPartSize || u.partSize > MaxPartSize {
		return fmt.Errorf("part size %d outside [%d, %d]", u.partSize, MinPartSize, MaxPartSize)
	}
	totalParts := (u.fileSize + u.partSize - 1) / u.partSize
	if totalParts > MaxParts {
		return fmt.Errorf("file would require %d parts, exceeds protocol limit %d", totalParts, MaxParts)
	}
	return nil
}

type initResponse struct {
	Code int `json:"code"`
	Data struct {
		Id        string `json:"id"`
		Location  string `json:"location"`
		ExpiresAt string `json:"expiresAt"`
	} `json:"data"`
}

func (u *Uploader) init(ctx context.Context) error {
	body := map[string]any{"name": u.fileName}
	if u.locationId != "" {
		body["locationId"] = u.locationId
	}
	if u.guestLinkId != "" {
		body["guestUploadLinkId"] = u.guestLinkId
	} else if u.parentId != "" {
		body["parentId"] = u.parentId
	}
	if u.note != "" {
		body["note"] = base64.StdEncoding.EncodeToString([]byte(u.note))
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal init body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.server+"/api/upload", bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if u.token != "" && u.guestLinkId == "" {
		req.Header.Set("Authorization", "Bearer "+u.token)
	}

	resp, err := u.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("init: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		msg := readServerMessage(resp)
		return &serverError{status: resp.StatusCode, message: msg}
	}

	var parsed initResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return fmt.Errorf("init: decode response: %w", err)
	}
	u.uploadId = parsed.Data.Id
	u.location = parsed.Data.Location
	if t, err := time.Parse(time.RFC3339, parsed.Data.ExpiresAt); err == nil {
		u.expiresAt = t
	}
	return nil
}

// readServerMessage pulls a user-facing message out of an error response body.
// Server returns {"message":"..."} or {"error":"..."}; falls back to raw body.
func readServerMessage(resp *http.Response) string {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var env struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(data, &env); err == nil {
		if env.Message != "" {
			return env.Message
		}
		if env.Error != "" {
			return env.Error
		}
	}
	return strings.TrimSpace(string(data))
}

// progressReader wraps an io.Reader and atomically bumps the given slot
// on every successful Read. On retry, the worker resets the slot to 0
// before constructing a fresh progressReader — that's the rollback.
//
// The done flag lets the caller detach the reader from its slot after a
// request attempt returns. The Go HTTP transport may drain the request
// body in a background goroutine after we see the response, and without
// a detach hook those late reads would leak into the next attempt's
// counter (the Store(0) at the top of the retry loop runs before the
// stale drain completes).
type progressReader struct {
	r    io.Reader
	slot *atomic.Int64
	done atomic.Bool
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.r.Read(p)
	if n > 0 && !pr.done.Load() {
		pr.slot.Add(int64(n))
	}
	return n, err
}

func (u *Uploader) uploadPart(ctx context.Context, partNum int) error {
	idx := partNum - 1
	offset := int64(idx) * u.partSize
	length := u.partSize
	if offset+length > u.fileSize {
		length = u.fileSize - offset
	}

	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		// Rollback any progress counted on a previous attempt of this part.
		u.tracker.parts[idx].Store(0)

		section := io.NewSectionReader(u.file, offset, length)
		body := &progressReader{r: section, slot: &u.tracker.parts[idx]}

		req, err := http.NewRequestWithContext(ctx, http.MethodPatch, u.location, body)
		if err != nil {
			return err
		}
		req.ContentLength = length
		req.Header.Set(headerUploadLength, strconv.FormatInt(length, 10))
		req.Header.Set(headerUploadPartNumber, strconv.Itoa(partNum))
		if u.token != "" && u.guestLinkId == "" {
			req.Header.Set("Authorization", "Bearer "+u.token)
		}

		resp, err := u.httpClient.Do(req)
		if err != nil {
			body.done.Store(true)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lastErr = fmt.Errorf("part %d: attempt %d/%d: %w", partNum, attempt, maxRetries, err)
			if berr := sleepBackoff(ctx, attempt); berr != nil {
				return berr
			}
			continue
		}

		status := resp.StatusCode
		msg := ""
		if status >= 400 {
			msg = readServerMessage(resp)
		}
		resp.Body.Close()
		body.done.Store(true)

		switch {
		case status == 200 || status == 204:
			return nil
		case status == http.StatusGone:
			return errExpired
		case status == http.StatusRequestEntityTooLarge:
			return errPartTooLarge
		case status >= 500:
			lastErr = fmt.Errorf("part %d: attempt %d/%d: server %d: %s", partNum, attempt, maxRetries, status, msg)
			if berr := sleepBackoff(ctx, attempt); berr != nil {
				return berr
			}
			continue
		default:
			return &serverError{status: status, message: msg}
		}
	}
	return lastErr
}

func (u *Uploader) uploadAllParts(ctx context.Context, totalParts int) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan int, totalParts)
	for i := 1; i <= totalParts; i++ {
		jobs <- i
	}
	close(jobs)

	errs := make(chan error, u.workers)
	var wg sync.WaitGroup

	workers := u.workers
	if workers > totalParts {
		workers = totalParts
	}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for partNum := range jobs {
				if ctx.Err() != nil {
					return
				}
				if err := u.uploadPart(ctx, partNum); err != nil {
					select {
					case errs <- err:
					default:
					}
					cancel()
					return
				}
			}
		}()
	}

	wg.Wait()
	close(errs)
	if err := <-errs; err != nil {
		return err
	}
	return nil
}

type completeResponse struct {
	Code int `json:"code"`
	Data struct {
		Id        string `json:"id"`
		Name      string `json:"name"`
		Size      int64  `json:"size"`
		CreatedAt string `json:"createdAt"`
	} `json:"data"`
}

// completeResult holds both the share link and the raw JSON body from the
// server's complete response — the latter is printed when --json is set.
type completeResult struct {
	link    string
	rawJSON []byte
}

func (u *Uploader) complete(ctx context.Context) (completeResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.server+"/api/upload/"+u.uploadId, nil)
	if err != nil {
		return completeResult{}, err
	}
	if u.token != "" && u.guestLinkId == "" {
		req.Header.Set("Authorization", "Bearer "+u.token)
	}
	resp, err := u.httpClient.Do(req)
	if err != nil {
		return completeResult{}, fmt.Errorf("complete: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusGone {
		return completeResult{}, errExpired
	}
	if resp.StatusCode >= 400 {
		return completeResult{}, &serverError{status: resp.StatusCode, message: readServerMessage(resp)}
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return completeResult{}, fmt.Errorf("complete: read body: %w", err)
	}
	var parsed completeResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return completeResult{}, fmt.Errorf("complete: decode: %w", err)
	}
	return completeResult{
		link:    u.server + "/" + parsed.Data.Id,
		rawJSON: raw,
	}, nil
}

func (u *Uploader) run(ctx context.Context) (completeResult, error) {
	if u.partSize == 0 {
		u.partSize = calcPartSize(u.fileSize)
	}
	if err := u.validate(); err != nil {
		return completeResult{}, err
	}

	totalParts := int((u.fileSize + u.partSize - 1) / u.partSize)
	u.tracker = newPartTracker(totalParts, u.partSize)

	if u.httpClient == nil {
		u.httpClient = &http.Client{Timeout: 0}
	}
	if u.progress == nil {
		u.progress = noopProgress{}
	}

	if err := u.init(ctx); err != nil {
		return completeResult{}, err
	}

	u.progress.Start(u.fileSize, u.tracker)

	if err := u.uploadAllParts(ctx, totalParts); err != nil {
		u.progress.Finish(false)
		return completeResult{}, err
	}

	result, err := u.complete(ctx)
	u.progress.Finish(err == nil)
	if err != nil {
		return completeResult{}, err
	}
	return result, nil
}
