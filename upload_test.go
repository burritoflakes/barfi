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
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestUploader(t *testing.T, contents []byte) *Uploader {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "barfi-test-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(contents); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	return &Uploader{
		file:     f,
		fileSize: int64(len(contents)),
		fileName: "test.bin",
		server:   "https://example.com",
		partSize: MinPartSize,
		workers:  2,
		progress: noopProgress{},
	}
}

func TestValidate_OK(t *testing.T) {
	u := newTestUploader(t, []byte("hello"))
	if err := u.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestValidate_ParentRequiresToken(t *testing.T) {
	u := newTestUploader(t, []byte("hello"))
	u.parentId = "abc"
	err := u.validate()
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("want token-required error, got %v", err)
	}
}

func TestValidate_ParentAndGuestMutuallyExclusive(t *testing.T) {
	u := newTestUploader(t, []byte("hello"))
	u.parentId = "abc"
	u.token = "tok"
	u.guestLinkId = "guest"
	err := u.validate()
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("want mutually-exclusive error, got %v", err)
	}
}

func TestValidate_FileSizeCap(t *testing.T) {
	u := newTestUploader(t, []byte("hello"))
	u.fileSize = MaxFileSize + 1
	err := u.validate()
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("want exceeds error, got %v", err)
	}
}

func TestValidate_PartCountCap(t *testing.T) {
	u := newTestUploader(t, []byte("hello"))
	u.fileSize = int64(MinPartSize) * (MaxParts + 1)
	u.partSize = MinPartSize
	err := u.validate()
	if err == nil || !strings.Contains(err.Error(), "parts") {
		t.Fatalf("want too-many-parts error, got %v", err)
	}
}

// fakeBus is a minimal in-memory BUS protocol server for tests.
// Subsequent upload tasks extend it with PATCH and complete behavior.
type fakeBus struct {
	t *testing.T

	mu            sync.Mutex
	initBody      map[string]any
	initCalls     int
	patchParts    map[int][]byte // partNum -> bytes received
	patchAttempts map[int]int    // partNum -> attempt count
	completeCalls int

	// Per-test knobs.
	patchStatus  func(partNum, attempt int) (status int, body string)
	completeResp string
	initToken    string // the "encrypted" upload id to return
}

func newFakeBus(t *testing.T) (*fakeBus, *httptest.Server) {
	fb := &fakeBus{
		t:             t,
		patchParts:    map[int][]byte{},
		patchAttempts: map[int]int{},
		initToken:     "tok-abc",
	}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/api/upload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		fb.mu.Lock()
		defer fb.mu.Unlock()
		fb.initCalls++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		fb.initBody = body
		resp := map[string]any{
			"code": 200,
			"data": map[string]any{
				"id":        fb.initToken,
				"location":  srv.URL + "/u/" + fb.initToken,
				"expiresAt": "2099-01-01T00:00:00Z",
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/api/upload/"+fb.initToken, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		fb.mu.Lock()
		defer fb.mu.Unlock()
		fb.completeCalls++
		body := fb.completeResp
		if body == "" {
			body = `{"code":201,"data":{"id":"final-id","name":"test.bin","size":5,"createdAt":"2099-01-01T00:00:00Z"}}`
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(body))
	})

	mux.HandleFunc("/u/"+fb.initToken, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		partNum := 0
		fmt.Sscanf(r.Header.Get(headerUploadPartNumber), "%d", &partNum)

		fb.mu.Lock()
		fb.patchAttempts[partNum]++
		attempt := fb.patchAttempts[partNum]
		fn := fb.patchStatus
		fb.mu.Unlock()

		status := 204
		var rbody string
		if fn != nil {
			status, rbody = fn(partNum, attempt)
		}
		if status == 204 {
			data, _ := io.ReadAll(r.Body)
			fb.mu.Lock()
			fb.patchParts[partNum] = data
			fb.mu.Unlock()
			w.WriteHeader(204)
			return
		}
		w.WriteHeader(status)
		if rbody != "" {
			_, _ = w.Write([]byte(rbody))
		}
	})

	return fb, srv
}

