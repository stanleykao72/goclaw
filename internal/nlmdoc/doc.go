package nlmdoc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Drive API endpoints. All Doc create/append goes through the Drive API
// (drive.googleapis.com) because the Docs API is service-disabled on the
// rclone OAuth project — see the package doc comment.
const (
	driveFilesURL       = "https://www.googleapis.com/drive/v3/files"
	driveUploadFilesURL = "https://www.googleapis.com/upload/drive/v3/files"

	mimeFolder    = "application/vnd.google-apps.folder"
	mimeGoogleDoc = "application/vnd.google-apps.document"
	mimeText      = "text/plain"

	// utf8BOM is prepended by Drive's text/plain export on every read. We strip
	// it before re-appending so BOMs do not accumulate across drains. Written
	// as an escape (not a literal BOM) so the source file stays BOM-free.
	utf8BOM = "\uFEFF"

	// driveMaxRetries bounds the rate-limit backoff. The shared rclone GCP
	// project (202264815644) enforces a per-MINUTE Drive query quota across all
	// rclone users, so a burst gets a retriable 403 (reason rateLimitExceeded);
	// the backoff must be long enough to outlast a 60s quota window, hence 6
	// attempts with a multi-second base (~2+4+8+16+20+20 ≈ 70s).
	driveMaxRetries = 6

	// driveRetryMaxDelay caps a single backoff step so growth stays bounded.
	driveRetryMaxDelay = 20 * time.Second
)

// driveRetryBaseDelay is the base backoff for rate-limit retries. It is a var
// (not const) so tests can shrink it to keep the suite fast.
var driveRetryBaseDelay = 2 * time.Second

// httpDoer is the minimal HTTP surface the Drive client needs. Injectable so
// tests assert request shapes against an httptest server (or a pure fake)
// without real network.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// DocLibrary is the Drive-Doc surface the provisioner (sub-phase 2.2) depends
// on. Kept small so unit tests fake it directly.
type DocLibrary interface {
	// EnsureFolder ensures the nested folder path exists under My Drive root,
	// creating each missing level, and returns the leaf folder id. Idempotent.
	EnsureFolder(ctx context.Context, path []string) (folderID string, err error)
	// CreateDoc creates a Google Doc named title inside folderID and returns
	// its file id (used as drive_doc_id). initialText may be empty.
	CreateDoc(ctx context.Context, folderID, title, initialText string) (docID string, err error)
	// AppendText appends text to the Doc via export-then-update-media. A
	// caller-supplied separator is the responsibility of the caller; this
	// method does not insert one between successive appends beyond preserving
	// the existing content verbatim.
	AppendText(ctx context.Context, docID, text string) error
}

// DriveDocLibrary implements DocLibrary against the Google Drive API using a
// rclone-brokered token.
type DriveDocLibrary struct {
	tokens TokenSource
	http   httpDoer

	// folderCache memoizes resolved folder ids by their full path key so
	// EnsureFolder does not re-issue files.list calls on every provision —
	// those calls were a major contributor to the shared-project per-minute
	// Drive quota (403 rateLimitExceeded). Folder ids are stable once created.
	folderMu    sync.Mutex
	folderCache map[string]string
}

// NewDriveDocLibrary constructs a Drive-backed DocLibrary. A nil http client
// falls back to http.DefaultClient.
func NewDriveDocLibrary(tokens TokenSource, client httpDoer) *DriveDocLibrary {
	if client == nil {
		client = http.DefaultClient
	}
	return &DriveDocLibrary{tokens: tokens, http: client, folderCache: map[string]string{}}
}

// driveErrorBody is the standard Drive API error envelope.
type driveErrorBody struct {
	Error struct {
		Code    int    `json:"code"`
		Status  string `json:"status"`
		Message string `json:"message"`
		Errors  []struct {
			Reason string `json:"reason"`
			Domain string `json:"domain"`
		} `json:"errors"`
	} `json:"error"`
}

// isRateLimited reports whether a 403 body is a retriable rate-limit error
// (NOT a real permissions failure). The shared rclone project intermittently
// returns these under light bursts.
func isRateLimited(status int, body []byte) bool {
	if status != http.StatusForbidden && status != http.StatusTooManyRequests {
		return false
	}
	var e driveErrorBody
	if json.Unmarshal(body, &e) != nil {
		return false
	}
	if strings.Contains(strings.ToUpper(e.Error.Status), "RATE_LIMIT_EXCEEDED") {
		return true
	}
	for _, sub := range e.Error.Errors {
		if sub.Reason == "rateLimitExceeded" || sub.Reason == "userRateLimitExceeded" {
			return true
		}
	}
	return false
}

