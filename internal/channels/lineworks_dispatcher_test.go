package channels

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
)

// fakeLWChannel implements lineWorksWebhookChannel for testing the shared
// /webhook/lineworks dispatcher WITHOUT importing the concrete lineworks
// package (which would create an import cycle: lineworks imports channels).
//
// VerifyAndHandle mirrors the real contract: it verifies HMAC-SHA256 of rawBody
// keyed by this bot's secret against the base64 signature using a constant-time
// compare, and ONLY on a match records the dispatch and returns true. On a
// non-match it returns false with no side effects, so the dispatcher can try the
// next candidate.
type fakeLWChannel struct {
	BaseChannel
	secret string

	mu          sync.Mutex
	dispatched  bool
	dispatchedN int
	lastBody    []byte
}

func newFakeLWChannel(name, secret string) *fakeLWChannel {
	f := &fakeLWChannel{secret: secret}
	f.BaseChannel = BaseChannel{name: name}
	return f
}

func (f *fakeLWChannel) Type() string                                        { return "lineworks" }
func (f *fakeLWChannel) Start(_ context.Context) error                       { return nil }
func (f *fakeLWChannel) Stop(_ context.Context) error                        { return nil }
func (f *fakeLWChannel) IsRunning() bool                                     { return true }
func (f *fakeLWChannel) IsAllowed(_ string) bool                             { return true }
func (f *fakeLWChannel) Send(_ context.Context, _ bus.OutboundMessage) error { return nil }

func (f *fakeLWChannel) VerifyAndHandle(rawBody []byte, sig string) bool {
	if f.secret == "" || sig == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(f.secret))
	mac.Write(rawBody)
	expected := mac.Sum(nil)
	got, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return false
	}
	if !hmac.Equal(got, expected) {
		return false
	}
	// Match: record dispatch. Real channel dispatches asynchronously; we record
	// synchronously so the assertion is race-free.
	f.mu.Lock()
	f.dispatched = true
	f.dispatchedN++
	f.lastBody = append([]byte(nil), rawBody...)
	f.mu.Unlock()
	return true
}

func (f *fakeLWChannel) wasDispatched() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dispatched
}

// signLW produces the X-WORKS-Signature value (standard base64 HMAC-SHA256).
func signLW(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// TestLineWorksDispatcher_RoutesToMatchingBot proves the body signed for bot B is
// routed to B (not A), the dispatcher replies 200, and A sees nothing.
func TestLineWorksDispatcher_RoutesToMatchingBot(t *testing.T) {
	t.Parallel()

	mgr := NewManager(bus.New())
	botA := newFakeLWChannel("lw-a", "secret-AAAA")
	botB := newFakeLWChannel("lw-b", "secret-BBBB")
	mgr.RegisterChannel("lw-a", botA)
	mgr.RegisterChannel("lw-b", botB)

	path, handler, ok := mgr.LineWorksWebhookDispatcher()
	if !ok {
		t.Fatal("dispatcher not available with lineworks channels registered")
	}
	if path != "/webhook/lineworks" {
		t.Fatalf("path = %q, want /webhook/lineworks", path)
	}

	body := []byte(`{"type":"message","source":{"userId":"u1"}}`)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
	req.Header.Set("X-WORKS-Signature", signLW(botB.secret, body))
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !botB.wasDispatched() {
		t.Error("bot B (signing bot) should have been dispatched")
	}
	if botA.wasDispatched() {
		t.Error("bot A (non-matching) must NOT be dispatched")
	}
	if string(botB.lastBody) != string(body) {
		t.Errorf("bot B received body %q, want %q", botB.lastBody, body)
	}
}

// TestLineWorksDispatcher_RejectsUnsignedBody proves a garbage/unsigned body is
// rejected with 401 and NO bot is dispatched (HMAC verified before any dispatch).
func TestLineWorksDispatcher_RejectsUnsignedBody(t *testing.T) {
	t.Parallel()

	mgr := NewManager(bus.New())
	botA := newFakeLWChannel("lw-a", "secret-AAAA")
	botB := newFakeLWChannel("lw-b", "secret-BBBB")
	mgr.RegisterChannel("lw-a", botA)
	mgr.RegisterChannel("lw-b", botB)

	_, handler, ok := mgr.LineWorksWebhookDispatcher()
	if !ok {
		t.Fatal("dispatcher not available")
	}

	body := []byte(`{"type":"message"}`)

	t.Run("garbage signature", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/webhook/lineworks", strings.NewReader(string(body)))
		req.Header.Set("X-WORKS-Signature", base64.StdEncoding.EncodeToString([]byte("not-a-real-mac")))
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rr.Code)
		}
	})

	t.Run("missing signature header", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/webhook/lineworks", strings.NewReader(string(body)))
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rr.Code)
		}
	})

	if botA.wasDispatched() || botB.wasDispatched() {
		t.Error("no bot may be dispatched for an unverified body")
	}
}