func TestInit_SendsExpectedBody(t *testing.T) {
	fb, srv := newFakeBus(t)
	u := newTestUploader(t, []byte("hello"))
	u.server = srv.URL
	u.locationId = "loc1"
	u.note = "hello world"
	u.httpClient = srv.Client()

	if err := u.init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	if fb.initCalls != 1 {
		t.Fatalf("initCalls = %d, want 1", fb.initCalls)
	}
	if got := fb.initBody["name"]; got != "test.bin" {
		t.Errorf("name = %v, want test.bin", got)
	}
	if got := fb.initBody["locationId"]; got != "loc1" {
		t.Errorf("locationId = %v, want loc1", got)
	}
	wantNote := base64.StdEncoding.EncodeToString([]byte("hello world"))
	if got, _ := fb.initBody["note"].(string); got != wantNote {
		t.Errorf("note not base64-encoded: got %q want %q", got, wantNote)
	}
	if u.uploadId != fb.initToken {
		t.Errorf("u.uploadId = %q, want %q", u.uploadId, fb.initToken)
	}
	if u.location == "" {
		t.Errorf("u.location not set")
	}
}

func TestInit_AuthHeaderWhenTokenSet(t *testing.T) {
	var gotAuth string
	fb, srv := newFakeBus(t)
	origHandler := srv.Config.Handler
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/upload" && r.Method == http.MethodPost {
			gotAuth = r.Header.Get("Authorization")
		}
		origHandler.ServeHTTP(w, r)
	})

	u := newTestUploader(t, []byte("hello"))
	u.server = srv.URL
	u.token = "tok-xyz"
	u.parentId = "parent-1"
	u.httpClient = srv.Client()

	if err := u.init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	if gotAuth != "Bearer tok-xyz" {
		t.Errorf("auth header = %q, want Bearer tok-xyz", gotAuth)
	}
	_ = fb
}

func TestProgressReader_CountsBytes(t *testing.T) {
	pt := newPartTracker(1, MinPartSize)
	pr := &progressReader{r: strings.NewReader("hello world"), slot: &pt.parts[0]}
	data, err := io.ReadAll(pr)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(data) != "hello world" {
		t.Errorf("data mismatch: %q", data)
	}
	if got := pt.parts[0].Load(); got != int64(len("hello world")) {
		t.Errorf("slot = %d, want %d", got, len("hello world"))
	}
}

func TestProgressReader_RollbackOnReset(t *testing.T) {
	pt := newPartTracker(2, MinPartSize)
	// First attempt reads 5 bytes, then we simulate retry by resetting the slot.
	pr1 := &progressReader{r: strings.NewReader("hello"), slot: &pt.parts[0]}
	_, _ = io.ReadAll(pr1)
	if pt.parts[0].Load() != 5 {
		t.Fatalf("after first attempt: %d", pt.parts[0].Load())
	}
	// Simulate retry prologue.
	pt.parts[0].Store(0)
	pr2 := &progressReader{r: strings.NewReader("hi"), slot: &pt.parts[0]}
	_, _ = io.ReadAll(pr2)
	if got := pt.parts[0].Load(); got != 2 {
		t.Fatalf("after retry: %d, want 2", got)
	}
}

// ---------- Task 15: uploadPart happy path ----------

func TestUploadPart_Success(t *testing.T) {
	fb, srv := newFakeBus(t)
	u := newTestUploader(t, bytes.Repeat([]byte("a"), MinPartSize))
	u.server = srv.URL
	u.httpClient = srv.Client()
	u.tracker = newPartTracker(1, MinPartSize)
	if err := u.init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := u.uploadPart(context.Background(), 1); err != nil {
		t.Fatalf("uploadPart: %v", err)
	}
	if len(fb.patchParts[1]) != MinPartSize {
		t.Errorf("part 1 size = %d, want %d", len(fb.patchParts[1]), MinPartSize)
	}
	if got := u.tracker.parts[0].Load(); got != int64(MinPartSize) {
		t.Errorf("slot[0] = %d, want %d", got, MinPartSize)
	}
}

// ---------- Task 16: retry + giveup + rollback ----------

func TestUploadPart_Retry500(t *testing.T) {
	orig := sleepBackoff
	sleepBackoff = func(ctx context.Context, attempt int) error { return nil }
	t.Cleanup(func() { sleepBackoff = orig })

	fb, srv := newFakeBus(t)
	fb.patchStatus = func(partNum, attempt int) (int, string) {
		if attempt < 3 {
			return 500, `{"message":"transient"}`
		}
		return 204, ""
	}
	u := newTestUploader(t, bytes.Repeat([]byte("a"), MinPartSize))
	u.server = srv.URL
	u.httpClient = srv.Client()
	u.tracker = newPartTracker(1, MinPartSize)
	if err := u.init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := u.uploadPart(context.Background(), 1); err != nil {
		t.Fatalf("uploadPart: %v", err)
	}
	if got := u.tracker.parts[0].Load(); got != int64(MinPartSize) {
		t.Errorf("slot[0] = %d, want %d (progress should not exceed real bytes after retries)", got, MinPartSize)
	}
}