// doJSON executes an authenticated request and decodes a JSON response into
// out. It retries on rate-limit 403/429 with backoff and re-brokers the token
// once on 401. The bodyFn builds a fresh body+content-type per attempt (bodies
// are not replayable). out may be nil to discard the body.
func (l *DriveDocLibrary) doJSON(ctx context.Context, method, rawURL string, bodyFn func() (io.Reader, string), out any) error {
	body, err := l.doRaw(ctx, method, rawURL, bodyFn)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode drive response: %w", err)
	}
	return nil
}

// doRaw executes an authenticated request and returns the raw response body,
// applying token-refresh and rate-limit retry policy.
func (l *DriveDocLibrary) doRaw(ctx context.Context, method, rawURL string, bodyFn func() (io.Reader, string)) ([]byte, error) {
	var lastErr error
	triedReauth := false

	for attempt := 0; attempt <= driveMaxRetries; attempt++ {
		tok, err := l.tokens.Token(ctx)
		if err != nil {
			return nil, fmt.Errorf("acquire token: %w", err)
		}

		var rdr io.Reader
		var contentType string
		if bodyFn != nil {
			rdr, contentType = bodyFn()
		}
		req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}

		resp, err := l.http.Do(req)
		if err != nil {
			lastErr = err
			if !sleepBackoff(ctx, attempt) {
				return nil, lastErr
			}
			continue
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return respBody, nil
		}

		// 401 → token may have expired mid-op. Re-broker once and retry.
		if resp.StatusCode == http.StatusUnauthorized && !triedReauth {
			triedReauth = true
			l.tokens.Invalidate()
			continue
		}

		// Retriable rate-limit?
		if isRateLimited(resp.StatusCode, respBody) {
			lastErr = fmt.Errorf("drive rate limited (%d): %s", resp.StatusCode, string(respBody))
			if !sleepBackoff(ctx, attempt) {
				return nil, lastErr
			}
			continue
		}

		return nil, fmt.Errorf("drive %s %s: status %d: %s", method, rawURL, resp.StatusCode, string(respBody))
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("drive request exhausted retries")
	}
	return nil, lastErr
}

