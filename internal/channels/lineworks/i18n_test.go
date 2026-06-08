package lineworks

import (
	"context"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// --- localize ---

func TestLocalize_Hit(t *testing.T) {
	if got := localize(langZH, keyResetDone); got != "已重設對話紀錄。" {
		t.Fatalf("zh_TW keyResetDone = %q, want the Traditional Chinese reset ack", got)
	}
	if got := localize(langVI, keyStopOne); got != commandStrings[langVI][keyStopOne] {
		t.Fatalf("vi_VN keyStopOne = %q, want the Vietnamese value", got)
	}
}

func TestLocalize_LangFallbackToEnglish(t *testing.T) {
	// An unknown language falls back to the English text.
	if got := localize("ja_JP", keyResetDone); got != commandStrings[langEN][keyResetDone] {
		t.Fatalf("unknown lang did not fall back to English: got %q", got)
	}
}

func TestLocalize_KeyMissingReturnsKey(t *testing.T) {
	const missing = "no.such.key"
	if got := localize(langEN, missing); got != missing {
		t.Fatalf("missing key should return the key itself, got %q", got)
	}
	// Missing in a known non-English lang AND missing in English → key.
	if got := localize(langZH, missing); got != missing {
		t.Fatalf("missing key (zh) should return the key itself, got %q", got)
	}
}

func TestLocalize_ArgsApplied(t *testing.T) {
	got := localize(langEN, keyWriterAdded, "userX")
	want := "Added userX as a file writer."
	if got != want {
		t.Fatalf("localize with args = %q, want %q", got, want)
	}
	// No args → the format string is returned verbatim (no Sprintf).
	if got := localize(langEN, keyWriterAdded); got != commandStrings[langEN][keyWriterAdded] {
		t.Fatalf("localize without args should not run Sprintf, got %q", got)
	}
}

// --- normalizeLang ---

func TestNormalizeLang_Table(t *testing.T) {
	cases := map[string]string{
		"en_US":     langEN,
		"en":        langEN,
		"zh_TW":     langZH,
		"vi_VN":     langVI,
		"":          langEN,
		"fr_FR":     langEN,
		"zh_CN":     langEN, // Simplified Chinese is not a supported command lang
		"ZH_TW":     langEN, // case-sensitive by design (Odoo emits canonical codes)
		"vi":        langEN,
	}
	for in, want := range cases {
		if got := normalizeLang(in); got != want {
			t.Errorf("normalizeLang(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- key-set parity across languages ---

func TestCommandStrings_AllLangsShareKeySet(t *testing.T) {
	en := commandStrings[langEN]
	if len(en) == 0 {
		t.Fatal("English key set is empty")
	}
	for _, lang := range supportedLangs() {
		if lang == langEN {
			continue
		}
		m := commandStrings[lang]
		// Every English key must exist in the other language.
		for k := range en {
			if _, ok := m[k]; !ok {
				t.Errorf("lang %q is missing key %q", lang, k)
			}
		}
		// And the other language must not define extra keys.
		for k := range m {
			if _, ok := en[k]; !ok {
				t.Errorf("lang %q has extra key %q not in English", lang, k)
			}
		}
	}
}

// --- resolveUserLang ---

// fakeCredsStore implements credentialLangStore. It returns a preset credential
// (and records writes) so resolveUserLang can be exercised without a DB.
type fakeCredsStore struct {
	creds *store.MCPUserCredentials
	err   error

	getCalls  int
	gotTenant uuid.UUID // tenant resolved from the ctx of the last GetUserCredentials
	written   *store.MCPUserCredentials
}

func (f *fakeCredsStore) GetUserCredentials(ctx context.Context, _ uuid.UUID, _ string) (*store.MCPUserCredentials, error) {
	f.getCalls++
	f.gotTenant = store.TenantIDFromContext(ctx)
	if f.err != nil {
		return nil, f.err
	}
	return f.creds, nil
}

func (f *fakeCredsStore) SetUserCredentials(_ context.Context, _ uuid.UUID, _ string, creds store.MCPUserCredentials) error {
	f.written = &creds
	return nil
}

func newLangChannel(t *testing.T) *Channel {
	t.Helper()
	mb := bus.New()
	t.Cleanup(mb.Close)
	c := New(nil, Config{DMPolicy: "open", GroupPolicy: "open"}, mb)
	c.SetName("lineworks")
	c.SetTenantID(uuid.New())
	return c
}

func TestResolveUserLang_EnvHit(t *testing.T) {
	c := newLangChannel(t)
	c.SetCredsStore(&fakeCredsStore{
		creds: &store.MCPUserCredentials{APIKey: "k", Env: map[string]string{"odoo_lang": "zh_TW"}},
	})
	c.SetMCPServerID(uuid.New())

	if got := c.resolveUserLang(context.Background(), "userA"); got != langZH {
		t.Fatalf("resolveUserLang env hit = %q, want %q", got, langZH)
	}
}

func TestResolveUserLang_NoCredsStoreReturnsEnglish(t *testing.T) {
	c := newLangChannel(t)
	// No creds store wired.
	if got := c.resolveUserLang(context.Background(), "userA"); got != langEN {
		t.Fatalf("resolveUserLang without creds store = %q, want %q", got, langEN)
	}
}

func TestResolveUserLang_NoServerIDReturnsEnglish(t *testing.T) {
	c := newLangChannel(t)
	c.SetCredsStore(&fakeCredsStore{
		creds: &store.MCPUserCredentials{APIKey: "k", Env: map[string]string{"odoo_lang": "zh_TW"}},
	})
	// mcpServerID left zero → fall back to English.
	if got := c.resolveUserLang(context.Background(), "userA"); got != langEN {
		t.Fatalf("resolveUserLang with zero server id = %q, want %q", got, langEN)
	}
}

func TestResolveUserLang_UnknownLangNormalizesToEnglish(t *testing.T) {
	c := newLangChannel(t)
	c.SetCredsStore(&fakeCredsStore{
		creds: &store.MCPUserCredentials{APIKey: "k", Env: map[string]string{"odoo_lang": "fr_FR"}},
	})
	c.SetMCPServerID(uuid.New())
	if got := c.resolveUserLang(context.Background(), "userA"); got != langEN {
		t.Fatalf("resolveUserLang unknown stored lang = %q, want %q", got, langEN)
	}
}

func TestResolveUserLang_NoStoredLangReturnsEnglish(t *testing.T) {
	c := newLangChannel(t)
	c.SetCredsStore(&fakeCredsStore{
		creds: &store.MCPUserCredentials{APIKey: "k"}, // no Env
	})
	c.SetMCPServerID(uuid.New())
	if got := c.resolveUserLang(context.Background(), "userA"); got != langEN {
		t.Fatalf("resolveUserLang with no stored lang = %q, want %q", got, langEN)
	}
}

func TestResolveUserLang_CacheReuse(t *testing.T) {
	c := newLangChannel(t)
	fake := &fakeCredsStore{
		creds: &store.MCPUserCredentials{APIKey: "k", Env: map[string]string{"odoo_lang": "vi_VN"}},
	}
	c.SetCredsStore(fake)
	c.SetMCPServerID(uuid.New())

	if got := c.resolveUserLang(context.Background(), "userA"); got != langVI {
		t.Fatalf("first resolve = %q, want %q", got, langVI)
	}
	if got := c.resolveUserLang(context.Background(), "userA"); got != langVI {
		t.Fatalf("second resolve = %q, want %q", got, langVI)
	}
	// The second call must be served from cache (only one store read total).
	if fake.getCalls != 1 {
		t.Fatalf("expected 1 credential read (cache reuse), got %d", fake.getCalls)
	}
}

func TestResolveUserLang_EmptyUserIDReturnsEnglish(t *testing.T) {
	c := newLangChannel(t)
	c.SetCredsStore(&fakeCredsStore{
		creds: &store.MCPUserCredentials{APIKey: "k", Env: map[string]string{"odoo_lang": "zh_TW"}},
	})
	c.SetMCPServerID(uuid.New())
	if got := c.resolveUserLang(context.Background(), ""); got != langEN {
		t.Fatalf("resolveUserLang empty userID = %q, want %q", got, langEN)
	}
}

// A store error must not crash and must fall back to English.
func TestResolveUserLang_StoreErrorReturnsEnglish(t *testing.T) {
	c := newLangChannel(t)
	c.SetCredsStore(&fakeCredsStore{err: errors.New("db down")})
	c.SetMCPServerID(uuid.New())
	if got := c.resolveUserLang(context.Background(), "userA"); got != langEN {
		t.Fatalf("resolveUserLang on store error = %q, want %q", got, langEN)
	}
}

// Tenant alignment (regression lock): autobind writes the credential under
// MasterTenantID, so the lang read MUST resolve under Master too — NOT under the
// channel's own TenantID that the command path injects. The credential read must
// therefore receive a tenant-less context (TenantIDFromContext == Nil → Master),
// even when the caller's ctx carries a non-master channel tenant.
func TestResolveUserLang_ReadsUnderMasterTenant(t *testing.T) {
	c := newLangChannel(t) // newLangChannel sets a random (non-master) tenant
	fake := &fakeCredsStore{
		creds: &store.MCPUserCredentials{APIKey: "k", Env: map[string]string{"odoo_lang": "zh_TW"}},
	}
	c.SetCredsStore(fake)
	c.SetMCPServerID(uuid.New())

	// Mirror commands.go: the command path overrides ctx with the channel tenant.
	ctx := store.WithTenantID(context.Background(), c.TenantID())
	if got := c.resolveUserLang(ctx, "userA"); got != langZH {
		t.Fatalf("resolveUserLang = %q, want %q", got, langZH)
	}
	if fake.gotTenant != uuid.Nil {
		t.Fatalf("credential read used tenant %v, want tenant-less (Nil → Master) to match autobind's write", fake.gotTenant)
	}
}

// Format-verb parity: every parameterized translation MUST carry the same
// ordered set of printf verbs as its English counterpart, otherwise
// localize()'s fmt.Sprintf emits garbage at runtime (e.g. %!s(MISSING)) while
// the key-set parity test still passes. Guards future translation edits.
func TestCommandStrings_FormatVerbParity(t *testing.T) {
	verbRe := regexp.MustCompile(`%[#+\- 0]*\*?\.?[0-9]*[a-zA-Z]`)
	verbs := func(s string) []string {
		return verbRe.FindAllString(strings.ReplaceAll(s, "%%", ""), -1)
	}
	en := commandStrings[langEN]
	for key, enVal := range en {
		enVerbs := verbs(enVal)
		for _, lang := range supportedLangs() {
			if lang == langEN {
				continue
			}
			if got := verbs(commandStrings[lang][key]); !reflect.DeepEqual(got, enVerbs) {
				t.Errorf("key %q lang %q verbs %v != English %v", key, lang, got, enVerbs)
			}
		}
	}
}