func TestUploadPart_GiveUpAfterMaxRetries(t *testing.T) {
	orig := sleepBackoff
	sleepBackoff = func(ctx context.Context, attempt int) error { return nil }
	t.Cleanup(func() { sleepBackoff = orig })

	fb, srv := newFakeBus(t)
	fb.patchStatus = func(partNum, attempt int) (int, string) {
		return 503, `{"message":"still down"}`
	}
	u := newTestUploader(t, bytes.Repeat([]byte("a"), MinPartSize))
	u.server = srv.URL
	u.httpClient = srv.Client()
	u.tracker = newPartTracker(1, MinPartSize)
	if err := u.init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	err := u.uploadPart(context.Background(), 1)
	if err == nil {
		t.Fatal("expected failure after max retries")
	}
	if !strings.Contains(err.Error(), "attempt 5/5") {
		t.Errorf("error should mention attempt 5/5, got: %v", err)
	}
	_ = fb
}

func TestUploadPart_ProgressRollbackOnRetry(t *testing.T) {
	orig := sleepBackoff
	sleepBackoff = func(ctx context.Context, attempt int) error { return nil }
	t.Cleanup(func() { sleepBackoff = orig })

	fb, srv := newFakeBus(t)
	fb.patchStatus = func(partNum, attempt int) (int, string) {
		if attempt == 1 {
			return 500, "half-read"
		}
		return 204, ""
	}
	u := newTestUploader(t, bytes.Repeat([]byte("a"), MinPartSize))
	u.server = srv.URL
	u.httpClient = srv.Client()
	u.tracker = newPartTracker(1, MinPartSize)
	if err := u.init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := u.uploadPart(context.Background(), 1); err != nil {
		t.Fatalf("uploadPart: %v", err)
	}
	if got := u.tracker.total(); got != int64(MinPartSize) {
		t.Errorf("total after retry = %d, want %d", got, MinPartSize)
	}
}

// ---------- Task 17: fail-fast on 4xx, 410, 413 ----------

func TestUploadPart_FailFastOn400(t *testing.T) {
	fb, srv := newFakeBus(t)
	fb.patchStatus = func(partNum, attempt int) (int, string) {
		return 400, `{"message":"bad part"}`
	}
	u := newTestUploader(t, bytes.Repeat([]byte("a"), MinPartSize))
	u.server = srv.URL
	u.httpClient = srv.Client()
	u.tracker = newPartTracker(1, MinPartSize)
	if err := u.init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	err := u.uploadPart(context.Background(), 1)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "bad part") {
		t.Errorf("want 'bad part' in error, got: %v", err)
	}
	if fb.patchParts[1] != nil {
		t.Errorf("should not have accepted the part on 400")
	}
}

func TestUploadPart_410Gone(t *testing.T) {
	fb, srv := newFakeBus(t)
	fb.patchStatus = func(partNum, attempt int) (int, string) {
		return 410, ""
	}
	u := newTestUploader(t, bytes.Repeat([]byte("a"), MinPartSize))
	u.server = srv.URL
	u.httpClient = srv.Client()
	u.tracker = newPartTracker(1, MinPartSize)
	if err := u.init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	err := u.uploadPart(context.Background(), 1)
	if !errors.Is(err, errExpired) {
		t.Errorf("want errExpired, got %v", err)
	}
	_ = fb
}

func TestUploadPart_413TooLarge(t *testing.T) {
	fb, srv := newFakeBus(t)
	fb.patchStatus = func(partNum, attempt int) (int, string) {
		return 413, ""
	}
	u := newTestUploader(t, bytes.Repeat([]byte("a"), MinPartSize))
	u.server = srv.URL
	u.httpClient = srv.Client()
	u.tracker = newPartTracker(1, MinPartSize)
	if err := u.init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	err := u.uploadPart(context.Background(), 1)
	if !errors.Is(err, errPartTooLarge) {
		t.Errorf("want errPartTooLarge, got %v", err)
	}
	_ = fb
}

// ---------- Task 18: uploadAllParts ----------

