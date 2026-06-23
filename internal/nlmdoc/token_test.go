package nlmdoc

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// fakeRclone records the rclone subcommands invoked and returns canned output
// for `config show`. about always succeeds (the refresh side-effect).
type fakeRclone struct {
	aboutCalls atomic.Int32
	showCalls  atomic.Int32
	showOut    string // returned for `config show`
	aboutErr   error
}

func (f *fakeRclone) run(ctx context.Context, args []string) ([]byte, error) {
	if len(args) >= 1 && args[0] == "about" {
		f.aboutCalls.Add(1)
		if f.aboutErr != nil {
			return nil, f.aboutErr
		}
		return []byte("Total: 5 TiB\n"), nil
	}
	if len(args) >= 2 && args[0] == "config" && args[1] == "show" {
		f.showCalls.Add(1)
		return []byte(f.showOut), nil
	}
	return nil, fmt.Errorf("unexpected rclone args: %v", args)
}

func showOutput(accessToken string, expiry time.Time) string {
	return "[gdrive]\n" +
		"type = drive\n" +
		"scope = drive\n" +
		`token = {"access_token":"` + accessToken + `","refresh_token":"rt","token_type":"Bearer","expiry":"` +
		expiry.Format(time.RFC3339) + `"}` + "\n"
}

func newTestSource(f *fakeRclone) *RcloneTokenSource {
	s := NewRcloneTokenSource("gdrive")
	s.runner = f.run
	return s
}

func TestRcloneTokenSource_RefreshAndCache(t *testing.T) {
	exp := time.Now().Add(time.Hour)
	f := &fakeRclone{showOut: showOutput("tok-abc", exp)}
	s := newTestSource(f)

	tok, err := s.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "tok-abc" {
		t.Fatalf("got token %q, want tok-abc", tok)
	}
	if f.aboutCalls.Load() != 1 {
		t.Fatalf("about called %d times, want 1 (forced refresh)", f.aboutCalls.Load())
	}
	if f.showCalls.Load() != 1 {
		t.Fatalf("config show called %d times, want 1", f.showCalls.Load())
	}

	// Second call within validity must be served from cache (no new rclone).
	tok2, err := s.Token(context.Background())
	if err != nil {
		t.Fatalf("Token (cached): %v", err)
	}
	if tok2 != "tok-abc" {
		t.Fatalf("cached token %q, want tok-abc", tok2)
	}
	if f.aboutCalls.Load() != 1 || f.showCalls.Load() != 1 {
		t.Fatalf("cache miss: about=%d show=%d, want 1/1", f.aboutCalls.Load(), f.showCalls.Load())
	}
}

func TestRcloneTokenSource_ReBrokerNearExpiry(t *testing.T) {
	// Expiry inside the skew window → must re-broker on every Token call.
	nearExp := time.Now().Add(tokenExpirySkew / 2)
	f := &fakeRclone{showOut: showOutput("tok-1", nearExp)}
	s := newTestSource(f)

	if _, err := s.Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}
	// Update the canned token so a re-broker yields a different value.
	f.showOut = showOutput("tok-2", time.Now().Add(time.Hour))
	tok, err := s.Token(context.Background())
	if err != nil {
		t.Fatalf("Token (re-broker): %v", err)
	}
	if tok != "tok-2" {
		t.Fatalf("got %q after re-broker, want tok-2", tok)
	}
	if f.aboutCalls.Load() != 2 {
		t.Fatalf("about called %d times, want 2 (re-broker)", f.aboutCalls.Load())
	}
}

func TestRcloneTokenSource_Invalidate(t *testing.T) {
	exp := time.Now().Add(time.Hour)
	f := &fakeRclone{showOut: showOutput("tok-x", exp)}
	s := newTestSource(f)

	if _, err := s.Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}
	s.Invalidate()
	f.showOut = showOutput("tok-y", exp)
	tok, err := s.Token(context.Background())
	if err != nil {
		t.Fatalf("Token after invalidate: %v", err)
	}
	if tok != "tok-y" {
		t.Fatalf("got %q, want tok-y after invalidate", tok)
	}
	if f.aboutCalls.Load() != 2 {
		t.Fatalf("about called %d times, want 2", f.aboutCalls.Load())
	}
}

func TestRcloneTokenSource_AboutError(t *testing.T) {
	f := &fakeRclone{aboutErr: fmt.Errorf("remote dead")}
	s := newTestSource(f)
	if _, err := s.Token(context.Background()); err == nil {
		t.Fatal("expected error when rclone about fails")
	}
}

func TestParseRcloneToken(t *testing.T) {
	exp := time.Date(2026, 6, 22, 9, 10, 38, 0, time.UTC)
	out := showOutput("aaa.bbb.ccc", exp)
	tok, err := parseRcloneToken([]byte(out))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if tok.AccessToken != "aaa.bbb.ccc" {
		t.Fatalf("access_token = %q", tok.AccessToken)
	}
	if tok.RefreshToken != "rt" {
		t.Fatalf("refresh_token = %q", tok.RefreshToken)
	}
	if !tok.Expiry.Equal(exp) {
		t.Fatalf("expiry = %v, want %v", tok.Expiry, exp)
	}
}

func TestParseRcloneToken_NoTokenLine(t *testing.T) {
	if _, err := parseRcloneToken([]byte("[gdrive]\ntype = drive\n")); err == nil {
		t.Fatal("expected error when no token line present")
	}
}

func TestParseRcloneToken_NotJSON(t *testing.T) {
	if _, err := parseRcloneToken([]byte("token = not-json\n")); err == nil {
		t.Fatal("expected error when token value is not JSON")
	}
}
