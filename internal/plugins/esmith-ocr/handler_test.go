package esmithocr

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testSecret = "test-secret"

func makeToken(secret, uid string, exp int64) string {
	expStr := strconv.FormatInt(exp, 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(uid + "." + expStr))
	return uid + "." + expStr + "." + hex.EncodeToString(mac.Sum(nil))
}

// tinyJPEG is a minimal payload http.DetectContentType sniffs as image/jpeg.
var tinyJPEG = []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00}

func newTestHandler(runner func(ctx context.Context, imagePath string) (*Result, error)) *Handler {
	h := NewHandler(testSecret, []string{"https://odoo-esmith*.odoo.com"})
	if runner != nil {
		h.runner = runner
	}
	return h
}

func newMux(h *Handler) *http.ServeMux {
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	return mux
}

func postExtract(t *testing.T, mux *http.ServeMux, body map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/fr/ocr/extract", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://odoo-esmith.odoo.com")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func countTempFiles(t *testing.T) int {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), "esmith-ocr-*"))
	if err != nil {
		t.Fatalf("glob temp dir: %v", err)
	}
	return len(matches)
}

func TestExtractHappyPath(t *testing.T) {
	var sawPath string
	var existedDuringRun bool
	runner := func(ctx context.Context, imagePath string) (*Result, error) {
		sawPath = imagePath
		if _, err := os.Stat(imagePath); err == nil {
			existedDuringRun = true
		}
		no := "AB12345678"
		conf := 0.9
		return &Result{InvoiceNo: &no, Confidence: &conf}, nil
	}
	h := newTestHandler(runner)
	mux := newMux(h)

	token := makeToken(testSecret, "7", time.Now().Add(5*time.Minute).Unix())
	image := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(tinyJPEG)
	rec := postExtract(t, mux, map[string]string{"image": image, "token": token})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Success bool    `json:"success"`
		Result  *Result `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Success || resp.Result == nil || resp.Result.InvoiceNo == nil || *resp.Result.InvoiceNo != "AB12345678" {
		t.Fatalf("unexpected response: %s", rec.Body.String())
	}
	if !existedDuringRun {
		t.Fatal("temp file did not exist while runner was executing")
	}
	if _, err := os.Stat(sawPath); !os.IsNotExist(err) {
		t.Fatalf("temp file %s was not removed after request (stat err = %v)", sawPath, err)
	}
	// CORS echo on the actual response.
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://odoo-esmith.odoo.com" {
		t.Fatalf("Access-Control-Allow-Origin = %q", got)
	}
}

func TestExtractRunnerNilResult(t *testing.T) {
	h := newTestHandler(func(ctx context.Context, imagePath string) (*Result, error) {
		return nil, nil
	})
	mux := newMux(h)
	token := makeToken(testSecret, "7", time.Now().Add(time.Minute).Unix())
	image := base64.StdEncoding.EncodeToString(tinyJPEG)
	rec := postExtract(t, mux, map[string]string{"image": image, "token": token})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["success"] != true {
		t.Fatalf("success = %v", resp["success"])
	}
	if v, present := resp["result"]; !present || v != nil {
		t.Fatalf("result = %v (present=%v), want explicit null", v, present)
	}
}

func TestExtractRunnerError(t *testing.T) {
	h := newTestHandler(func(ctx context.Context, imagePath string) (*Result, error) {
		return nil, fmt.Errorf("agy CLI not found")
	})
	mux := newMux(h)
	token := makeToken(testSecret, "7", time.Now().Add(time.Minute).Unix())
	image := base64.StdEncoding.EncodeToString(tinyJPEG)
	rec := postExtract(t, mux, map[string]string{"image": image, "token": token})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (degrade to null)", rec.Code)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"result":null`)) {
		t.Fatalf("body = %s, want result:null", rec.Body.String())
	}
}

func TestExtractTokenFailures(t *testing.T) {
	validImage := base64.StdEncoding.EncodeToString(tinyJPEG)
	forged := makeToken("wrong-secret", "7", time.Now().Add(time.Minute).Unix())
	expired := makeToken(testSecret, "7", time.Now().Add(-time.Minute).Unix())

	cases := []struct {
		name  string
		token string
	}{
		{"missing token", ""},
		{"expired token", expired},
		{"forged signature", forged},
		{"malformed token", "just-a-string"},
		{"two parts only", "7." + strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10)},
		{"non-hex signature", "7." + strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10) + ".zzzz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runnerCalled := false
			h := newTestHandler(func(ctx context.Context, imagePath string) (*Result, error) {
				runnerCalled = true
				return nil, nil
			})
			rec := postExtract(t, newMux(h), map[string]string{"image": validImage, "token": tc.token})
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if body := rec.Body.String(); !bytes.Contains([]byte(body), []byte(`"error":"unauthorized"`)) {
				t.Fatalf("body = %s, want generic unauthorized", body)
			}
			if runnerCalled {
				t.Fatal("runner must not be called on auth failure")
			}
		})
	}
}