// TestLineWorksDispatcher_HotLoadedBotSeen proves a bot registered AFTER the
// dispatcher handler was obtained is still routed to, because the candidate set
// is collected from the live registry on each request (no captured static slice).
func TestLineWorksDispatcher_HotLoadedBotSeen(t *testing.T) {
	t.Parallel()

	mgr := NewManager(bus.New())
	// Start with one bot so the dispatcher mounts.
	botA := newFakeLWChannel("lw-a", "secret-AAAA")
	mgr.RegisterChannel("lw-a", botA)

	_, handler, ok := mgr.LineWorksWebhookDispatcher()
	if !ok {
		t.Fatal("dispatcher not available")
	}

	// Hot-load bot C AFTER the handler closure was created.
	botC := newFakeLWChannel("lw-c", "secret-CCCC")
	mgr.RegisterChannel("lw-c", botC)

	body := []byte(`{"type":"message","hot":"loaded"}`)
	req := httptest.NewRequest(http.MethodPost, "/webhook/lineworks", strings.NewReader(string(body)))
	req.Header.Set("X-WORKS-Signature", signLW(botC.secret, body))
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !botC.wasDispatched() {
		t.Error("hot-loaded bot C must be seen by the live per-request snapshot")
	}
	if botA.wasDispatched() {
		t.Error("bot A must not be dispatched for a body it didn't sign")
	}
}

// TestLineWorksDispatcher_SingleBotUnchanged proves the single-bot path behaves
// exactly as before: one candidate, signed body → 200 + dispatch.
func TestLineWorksDispatcher_SingleBotUnchanged(t *testing.T) {
	t.Parallel()

	mgr := NewManager(bus.New())
	bot := newFakeLWChannel("lw-only", "the-only-secret")
	mgr.RegisterChannel("lw-only", bot)

	_, handler, ok := mgr.LineWorksWebhookDispatcher()
	if !ok {
		t.Fatal("dispatcher not available")
	}

	body := []byte(`{"type":"message","single":true}`)
	req := httptest.NewRequest(http.MethodPost, "/webhook/lineworks", strings.NewReader(string(body)))
	req.Header.Set("X-WORKS-Signature", signLW(bot.secret, body))
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !bot.wasDispatched() {
		t.Error("single bot must be dispatched for its own signed body")
	}
}

// TestLineWorksDispatcher_NotMountedWhenAbsent proves the dispatcher reports
// ok=false (so the gateway mounts nothing / 404) when no lineworks bot exists,
// while a non-lineworks channel never spuriously enables it.
func TestLineWorksDispatcher_NotMountedWhenAbsent(t *testing.T) {
	t.Parallel()

	mgr := NewManager(bus.New())
	// A non-lineworks channel must NOT enable the lineworks dispatcher.
	mgr.RegisterChannel("telegram-x", newMockChannel("telegram-x", TypeTelegram))

	if _, _, ok := mgr.LineWorksWebhookDispatcher(); ok {
		t.Fatal("dispatcher must be ok=false when no lineworks channel is registered")
	}
}

// TestLineWorksWebhookHandlers_ExcludesLineWorks proves WebhookHandlers() never
// emits the /webhook/lineworks route (so the gateway mounts that path exactly
// once via the shared dispatcher, avoiding a stdlib ServeMux double-register
// panic), while other webhook channels still flow through unchanged.
func TestLineWorksWebhookHandlers_ExcludesLineWorks(t *testing.T) {
	t.Parallel()

	mgr := NewManager(bus.New())
	mgr.RegisterChannel("lw-a", newFakeLWChannel("lw-a", "secret-AAAA"))
	mgr.RegisterChannel("lw-b", newFakeLWChannel("lw-b", "secret-BBBB"))

	for _, route := range mgr.WebhookHandlers() {
		if route.Path == "/webhook/lineworks" {
			t.Fatalf("WebhookHandlers() emitted /webhook/lineworks; it must be excluded so the path mounts exactly once")
		}
	}
}