func TestUploadAllParts_Concurrent(t *testing.T) {
	fb, srv := newFakeBus(t)
	const parts = 10
	size := int64(parts * MinPartSize)
	u := newTestUploader(t, bytes.Repeat([]byte("a"), int(size)))
	u.fileSize = size
	u.server = srv.URL
	u.httpClient = srv.Client()
	u.workers = 4
	u.tracker = newPartTracker(parts, MinPartSize)
	if err := u.init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := u.uploadAllParts(context.Background(), parts); err != nil {
		t.Fatalf("uploadAllParts: %v", err)
	}
	for i := 1; i <= parts; i++ {
		if len(fb.patchParts[i]) == 0 {
			t.Errorf("part %d missing", i)
		}
	}
	if got := u.tracker.total(); got != size {
		t.Errorf("total = %d, want %d", got, size)
	}
}

func TestUploadAllParts_CancelPropagates(t *testing.T) {
	orig := sleepBackoff
	sleepBackoff = func(ctx context.Context, attempt int) error { return nil }
	t.Cleanup(func() { sleepBackoff = orig })

	fb, srv := newFakeBus(t)
	// Make every request fail after a small delay so we can cancel mid-upload.
	fb.patchStatus = func(partNum, attempt int) (int, string) {
		time.Sleep(50 * time.Millisecond)
		return 500, `{"message":"boom"}`
	}
	u := newTestUploader(t, bytes.Repeat([]byte("a"), MinPartSize*4))
	u.fileSize = int64(MinPartSize * 4)
	u.server = srv.URL
	u.httpClient = srv.Client()
	u.workers = 2
	u.tracker = newPartTracker(4, MinPartSize)
	if err := u.init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- u.uploadAllParts(ctx, 4) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("uploadAllParts did not return after cancel")
	}
	_ = fb
}

// ---------- Task 19: complete ----------

func TestComplete_Success(t *testing.T) {
	fb, srv := newFakeBus(t)
	u := newTestUploader(t, []byte("hi"))
	u.server = srv.URL
	u.httpClient = srv.Client()
	if err := u.init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	result, err := u.complete(context.Background())
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	want := srv.URL + "/final-id"
	if result.link != want {
		t.Errorf("link = %q, want %q", result.link, want)
	}
	if len(result.rawJSON) == 0 {
		t.Errorf("rawJSON should not be empty")
	}
	if fb.completeCalls != 1 {
		t.Errorf("completeCalls = %d, want 1", fb.completeCalls)
	}
}

func TestComplete_Expired(t *testing.T) {
	fb, srv := newFakeBus(t)
	// Replace the server handler to always return 410 on the complete route.
	origHandler := srv.Config.Handler
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/upload/") && r.Method == http.MethodPost {
			w.WriteHeader(410)
			return
		}
		origHandler.ServeHTTP(w, r)
	})
	u := newTestUploader(t, []byte("hi"))
	u.server = srv.URL
	u.httpClient = srv.Client()
	u.uploadId = fb.initToken
	_, err := u.complete(context.Background())
	if !errors.Is(err, errExpired) {
		t.Errorf("want errExpired, got %v", err)
	}
}

// ---------- Task 20: run orchestrator ----------

func TestRun_EndToEnd(t *testing.T) {
	_, srv := newFakeBus(t)
	size := int64(3 * MinPartSize)
	u := newTestUploader(t, bytes.Repeat([]byte("x"), int(size)))
	u.fileSize = size
	u.server = srv.URL
	u.httpClient = srv.Client()
	u.workers = 2
	result, err := u.run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.HasSuffix(result.link, "/final-id") {
		t.Errorf("link = %q, want suffix /final-id", result.link)
	}
	if got := u.tracker.total(); got != u.fileSize {
		t.Errorf("tracker total = %d, want %d", got, u.fileSize)
	}
}

// ---------- Task 25: guest-link uploads skip auth header ----------

func TestUpload_GuestLinkNoAuth(t *testing.T) {
	var (
		gotAuthMu sync.Mutex
		gotAuth   string
	)
	_, srv := newFakeBus(t)
	origHandler := srv.Config.Handler
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/u/") {
			gotAuthMu.Lock()
			gotAuth = r.Header.Get("Authorization")
			gotAuthMu.Unlock()
		}
		origHandler.ServeHTTP(w, r)
	})

	u := newTestUploader(t, bytes.Repeat([]byte("a"), MinPartSize))
	u.server = srv.URL
	u.httpClient = srv.Client()
	u.token = "tok-should-not-be-sent"
	u.guestLinkId = "guest-link"
	u.tracker = newPartTracker(1, MinPartSize)
	if err := u.init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := u.uploadPart(context.Background(), 1); err != nil {
		t.Fatalf("uploadPart: %v", err)
	}
	gotAuthMu.Lock()
	defer gotAuthMu.Unlock()
	if gotAuth != "" {
		t.Errorf("guest upload must not send Authorization header, got %q", gotAuth)
	}
}