func TestExtractOversizeImage(t *testing.T) {
	runnerCalled := false
	h := newTestHandler(func(ctx context.Context, imagePath string) (*Result, error) {
		runnerCalled = true
		return nil, nil
	})
	mux := newMux(h)
	before := countTempFiles(t)

	// Valid jpeg header followed by >10MB of padding.
	big := append(append([]byte{}, tinyJPEG...), make([]byte, maxImageBytes+1)...)
	token := makeToken(testSecret, "7", time.Now().Add(time.Minute).Unix())
	rec := postExtract(t, mux, map[string]string{
		"image": base64.StdEncoding.EncodeToString(big),
		"token": token,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if runnerCalled {
		t.Fatal("runner must not run for oversize image")
	}
	if after := countTempFiles(t); after != before {
		t.Fatalf("temp files leaked: before=%d after=%d", before, after)
	}
}

func TestExtractNonImage(t *testing.T) {
	runnerCalled := false
	h := newTestHandler(func(ctx context.Context, imagePath string) (*Result, error) {
		runnerCalled = true
		return nil, nil
	})
	token := makeToken(testSecret, "7", time.Now().Add(time.Minute).Unix())
	rec := postExtract(t, newMux(h), map[string]string{
		"image": base64.StdEncoding.EncodeToString([]byte("this is plain text, not an image")),
		"token": token,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if runnerCalled {
		t.Fatal("runner must not run for non-image payload")
	}
}

func TestExtractInvalidBase64(t *testing.T) {
	h := newTestHandler(nil)
	token := makeToken(testSecret, "7", time.Now().Add(time.Minute).Unix())
	rec := postExtract(t, newMux(h), map[string]string{
		"image": "!!!not-base64!!!",
		"token": token,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestExtractMethodNotAllowed(t *testing.T) {
	h := newTestHandler(nil)
	mux := newMux(h)
	req := httptest.NewRequest(http.MethodGet, "/fr/ocr/extract", nil)
	req.Header.Set("Origin", "https://odoo-esmith.odoo.com")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestExtractPreflight(t *testing.T) {
	h := newTestHandler(nil)
	mux := newMux(h)

	t.Run("allowed origin", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/fr/ocr/extract", nil)
		req.Header.Set("Origin", "https://odoo-esmith-v18-stage39.odoo.com")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", rec.Code)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://odoo-esmith-v18-stage39.odoo.com" {
			t.Fatalf("Access-Control-Allow-Origin = %q", got)
		}
	})

	t.Run("disallowed origin", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/fr/ocr/extract", nil)
		req.Header.Set("Origin", "https://evil.example.com")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Fatalf("Access-Control-Allow-Origin leaked for disallowed origin: %q", got)
		}
	})
}

func TestExtractSemaphoreCapsConcurrency(t *testing.T) {
	var inFlight, maxSeen atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{}, 3)

	runner := func(ctx context.Context, imagePath string) (*Result, error) {
		n := inFlight.Add(1)
		for {
			cur := maxSeen.Load()
			if n <= cur || maxSeen.CompareAndSwap(cur, n) {
				break
			}
		}
		started <- struct{}{}
		<-release
		inFlight.Add(-1)
		return nil, nil
	}
	h := newTestHandler(runner)
	mux := newMux(h)

	token := makeToken(testSecret, "7", time.Now().Add(time.Minute).Unix())
	image := base64.StdEncoding.EncodeToString(tinyJPEG)

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := postExtract(t, mux, map[string]string{"image": image, "token": token})
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200", rec.Code)
			}
		}()
	}

	// Wait for exactly two runners to start; the third must be queued.
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for runners to start")
		}
	}
	// Give the third request a moment to (incorrectly) start if the
	// semaphore were broken.
	select {
	case <-started:
		t.Fatal("third runner started while two were in flight")
	case <-time.After(200 * time.Millisecond):
	}
	if got := maxSeen.Load(); got != 2 {
		t.Fatalf("max in-flight = %d, want 2", got)
	}

	close(release)
	// The queued third runner still consumes one `started` slot.
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("queued runner never started after release")
	}
	wg.Wait()
	if got := maxSeen.Load(); got > 2 {
		t.Fatalf("max in-flight = %d, want <= 2", got)
	}
}
