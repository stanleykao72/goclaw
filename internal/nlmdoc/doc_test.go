package nlmdoc

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// staticToken is a TokenSource that returns a fixed token (no rclone).
type staticToken struct {
	tok         string
	invalidated int
}

func (s *staticToken) Token(ctx context.Context) (string, error) { return s.tok, nil }
func (s *staticToken) Invalidate()                               { s.invalidated++ }

// recordedReq captures a request the fake doer saw.
type recordedReq struct {
	Method      string
	URL         string
	ContentType string
	Body        string
}

// fakeDoer routes requests through a handler func and records each request.
type fakeDoer struct {
	reqs    []recordedReq
	handler func(req *http.Request, body string) (*http.Response, error)
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	var body string
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		body = string(b)
	}
	f.reqs = append(f.reqs, recordedReq{
		Method:      req.Method,
		URL:         req.URL.String(),
		ContentType: req.Header.Get("Content-Type"),
		Body:        body,
	})
	return f.handler(req, body)
}

func jsonResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func newLib(doer *fakeDoer) *DriveDocLibrary {
	return NewDriveDocLibrary(&staticToken{tok: "T"}, doer)
}

func TestEnsureFolder_CreatesMissingLevels(t *testing.T) {
	doer := &fakeDoer{}
	doer.handler = func(req *http.Request, body string) (*http.Response, error) {
		switch req.Method {
		case http.MethodGet:
			// All folder lookups miss.
			return jsonResp(200, `{"files":[]}`), nil
		case http.MethodPost:
			// Each createFolder returns a deterministic id.
			return jsonResp(200, `{"id":"fid"}`), nil
		}
		t.Fatalf("unexpected method %s", req.Method)
		return nil, nil
	}

	lib := newLib(doer)
	id, err := lib.EnsureFolder(context.Background(), []string{"goclaw-memory", "esmith", "user"})
	if err != nil {
		t.Fatalf("EnsureFolder: %v", err)
	}
	if id != "fid" {
		t.Fatalf("leaf id = %q, want fid", id)
	}

	// 3 levels, all missing → 3 GET (find) + 3 POST (create) = 6 requests.
	if len(doer.reqs) != 6 {
		t.Fatalf("got %d requests, want 6", len(doer.reqs))
	}

	// First find: name='goclaw-memory', mimeType=folder, 'root' in parents.
	first := doer.reqs[0]
	if first.Method != http.MethodGet {
		t.Fatalf("req0 method = %s", first.Method)
	}
	if !strings.Contains(first.URL, "www.googleapis.com/drive/v3/files") {
		t.Fatalf("req0 url = %s", first.URL)
	}
	if !strings.Contains(first.URL, "goclaw-memory") {
		t.Fatalf("req0 query missing folder name: %s", first.URL)
	}
	if !strings.Contains(first.URL, "root") {
		t.Fatalf("req0 query missing root parent: %s", first.URL)
	}

	// First create POST: JSON metadata with folder mimeType.
	create := doer.reqs[1]
	if create.Method != http.MethodPost {
		t.Fatalf("req1 method = %s", create.Method)
	}
	if !strings.Contains(create.Body, mimeFolder) {
		t.Fatalf("create body missing folder mimeType: %s", create.Body)
	}
	if !strings.Contains(create.Body, "goclaw-memory") {
		t.Fatalf("create body missing name: %s", create.Body)
	}
}

func TestEnsureFolder_ReusesExisting(t *testing.T) {
	doer := &fakeDoer{}
	doer.handler = func(req *http.Request, body string) (*http.Response, error) {
		if req.Method == http.MethodGet {
			return jsonResp(200, `{"files":[{"id":"existing","name":"x"}]}`), nil
		}
		t.Fatalf("must not create when folder exists; got %s %s", req.Method, req.URL)
		return nil, nil
	}
	lib := newLib(doer)
	id, err := lib.EnsureFolder(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("EnsureFolder: %v", err)
	}
	if id != "existing" {
		t.Fatalf("id = %q, want existing", id)
	}
	// 2 levels, both exist → 2 GET, 0 POST.
	if len(doer.reqs) != 2 {
		t.Fatalf("got %d requests, want 2", len(doer.reqs))
	}
}

func TestCreateDoc_MultipartRelated(t *testing.T) {
	doer := &fakeDoer{}
	doer.handler = func(req *http.Request, body string) (*http.Response, error) {
		return jsonResp(200, `{"id":"doc123","name":"user-12345678-王小明","mimeType":"`+mimeGoogleDoc+`"}`), nil
	}
	lib := newLib(doer)
	id, err := lib.CreateDoc(context.Background(), "folderA", "user-12345678-王小明", "hello")
	if err != nil {
		t.Fatalf("CreateDoc: %v", err)
	}
	if id != "doc123" {
		t.Fatalf("doc id = %q, want doc123", id)
	}

	r := doer.reqs[0]
	if r.Method != http.MethodPost {
		t.Fatalf("method = %s", r.Method)
	}
	if !strings.Contains(r.URL, "/upload/drive/v3/files") {
		t.Fatalf("url = %s, want upload endpoint", r.URL)
	}
	if !strings.Contains(r.URL, "uploadType=multipart") {
		t.Fatalf("url missing uploadType=multipart: %s", r.URL)
	}
	if !strings.HasPrefix(r.ContentType, "multipart/related") {
		t.Fatalf("content-type = %q, want multipart/related", r.ContentType)
	}
	// Body must carry the doc mimeType (metadata part) and the initial text.
	if !strings.Contains(r.Body, mimeGoogleDoc) {
		t.Fatalf("body missing google-doc mimeType: %s", r.Body)
	}
	if !strings.Contains(r.Body, "folderA") {
		t.Fatalf("body missing parent folder: %s", r.Body)
	}
	if !strings.Contains(r.Body, "hello") {
		t.Fatalf("body missing initial text: %s", r.Body)
	}
}

