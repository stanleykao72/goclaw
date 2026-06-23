package store

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestParseMemoryBackend(t *testing.T) {
	cases := []struct {
		name        string
		otherConfig string
		want        string
	}{
		{"unset defaults to db", "", "db"},
		{"empty object defaults to db", `{}`, "db"},
		{"explicit vault", `{"memory_backend":"vault"}`, "vault"},
		{"explicit db", `{"memory_backend":"db"}`, "db"},
		{"unknown value falls back to db", `{"memory_backend":"sqlite"}`, "db"},
		{"wrong type falls back to db", `{"memory_backend":123}`, "db"},
		{"malformed json falls back to db", `{not json`, "db"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ag := &AgentData{}
			if c.otherConfig != "" {
				ag.OtherConfig = json.RawMessage(c.otherConfig)
			}
			if got := ag.ParseMemoryBackend(); got != c.want {
				t.Fatalf("ParseMemoryBackend() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestParseMemoryMode(t *testing.T) {
	cases := []struct {
		name        string
		otherConfig string
		want        string
	}{
		{"unset defaults to both", "", "both"},
		{"empty object defaults to both", `{}`, "both"},
		{"explicit notebook", `{"memory_mode":"notebook"}`, "notebook"},
		{"explicit vault", `{"memory_mode":"vault"}`, "vault"},
		{"explicit both", `{"memory_mode":"both"}`, "both"},
		{"unknown value falls back to both", `{"memory_mode":"nlm"}`, "both"},
		{"wrong type falls back to both", `{"memory_mode":123}`, "both"},
		{"malformed json falls back to both", `{not json`, "both"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ag := &AgentData{}
			if c.otherConfig != "" {
				ag.OtherConfig = json.RawMessage(c.otherConfig)
			}
			if got := ag.ParseMemoryMode(); got != c.want {
				t.Fatalf("ParseMemoryMode() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestParseMemory(t *testing.T) {
	cases := []struct {
		name        string
		otherConfig string
		wantMode    string
		wantBackend string
	}{
		// ── nothing set → default both-db ──────────────────────────────
		{"unset defaults to both-db", "", "both", "db"},
		{"empty object defaults to both-db", `{}`, "both", "db"},
		{"unrelated keys default to both-db", `{"prompt_mode":"full"}`, "both", "db"},

		// ── public single field: all 5 values decompose correctly ──────
		{"memory notebook", `{"memory":"notebook"}`, "notebook", "db"},
		{"memory vault", `{"memory":"vault"}`, "vault", "vault"},
		{"memory db", `{"memory":"db"}`, "vault", "db"},
		{"memory both-vault", `{"memory":"both-vault"}`, "both", "vault"},
		{"memory both-db", `{"memory":"both-db"}`, "both", "db"},

		// ── public field wins over legacy keys when present + valid ─────
		{
			"memory wins over legacy",
			`{"memory":"vault","memory_mode":"notebook","memory_backend":"db"}`,
			"vault", "vault",
		},

		// ── invalid public field → fall through to legacy on same bag ───
		{
			"garbage memory falls back to legacy",
			`{"memory":"nonsense","memory_mode":"notebook","memory_backend":"db"}`,
			"notebook", "db",
		},
		{
			"wrong-type memory falls back to legacy",
			`{"memory":123,"memory_mode":"vault","memory_backend":"vault"}`,
			"vault", "vault",
		},
		{
			"garbage memory with no legacy → default both-db",
			`{"memory":"nonsense"}`,
			"both", "db",
		},

		// ── legacy fallback: every combo honored when memory unset ─────
		{"legacy mode notebook only", `{"memory_mode":"notebook"}`, "notebook", "db"},
		{"legacy mode vault only", `{"memory_mode":"vault"}`, "vault", "db"},
		{"legacy mode both only", `{"memory_mode":"both"}`, "both", "db"},
		{"legacy backend vault only (no mode → both)", `{"memory_backend":"vault"}`, "both", "vault"},
		{"legacy backend db only (no mode → both)", `{"memory_backend":"db"}`, "both", "db"},
		{
			"legacy both keys vault+vault",
			`{"memory_mode":"vault","memory_backend":"vault"}`,
			"vault", "vault",
		},
		{
			"legacy invalid values default each axis",
			`{"memory_mode":"nlm","memory_backend":"sqlite"}`,
			"both", "db",
		},

		// ── live shapes (PRE-migration legacy keys, read verbatim per axis;
		//    the notebook→db fold happens at MIGRATION time, not in the legacy
		//    fallback, so backend=vault is honored here exactly as stored) ─────
		{
			"esmith-general PRE-migrate: legacy notebook + vault backend → verbatim",
			`{"memory_mode":"notebook","memory_backend":"vault"}`,
			"notebook", "vault",
		},
		{
			"e-smith-hub PRE-migrate: legacy backend vault, no mode → both,vault",
			`{"memory_backend":"vault"}`,
			"both", "vault",
		},
		// ── live shapes (POST-migration single memory field) ───────────
		{
			"esmith-general POST-migrate: memory notebook → notebook,db (backend folds)",
			`{"memory":"notebook"}`,
			"notebook", "db",
		},
		{
			"e-smith-hub POST-migrate: memory both-vault → both,vault",
			`{"memory":"both-vault"}`,
			"both", "vault",
		},

		// ── malformed json → default both-db ───────────────────────────
		{"malformed json defaults to both-db", `{not json`, "both", "db"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ag := &AgentData{}
			if c.otherConfig != "" {
				ag.OtherConfig = json.RawMessage(c.otherConfig)
			}
			got := ag.ParseMemory()
			if got.Mode != c.wantMode || got.Backend != c.wantBackend {
				t.Fatalf("ParseMemory() = {Mode:%q Backend:%q}, want {Mode:%q Backend:%q}",
					got.Mode, got.Backend, c.wantMode, c.wantBackend)
			}
			// Derived methods must agree with the pair (callers depend on this).
			if m := ag.ParseMemoryMode(); m != c.wantMode {
				t.Fatalf("ParseMemoryMode() = %q, want %q", m, c.wantMode)
			}
			if b := ag.ParseMemoryBackend(); b != c.wantBackend {
				t.Fatalf("ParseMemoryBackend() = %q, want %q", b, c.wantBackend)
			}
		})
	}
}

// deriveMemoryFromLegacy mirrors the CASE expression in
// migrations/000083_merge_memory_config.up.sql. The migration is a pure SQL
// JSONB data migration that cannot run inside this unit-test package, so this
// Go mirror pins the (mode, backend) → public-memory mapping and asserts it
// round-trips through ParseMemory (the migration SQL is verified by mirror).
func deriveMemoryFromLegacy(mode, backend string) string {
	switch {
	case mode == "notebook":
		return "notebook"
	case mode == "vault" && backend == "vault":
		return "vault"
	case mode == "vault" && backend == "db":
		return "db"
	case mode == "both" && backend == "vault":
		return "both-vault"
	default: // (both, db) and the ELSE catch-all
		return "both-db"
	}
}

func TestMergeMemoryMigrationMappingMirror(t *testing.T) {
	// UP CASE mapping (legacy mode/backend coalesced to both/db) → public memory.
	cases := []struct {
		mode, backend string
		want          string
	}{
		{"notebook", "db", "notebook"},
		{"notebook", "vault", "notebook"}, // mode wins
		{"vault", "vault", "vault"},
		{"vault", "db", "db"},
		{"both", "vault", "both-vault"},
		{"both", "db", "both-db"},
		// coalesce defaults: missing mode→both, missing backend→db
		{"both", "db", "both-db"},
		// live shapes
		{"notebook", "vault", "notebook"}, // esmith-general
		{"both", "vault", "both-vault"},   // e-smith-hub (no mode → both)
	}
	for _, c := range cases {
		got := deriveMemoryFromLegacy(c.mode, c.backend)
		if got != c.want {
			t.Fatalf("deriveMemoryFromLegacy(%q,%q) = %q, want %q", c.mode, c.backend, got, c.want)
		}
		// The derived public value must round-trip back through ParseMemory to
		// the SAME (mode, backend) pair — EXCEPT for the notebook asymmetry where
		// any backend folds to db (backend inert when mode=notebook).
		ag := &AgentData{OtherConfig: json.RawMessage(`{"memory":"` + got + `"}`)}
		pair := ag.ParseMemory()
		wantMode, wantBackend := c.mode, c.backend
		if c.mode == "notebook" {
			wantBackend = "db" // documented asymmetry
		}
		if pair.Mode != wantMode || pair.Backend != wantBackend {
			t.Fatalf("round-trip memory=%q → {Mode:%q Backend:%q}, want {Mode:%q Backend:%q}",
				got, pair.Mode, pair.Backend, wantMode, wantBackend)
		}
	}
}

func TestParseReasoningConfigDefaultsToOff(t *testing.T) {
	agent := &AgentData{}

	got := agent.ParseReasoningConfig()
	if got.OverrideMode != ReasoningOverrideInherit {
		t.Fatalf("OverrideMode = %q, want %q", got.OverrideMode, ReasoningOverrideInherit)
	}
	if got.Effort != "off" {
		t.Fatalf("Effort = %q, want off", got.Effort)
	}
	if got.Fallback != ReasoningFallbackDowngrade {
		t.Fatalf("Fallback = %q, want %q", got.Fallback, ReasoningFallbackDowngrade)
	}
	if got.Source != ReasoningSourceUnset {
		t.Fatalf("Source = %q, want %q", got.Source, ReasoningSourceUnset)
	}
}

func TestParseReasoningConfigUsesLegacyThinkingLevel(t *testing.T) {
	agent := &AgentData{
		ThinkingLevel: "medium",
	}

	got := agent.ParseReasoningConfig()
	if got.Effort != "medium" {
		t.Fatalf("Effort = %q, want medium", got.Effort)
	}
	if got.OverrideMode != ReasoningOverrideCustom {
		t.Fatalf("OverrideMode = %q, want %q", got.OverrideMode, ReasoningOverrideCustom)
	}
	if got.Source != ReasoningSourceLegacy {
		t.Fatalf("Source = %q, want %q", got.Source, ReasoningSourceLegacy)
	}
}

func TestParseReasoningConfigPrefersAdvancedSettings(t *testing.T) {
	agent := &AgentData{
		ThinkingLevel:   "high",
		ReasoningConfig: json.RawMessage(`{"effort": "xhigh", "fallback": "provider_default"}`),
	}

	got := agent.ParseReasoningConfig()
	if got.Effort != "xhigh" {
		t.Fatalf("Effort = %q, want xhigh", got.Effort)
	}
	if got.OverrideMode != ReasoningOverrideCustom {
		t.Fatalf("OverrideMode = %q, want %q", got.OverrideMode, ReasoningOverrideCustom)
	}
	if got.Fallback != ReasoningFallbackProviderDefault {
		t.Fatalf("Fallback = %q, want %q", got.Fallback, ReasoningFallbackProviderDefault)
	}
	if got.Source != ReasoningSourceAdvanced {
		t.Fatalf("Source = %q, want %q", got.Source, ReasoningSourceAdvanced)
	}
}

func TestParseReasoningConfigKeepsLegacyEffortWhenAdvancedOnlySetsFallback(t *testing.T) {
	agent := &AgentData{
		ThinkingLevel:   "medium",
		ReasoningConfig: json.RawMessage(`{"fallback": "off"}`),
	}

	got := agent.ParseReasoningConfig()
	if got.Effort != "medium" {
		t.Fatalf("Effort = %q, want medium", got.Effort)
	}
	if got.Fallback != ReasoningFallbackDisable {
		t.Fatalf("Fallback = %q, want %q", got.Fallback, ReasoningFallbackDisable)
	}
}

func TestParseReasoningConfigPreservesExplicitInherit(t *testing.T) {
	agent := &AgentData{
		ThinkingLevel:   "high",
		ReasoningConfig: json.RawMessage(`{"override_mode": "inherit"}`),
	}

	got := agent.ParseReasoningConfig()
	if got.OverrideMode != ReasoningOverrideInherit {
		t.Fatalf("OverrideMode = %q, want %q", got.OverrideMode, ReasoningOverrideInherit)
	}
	if got.Effort != "off" {
		t.Fatalf("Effort = %q, want off", got.Effort)
	}
	if got.Source != ReasoningSourceUnset {
		t.Fatalf("Source = %q, want %q", got.Source, ReasoningSourceUnset)
	}
}

func TestParseProviderReasoningConfigNormalizesDefaults(t *testing.T) {
	settings := json.RawMessage(`{
		"reasoning_defaults": {"effort": " xhigh ", "fallback": "provider_default"}
	}`)

	got := ParseProviderReasoningConfig(settings)
	if got == nil {
		t.Fatal("ParseProviderReasoningConfig() = nil, want config")
	}
	if got.Effort != "xhigh" {
		t.Fatalf("Effort = %q, want xhigh", got.Effort)
	}
	if got.Fallback != ReasoningFallbackProviderDefault {
		t.Fatalf("Fallback = %q, want %q", got.Fallback, ReasoningFallbackProviderDefault)
	}
}

func TestResolveEffectiveReasoningConfigUsesProviderDefaults(t *testing.T) {
	got := ResolveEffectiveReasoningConfig(
		&ProviderReasoningConfig{Effort: "medium", Fallback: ReasoningFallbackDisable},
		AgentReasoningConfig{OverrideMode: ReasoningOverrideInherit},
	)

	if got.OverrideMode != ReasoningOverrideInherit {
		t.Fatalf("OverrideMode = %q, want %q", got.OverrideMode, ReasoningOverrideInherit)
	}
	if got.Effort != "medium" {
		t.Fatalf("Effort = %q, want medium", got.Effort)
	}
	if got.Fallback != ReasoningFallbackDisable {
		t.Fatalf("Fallback = %q, want %q", got.Fallback, ReasoningFallbackDisable)
	}
	if got.Source != ReasoningSourceProviderDefault {
		t.Fatalf("Source = %q, want %q", got.Source, ReasoningSourceProviderDefault)
	}
}

func TestResolveEffectiveReasoningConfigPreservesCustomAgentReasoning(t *testing.T) {
	got := ResolveEffectiveReasoningConfig(
		&ProviderReasoningConfig{Effort: "medium", Fallback: ReasoningFallbackDisable},
		AgentReasoningConfig{
			OverrideMode: ReasoningOverrideCustom,
			Effort:       "xhigh",
			Fallback:     ReasoningFallbackProviderDefault,
			Source:       ReasoningSourceAdvanced,
		},
	)

	if got.OverrideMode != ReasoningOverrideCustom {
		t.Fatalf("OverrideMode = %q, want %q", got.OverrideMode, ReasoningOverrideCustom)
	}
	if got.Effort != "xhigh" {
		t.Fatalf("Effort = %q, want xhigh", got.Effort)
	}
	if got.Source != ReasoningSourceAdvanced {
		t.Fatalf("Source = %q, want %q", got.Source, ReasoningSourceAdvanced)
	}
}

func TestParseChatGPTOAuthRoutingNormalizesNames(t *testing.T) {
	agent := &AgentData{
		ChatGPTOAuthRouting: json.RawMessage(`{
			"strategy": "round_robin",
			"extra_provider_names": [" openai-codex-backup ", "", "openai-codex-backup", "openai-codex-team"]
		}`),
	}

	got := agent.ParseChatGPTOAuthRouting()
	if got == nil {
		t.Fatal("ParseChatGPTOAuthRouting() = nil, want config")
	}
	if got.Strategy != ChatGPTOAuthStrategyRoundRobin {
		t.Fatalf("Strategy = %q, want %q", got.Strategy, ChatGPTOAuthStrategyRoundRobin)
	}
	if got.OverrideMode != ChatGPTOAuthOverrideCustom {
		t.Fatalf("OverrideMode = %q, want %q", got.OverrideMode, ChatGPTOAuthOverrideCustom)
	}

	wantExtras := []string{"openai-codex-backup", "openai-codex-team"}
	if !reflect.DeepEqual(got.ExtraProviderNames, wantExtras) {
		t.Fatalf("ExtraProviderNames = %#v, want %#v", got.ExtraProviderNames, wantExtras)
	}
}

func TestPublicChatGPTOAuthRoutingMigratesLegacyStrategiesToPriorityOrder(t *testing.T) {
	for _, tc := range []struct {
		name     string
		strategy string
	}{
		{name: "unknown", strategy: "something_else"},
		{name: "manual", strategy: "manual"},
		{name: "primary_first", strategy: "primary_first"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := &AgentData{
				ChatGPTOAuthRouting: json.RawMessage(`{
					"strategy": "` + tc.strategy + `",
					"extra_provider_names": ["openai-codex-backup"]
				}`),
			}

			got := agent.ParseChatGPTOAuthRouting()
			if got == nil {
				t.Fatal("ParseChatGPTOAuthRouting() = nil, want config")
			}
			public := PublicChatGPTOAuthRouting(got)
			if public == nil {
				t.Fatal("PublicChatGPTOAuthRouting() = nil, want config")
			}
			if public.Strategy != ChatGPTOAuthStrategyPriority {
				t.Fatalf("Strategy = %q, want %q", public.Strategy, ChatGPTOAuthStrategyPriority)
			}
		})
	}
}

func TestPublicChatGPTOAuthRoutingCanonicalizesSingleAccountOverrideToPriorityOrder(t *testing.T) {
	agent := &AgentData{
		ChatGPTOAuthRouting: json.RawMessage(`{
			"strategy": "manual",
			"extra_provider_names": []
		}`),
	}

	got := agent.ParseChatGPTOAuthRouting()
	if got == nil {
		t.Fatal("ParseChatGPTOAuthRouting() = nil, want config")
	}
	if got.OverrideMode != ChatGPTOAuthOverrideCustom {
		t.Fatalf("OverrideMode = %q, want %q", got.OverrideMode, ChatGPTOAuthOverrideCustom)
	}
	public := PublicChatGPTOAuthRouting(got)
	if public == nil {
		t.Fatal("PublicChatGPTOAuthRouting() = nil, want config")
	}
	if public.Strategy != ChatGPTOAuthStrategyPriority {
		t.Fatalf("Strategy = %q, want %q", public.Strategy, ChatGPTOAuthStrategyPriority)
	}
	if got.ExtraProviderNames == nil {
		t.Fatal("ExtraProviderNames = nil, want explicit empty slice preserved")
	}
}

func TestParseChatGPTOAuthRoutingPreservesExplicitInheritMode(t *testing.T) {
	agent := &AgentData{
		ChatGPTOAuthRouting: json.RawMessage(`{
			"override_mode": "inherit"
		}`),
	}

	got := agent.ParseChatGPTOAuthRouting()
	if got == nil {
		t.Fatal("ParseChatGPTOAuthRouting() = nil, want config")
	}
	if got.OverrideMode != ChatGPTOAuthOverrideInherit {
		t.Fatalf("OverrideMode = %q, want %q", got.OverrideMode, ChatGPTOAuthOverrideInherit)
	}
	public := PublicChatGPTOAuthRouting(got)
	if public == nil {
		t.Fatal("PublicChatGPTOAuthRouting() = nil, want config")
	}
	if public.Strategy != ChatGPTOAuthStrategyPriority {
		t.Fatalf("Strategy = %q, want %q", public.Strategy, ChatGPTOAuthStrategyPriority)
	}
}

func TestResolveEffectiveChatGPTOAuthRoutingUsesProviderDefaultsWhenAgentUnset(t *testing.T) {
	defaults := &ChatGPTOAuthRoutingConfig{
		Strategy:           ChatGPTOAuthStrategyRoundRobin,
		ExtraProviderNames: []string{"codex-work"},
	}

	got := ResolveEffectiveChatGPTOAuthRouting(defaults, nil)
	if got == nil {
		t.Fatal("ResolveEffectiveChatGPTOAuthRouting() = nil, want config")
	}
	if got.Strategy != ChatGPTOAuthStrategyRoundRobin {
		t.Fatalf("Strategy = %q, want %q", got.Strategy, ChatGPTOAuthStrategyRoundRobin)
	}
	if !reflect.DeepEqual(got.ExtraProviderNames, []string{"codex-work"}) {
		t.Fatalf("ExtraProviderNames = %#v, want %#v", got.ExtraProviderNames, []string{"codex-work"})
	}
}

func TestResolveEffectiveChatGPTOAuthRoutingAllowsInheritWithoutSavedProviderPool(t *testing.T) {
	override := &ChatGPTOAuthRoutingConfig{
		OverrideMode: ChatGPTOAuthOverrideInherit,
	}

	got := ResolveEffectiveChatGPTOAuthRouting(nil, override)
	if got != nil {
		t.Fatalf("ResolveEffectiveChatGPTOAuthRouting() = %#v, want nil", got)
	}
}

func TestResolveEffectiveChatGPTOAuthRoutingInheritForwardsProviderDefaults(t *testing.T) {
	defaults := &ChatGPTOAuthRoutingConfig{
		Strategy:           ChatGPTOAuthStrategyRoundRobin,
		ExtraProviderNames: []string{"codex-work"},
	}
	override := &ChatGPTOAuthRoutingConfig{
		OverrideMode: ChatGPTOAuthOverrideInherit,
	}

	got := ResolveEffectiveChatGPTOAuthRouting(defaults, override)
	if got == nil {
		t.Fatal("ResolveEffectiveChatGPTOAuthRouting() = nil, want config forwarding provider defaults")
	}
	if got.Strategy != ChatGPTOAuthStrategyRoundRobin {
		t.Fatalf("Strategy = %q, want %q", got.Strategy, ChatGPTOAuthStrategyRoundRobin)
	}
	if !reflect.DeepEqual(got.ExtraProviderNames, []string{"codex-work"}) {
		t.Fatalf("ExtraProviderNames = %#v, want provider defaults", got.ExtraProviderNames)
	}
}

func TestResolveEffectiveChatGPTOAuthRoutingAllowsCustomSingleAccountToDisableDefaults(t *testing.T) {
	defaults := &ChatGPTOAuthRoutingConfig{
		Strategy:           ChatGPTOAuthStrategyRoundRobin,
		ExtraProviderNames: []string{"codex-work"},
	}
	override := &ChatGPTOAuthRoutingConfig{
		OverrideMode:       ChatGPTOAuthOverrideCustom,
		Strategy:           ChatGPTOAuthStrategyPriority,
		ExtraProviderNames: []string{},
	}

	got := ResolveEffectiveChatGPTOAuthRouting(defaults, override)
	if got == nil {
		t.Fatal("ResolveEffectiveChatGPTOAuthRouting() = nil, want config")
	}
	if got.Strategy != ChatGPTOAuthStrategyPriority {
		t.Fatalf("Strategy = %q, want %q", got.Strategy, ChatGPTOAuthStrategyPriority)
	}
	if len(got.ExtraProviderNames) != 0 {
		t.Fatalf("ExtraProviderNames = %#v, want empty", got.ExtraProviderNames)
	}
}

func TestResolveEffectiveChatGPTOAuthRoutingRoundRobinEmptyExtrasKeepsDefaults(t *testing.T) {
	defaults := &ChatGPTOAuthRoutingConfig{
		Strategy:           ChatGPTOAuthStrategyRoundRobin,
		ExtraProviderNames: []string{"codex-work"},
	}
	override := &ChatGPTOAuthRoutingConfig{
		OverrideMode:       ChatGPTOAuthOverrideCustom,
		Strategy:           ChatGPTOAuthStrategyRoundRobin,
		ExtraProviderNames: []string{},
	}

	got := ResolveEffectiveChatGPTOAuthRouting(defaults, override)
	if got == nil {
		t.Fatal("ResolveEffectiveChatGPTOAuthRouting() = nil, want config")
	}
	if !reflect.DeepEqual(got.ExtraProviderNames, defaults.ExtraProviderNames) {
		t.Fatalf("ExtraProviderNames = %#v, want %#v", got.ExtraProviderNames, defaults.ExtraProviderNames)
	}
}

func TestResolveEffectiveChatGPTOAuthRoutingKeepsProviderOwnedMembersForStrategyOverride(t *testing.T) {
	defaults := &ChatGPTOAuthRoutingConfig{
		Strategy:           ChatGPTOAuthStrategyRoundRobin,
		ExtraProviderNames: []string{"codex-work", "codex-team"},
	}
	override := &ChatGPTOAuthRoutingConfig{
		OverrideMode: ChatGPTOAuthOverrideCustom,
		Strategy:     ChatGPTOAuthStrategyPriority,
	}

	got := ResolveEffectiveChatGPTOAuthRouting(defaults, override)
	if got == nil {
		t.Fatal("ResolveEffectiveChatGPTOAuthRouting() = nil, want config")
	}
	if got.Strategy != ChatGPTOAuthStrategyPriority {
		t.Fatalf("Strategy = %q, want %q", got.Strategy, ChatGPTOAuthStrategyPriority)
	}
	if !reflect.DeepEqual(got.ExtraProviderNames, defaults.ExtraProviderNames) {
		t.Fatalf("ExtraProviderNames = %#v, want %#v", got.ExtraProviderNames, defaults.ExtraProviderNames)
	}
}

func TestResolveEffectiveChatGPTOAuthRoutingIgnoresCustomMembersWhenProviderOwnsPool(t *testing.T) {
	defaults := &ChatGPTOAuthRoutingConfig{
		Strategy:           ChatGPTOAuthStrategyRoundRobin,
		ExtraProviderNames: []string{"codex-work", "codex-team"},
	}
	override := &ChatGPTOAuthRoutingConfig{
		OverrideMode:       ChatGPTOAuthOverrideCustom,
		Strategy:           ChatGPTOAuthStrategyRoundRobin,
		ExtraProviderNames: []string{"rogue-provider"},
	}

	got := ResolveEffectiveChatGPTOAuthRouting(defaults, override)
	if got == nil {
		t.Fatal("ResolveEffectiveChatGPTOAuthRouting() = nil, want config")
	}
	if !reflect.DeepEqual(got.ExtraProviderNames, defaults.ExtraProviderNames) {
		t.Fatalf("ExtraProviderNames = %#v, want provider defaults %#v", got.ExtraProviderNames, defaults.ExtraProviderNames)
	}
}

// ─── ParseAllowImageGeneration ────────────────────────────────────────────

func TestParseAllowImageGeneration_DefaultTrue_NoOtherConfig(t *testing.T) {
	ag := &AgentData{}
	if !ag.ParseAllowImageGeneration() {
		t.Error("empty other_config must default to true (image gen enabled)")
	}
}

func TestParseAllowImageGeneration_DefaultTrue_EmptyObject(t *testing.T) {
	ag := &AgentData{OtherConfig: json.RawMessage(`{}`)}
	if !ag.ParseAllowImageGeneration() {
		t.Error("empty JSONB object must default to true")
	}
}

func TestParseAllowImageGeneration_ExplicitTrue(t *testing.T) {
	ag := &AgentData{OtherConfig: json.RawMessage(`{"allow_image_generation":true}`)}
	if !ag.ParseAllowImageGeneration() {
		t.Error("explicit true must return true")
	}
}

func TestParseAllowImageGeneration_ExplicitFalse(t *testing.T) {
	ag := &AgentData{OtherConfig: json.RawMessage(`{"allow_image_generation":false}`)}
	if ag.ParseAllowImageGeneration() {
		t.Error("explicit false must return false")
	}
}

func TestParseAllowImageGeneration_MalformedJSON_DefaultsTrue(t *testing.T) {
	ag := &AgentData{OtherConfig: json.RawMessage(`{not-json`)}
	if !ag.ParseAllowImageGeneration() {
		t.Error("malformed other_config must default to true")
	}
}

func TestParseAllowImageGeneration_UnrelatedKeys_DefaultsTrue(t *testing.T) {
	ag := &AgentData{OtherConfig: json.RawMessage(`{"self_evolve":true,"skill_evolve":false}`)}
	if !ag.ParseAllowImageGeneration() {
		t.Error("other_config without allow_image_generation key must default to true")
	}
}

func TestParseToolsConfigWaitPolicy(t *testing.T) {
	t.Parallel()
	agent := AgentData{
		ToolsConfig: json.RawMessage(`{"profile":"coding","wait":{"min_ms":500,"max_ms":60000},"toolCallPrefix":"proxy_"}`),
	}

	got := agent.ParseToolsConfig()
	if got == nil {
		t.Fatal("ParseToolsConfig() = nil")
	}
	if got.Wait == nil {
		t.Fatal("Wait policy was not parsed")
	}
	if got.Wait.MinMs != 500 || got.Wait.MaxMs != 60000 {
		t.Fatalf("Wait = %#v, want min=500 max=60000", got.Wait)
	}
	if got.ToolCallPrefix != "proxy_" {
		t.Fatalf("ToolCallPrefix = %q", got.ToolCallPrefix)
	}
}