// sleepBackoff waits driveRetryBaseDelay * 2^attempt (or returns false if there
// are no attempts left / the context is done).
func sleepBackoff(ctx context.Context, attempt int) bool {
	if attempt >= driveMaxRetries {
		return false
	}
	delay := driveRetryBaseDelay * (1 << attempt)
	if delay > driveRetryMaxDelay {
		delay = driveRetryMaxDelay
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// EnsureFolder resolves/creates each level of the path under My Drive root,
// chaining parents, and returns the leaf folder id. Idempotent: an existing
// folder at a level is reused.
func (l *DriveDocLibrary) EnsureFolder(ctx context.Context, path []string) (string, error) {
	cacheKey := strings.Join(path, "/")
	l.folderMu.Lock()
	if id, ok := l.folderCache[cacheKey]; ok {
		l.folderMu.Unlock()
		return id, nil
	}
	l.folderMu.Unlock()

	parent := "" // "" == My Drive root for the queries below
	for _, name := range path {
		if name == "" {
			return "", fmt.Errorf("EnsureFolder: empty path segment")
		}
		id, err := l.findFolder(ctx, name, parent)
		if err != nil {
			return "", err
		}
		if id == "" {
			id, err = l.createFolder(ctx, name, parent)
			if err != nil {
				return "", err
			}
		}
		parent = id
	}
	if parent == "" {
		return "", fmt.Errorf("EnsureFolder: empty path")
	}
	l.folderMu.Lock()
	l.folderCache[cacheKey] = parent
	l.folderMu.Unlock()
	return parent, nil
}

// driveFileList is the shape of a files.list response.
type driveFileList struct {
	Files []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"files"`
}

// findFolder returns the id of a folder named name under parent (root if
// parent==""), or "" if none exists.
func (l *DriveDocLibrary) findFolder(ctx context.Context, name, parent string) (string, error) {
	q := fmt.Sprintf("name = %s and mimeType = %s and trashed = false",
		driveQuote(name), driveQuote(mimeFolder))
	if parent != "" {
		q += fmt.Sprintf(" and %s in parents", driveQuote(parent))
	} else {
		q += " and 'root' in parents"
	}
	u := driveFilesURL + "?" + url.Values{
		"q":      {q},
		"fields": {"files(id,name)"},
	}.Encode()

	var list driveFileList
	if err := l.doJSON(ctx, http.MethodGet, u, nil, &list); err != nil {
		return "", err
	}
	if len(list.Files) == 0 {
		return "", nil
	}
	return list.Files[0].ID, nil
}

// createFolder creates a folder named name under parent (root if parent=="")
// and returns its id.
func (l *DriveDocLibrary) createFolder(ctx context.Context, name, parent string) (string, error) {
	meta := map[string]any{"name": name, "mimeType": mimeFolder}
	if parent != "" {
		meta["parents"] = []string{parent}
	}
	bodyFn := func() (io.Reader, string) {
		b, _ := json.Marshal(meta)
		return bytes.NewReader(b), "application/json"
	}
	u := driveFilesURL + "?" + url.Values{"fields": {"id"}}.Encode()
	var created struct {
		ID string `json:"id"`
	}
	if err := l.doJSON(ctx, http.MethodPost, u, bodyFn, &created); err != nil {
		return "", err
	}
	if created.ID == "" {
		return "", fmt.Errorf("createFolder %q: empty id in response", name)
	}
	return created.ID, nil
}

// CreateDoc creates a Google Doc named title inside folderID via a Drive
// multipart upload (metadata + text/plain body, converted to a Doc). The Doc
// is placed in the folder in one shot via parents in the metadata — no
// separate move call. Returns the file id.
func (l *DriveDocLibrary) CreateDoc(ctx context.Context, folderID, title, initialText string) (string, error) {
	if title == "" {
		return "", fmt.Errorf("CreateDoc: empty title")
	}
	meta := map[string]any{"name": title, "mimeType": mimeGoogleDoc}
	if folderID != "" {
		meta["parents"] = []string{folderID}
	}

	bodyFn := func() (io.Reader, string) {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)

		metaHdr := textproto.MIMEHeader{}
		metaHdr.Set("Content-Type", "application/json; charset=UTF-8")
		mp, _ := w.CreatePart(metaHdr)
		mb, _ := json.Marshal(meta)
		mp.Write(mb)

		textHdr := textproto.MIMEHeader{}
		textHdr.Set("Content-Type", mimeText)
		tp, _ := w.CreatePart(textHdr)
		io.WriteString(tp, initialText)

		w.Close()
		// Drive expects multipart/related, not the multipart/form-data the
		// stdlib writer emits — rewrite the content-type, keep the boundary.
		ct := "multipart/related; boundary=" + w.Boundary()
		return &buf, ct
	}

	u := driveUploadFilesURL + "?" + url.Values{
		"uploadType": {"multipart"},
		"fields":     {"id,name,mimeType,parents"},
	}.Encode()

	var created struct {
		ID string `json:"id"`
	}
	if err := l.doJSON(ctx, http.MethodPost, u, bodyFn, &created); err != nil {
		return "", err
	}
	if created.ID == "" {
		return "", fmt.Errorf("CreateDoc %q: empty id in response", title)
	}
	return created.ID, nil
}

// AppendText appends text to a Google Doc via read-modify-write: export the
// current Doc as text/plain (stripping the BOM Drive prepends), concatenate
// the new text, and re-upload via update-media. There is no server-side append
// without the (disabled) Docs API.
//
// CALLER CONTRACT: this preserves existing content verbatim and concatenates
// text directly — callers append their own separator (e.g. a trailing "\n")
// because Drive's export adds no trailing newline.
func (l *DriveDocLibrary) AppendText(ctx context.Context, docID, text string) error {
	if docID == "" {
		return fmt.Errorf("AppendText: empty docID")
	}
	if text == "" {
		return nil
	}

	existing, err := l.exportText(ctx, docID)
	if err != nil {
		return err
	}
	combined := existing + text

	bodyFn := func() (io.Reader, string) {
		return strings.NewReader(combined), mimeText
	}
	u := driveUploadFilesURL + "/" + url.PathEscape(docID) + "?" + url.Values{
		"uploadType": {"media"},
		"fields":     {"id,mimeType"},
	}.Encode()

	return l.doJSON(ctx, http.MethodPatch, u, bodyFn, nil)
}

// exportText reads the current Doc content as plain text, stripping the leading
// UTF-8 BOM that Drive's export always prepends.
func (l *DriveDocLibrary) exportText(ctx context.Context, docID string) (string, error) {
	u := driveFilesURL + "/" + url.PathEscape(docID) + "/export?" + url.Values{
		"mimeType": {mimeText},
	}.Encode()
	body, err := l.doRaw(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(string(body), utf8BOM), nil
}

// driveQuote wraps a value in single quotes for a Drive `q` clause, escaping
// embedded backslashes and single quotes per Drive's query grammar.
func driveQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	return "'" + s + "'"
}