func TestAppendText_ExportThenUpdateMediaStripsBOM(t *testing.T) {
	doer := &fakeDoer{}
	doer.handler = func(req *http.Request, body string) (*http.Response, error) {
		// Export GET returns existing content WITH a leading BOM.
		if req.Method == http.MethodGet && strings.Contains(req.URL.String(), "/export") {
			return &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(strings.NewReader(utf8BOM + "line 1\n")),
				Header:     make(http.Header),
			}, nil
		}
		// Update-media PATCH.
		if req.Method == http.MethodPatch {
			return jsonResp(200, `{"id":"doc1","mimeType":"`+mimeGoogleDoc+`"}`), nil
		}
		t.Fatalf("unexpected req %s %s", req.Method, req.URL)
		return nil, nil
	}
	lib := newLib(doer)
	if err := lib.AppendText(context.Background(), "doc1", "line 2\n"); err != nil {
		t.Fatalf("AppendText: %v", err)
	}

	if len(doer.reqs) != 2 {
		t.Fatalf("got %d reqs, want 2 (export + patch)", len(doer.reqs))
	}
	export := doer.reqs[0]
	if !strings.Contains(export.URL, "/export") || !strings.Contains(export.URL, "mimeType=text") {
		t.Fatalf("export url = %s", export.URL)
	}
	patch := doer.reqs[1]
	if patch.Method != http.MethodPatch {
		t.Fatalf("patch method = %s", patch.Method)
	}
	if !strings.Contains(patch.URL, "uploadType=media") {
		t.Fatalf("patch url missing uploadType=media: %s", patch.URL)
	}
	if patch.ContentType != mimeText {
		t.Fatalf("patch content-type = %q, want %q", patch.ContentType, mimeText)
	}
	// Combined body = existing (BOM stripped) + new text. No BOM, no dup.
	want := "line 1\nline 2\n"
	if patch.Body != want {
		t.Fatalf("patch body = %q, want %q", patch.Body, want)
	}
	if strings.Contains(patch.Body, utf8BOM) {
		t.Fatalf("patch body still contains BOM: %q", patch.Body)
	}
}

func TestAppendText_EmptyTextNoOp(t *testing.T) {
	doer := &fakeDoer{handler: func(req *http.Request, body string) (*http.Response, error) {
		t.Fatal("AppendText(\"\") must not issue any request")
		return nil, nil
	}}
	lib := newLib(doer)
	if err := lib.AppendText(context.Background(), "doc1", ""); err != nil {
		t.Fatalf("AppendText empty: %v", err)
	}
}

func TestDoRaw_RetriesRateLimit(t *testing.T) {
	orig := driveRetryBaseDelay
	driveRetryBaseDelay = time.Millisecond
	defer func() { driveRetryBaseDelay = orig }()

	doer := &fakeDoer{}
	calls := 0
	doer.handler = func(req *http.Request, body string) (*http.Response, error) {
		calls++
		if calls == 1 {
			return jsonResp(403, `{"error":{"code":403,"status":"PERMISSION_DENIED","message":"Quota exceeded","errors":[{"reason":"rateLimitExceeded","domain":"usageLimits"}]}}`), nil
		}
		return jsonResp(200, `{"files":[]}`), nil
	}
	// Shorten backoff so the test is fast.
	lib := newLib(doer)
	_, err := lib.findFolder(context.Background(), "x", "")
	if err != nil {
		t.Fatalf("findFolder: %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (1 rate-limited + 1 success)", calls)
	}
}

func TestDoRaw_ReauthOn401(t *testing.T) {
	tok := &staticToken{tok: "T"}
	doer := &fakeDoer{}
	calls := 0
	doer.handler = func(req *http.Request, body string) (*http.Response, error) {
		calls++
		if calls == 1 {
			return jsonResp(401, `{"error":{"code":401,"message":"invalid creds"}}`), nil
		}
		return jsonResp(200, `{"files":[]}`), nil
	}
	lib := NewDriveDocLibrary(tok, doer)
	if _, err := lib.findFolder(context.Background(), "x", ""); err != nil {
		t.Fatalf("findFolder: %v", err)
	}
	if tok.invalidated != 1 {
		t.Fatalf("token invalidated %d times, want 1", tok.invalidated)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestDoRaw_NonRetriableError(t *testing.T) {
	doer := &fakeDoer{handler: func(req *http.Request, body string) (*http.Response, error) {
		return jsonResp(404, `{"error":{"code":404,"message":"not found"}}`), nil
	}}
	lib := newLib(doer)
	if _, err := lib.findFolder(context.Background(), "x", ""); err == nil {
		t.Fatal("expected error on 404")
	}
}

func TestIsRateLimited(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"rate-limit reason", 403, `{"error":{"errors":[{"reason":"rateLimitExceeded"}]}}`, true},
		{"user rate-limit", 403, `{"error":{"errors":[{"reason":"userRateLimitExceeded"}]}}`, true},
		{"status RATE_LIMIT", 429, `{"error":{"status":"RESOURCE_EXHAUSTED RATE_LIMIT_EXCEEDED"}}`, true},
		{"real perms 403", 403, `{"error":{"errors":[{"reason":"insufficientPermissions"}]}}`, false},
		{"404 not rate", 404, `{"error":{}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRateLimited(tc.status, []byte(tc.body)); got != tc.want {
				t.Fatalf("isRateLimited(%d) = %v, want %v", tc.status, got, tc.want)
			}
		})
	}
}
