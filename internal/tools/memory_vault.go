package tools

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// memory_vault.go implements the "vault" memory backend: agent memory persisted
// as real Obsidian-markdown files under <vaultDir>/<scope>/ instead of Postgres.
//
// Naming note: this is deliberately distinct from goclaw's unrelated DB-backed
// "Knowledge Vault" (vault_interceptor.go / vault_search.go). Everything here is
// prefixed memoryVault / MemoryVault to avoid conceptual collision.

// memoryVaultIndexFile is the per-scope curated index that auto-inject reads each
// turn and that the agent is guided to maintain (the three-tier layout's top).
const memoryVaultIndexFile = "LONGTERM.md"

// MemoryVaultSubdir derives the per-conversation vault scope folder from the
// conversation identity. Group conversations are shared per chat; direct (1:1)
// conversations are per user. The layout mirrors the existing
// hub-memory/<channel>/{group-,user-}<id>/ structure.
//
// It uses peerKind ("group" | "direct") + chatID/userID — NOT a userID prefix
// (no "group:"/"lineworks:" literal exists in the runtime tool identity). Every
// segment is run through SanitizePathSegment so a crafted id cannot escape.
func MemoryVaultSubdir(ctx context.Context) string {
	user := store.UserIDFromContext(ctx)
	if user == "" {
		user = store.SenderIDFromContext(ctx)
	}
	return MemoryVaultSubdirFor(ToolChannelTypeFromCtx(ctx), ToolPeerKindFromCtx(ctx), ToolChatIDFromCtx(ctx), user)
}

// MemoryVaultSubdirFor is the pure scope computation. Exported so callers that do
// NOT run in the tool-dispatch context (e.g. the per-turn auto-injector, whose ctx
// lacks the tool peerKind/chatID keys) can reproduce the SAME scope a write
// produces — otherwise group writes and group recall would target different
// folders. Every segment is sanitized so a crafted id cannot escape.
func MemoryVaultSubdirFor(channelType, peerKind, chatID, userID string) string {
	channel := SanitizePathSegment(channelType)
	if channel == "" {
		channel = "unknown"
	}
	if peerKind == "group" && chatID != "" {
		return filepath.Join(channel, "group-"+SanitizePathSegment(chatID))
	}
	// Direct conversation. Guard against an empty identity so two unidentified
	// direct users don't collapse into a shared "user-" scope and cross-pollinate.
	user := SanitizePathSegment(userID)
	if user == "" {
		user = "unknown"
	}
	return filepath.Join(channel, "user-"+user)
}

// isVaultLayoutPath reports whether a path belongs to the three-tier vault layout
// (LONGTERM.md, topics/*, journal/*, archive/*). This is the EXTENSION used only
// when the backend is "vault"; it is intentionally NOT folded into isMemoryPath so
// that the db memory path stays bit-for-bit unchanged for non-vault agents.
func isVaultLayoutPath(path, workspace string) bool {
	clean := filepath.Clean(path)
	if workspace != "" && filepath.IsAbs(clean) {
		if rel, err := filepath.Rel(filepath.Clean(workspace), clean); err == nil && !strings.HasPrefix(rel, "..") {
			clean = rel
		}
	}
	slash := filepath.ToSlash(clean)
	if filepath.Base(slash) == memoryVaultIndexFile {
		return true
	}
	for _, pfx := range []string{"topics/", "journal/", "archive/"} {
		if strings.HasPrefix(slash, pfx) {
			return true
		}
	}
	return false
}

// isMemoryPathForBackend is the shared predicate that decides whether a path is a
// memory path for the given backend. For db it is exactly isMemoryPath (unchanged);
// for vault it additionally recognizes the vault layout. Used by both the
// interceptor routing AND the ACL exemption so an exempt path is always a real
// memory path.
func isMemoryPathForBackend(backend, path, workspace string) bool {
	if isMemoryPath(path, workspace) {
		return true
	}
	return backend == "vault" && isVaultLayoutPath(path, workspace)
}

// --- per-scope-file serialization (concurrency safety) ---

type keyedMutex struct{ m sync.Map }

func (k *keyedMutex) lock(key string) func() {
	v, _ := k.m.LoadOrStore(key, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// vaultFileMu serializes read-modify-write on a given vault file so concurrent
// appends (any group member can write) do not lose updates.
var vaultFileMu keyedMutex

// vaultFallbackWarned tracks agents we have already warned about a missing vault
// dir, so the fail-safe warning fires at most once per agent.
var vaultFallbackWarned sync.Map

// warnVaultFallbackOnce emits a loud (non-silent) warning the first time an agent
// configured for vault mode falls back to db because the vault dir is unset.
func warnVaultFallbackOnce(ctx context.Context) {
	agentID := store.AgentIDFromContext(ctx).String()
	if _, loaded := vaultFallbackWarned.LoadOrStore(agentID, true); !loaded {
		slog.Warn("memory vault: memory_backend=vault but memory vault dir is unset — falling back to db backend (group writes will hit the file-writer ACL and Chinese recall will use FTS)",
			"agent", agentID)
	}
}

// resolveMemoryVaultPath joins vaultDir/scope/relPath and confines the result
// within vaultDir/scope. Returns an error if the cleaned path escapes the scope
// directory (defense-in-depth on top of SanitizePathSegment on the scope id).
func resolveMemoryVaultPath(vaultDir, scope, relPath string) (string, error) {
	scopeDir := filepath.Join(vaultDir, scope)
	full := filepath.Join(scopeDir, relPath)
	rel, err := filepath.Rel(scopeDir, full)
	if err != nil {
		return "", fmt.Errorf("memory vault: cannot resolve path %q: %w", relPath, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("memory vault: path %q escapes scope %q", relPath, scope)
	}
	return full, nil
}

// atomicWriteFile writes data to path via a temp file in the same directory then
// renames — so a concurrent reader (e.g. obsidian-git) never sees a partial file.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".mvtmp-*")
	if err != nil {
		return fmt.Errorf("memory vault: create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("memory vault: write temp: %w", err)
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return fmt.Errorf("memory vault: chmod temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("memory vault: close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("memory vault: rename temp: %w", err)
	}
	return nil
}

// writeMemoryVaultFile writes a memory file to the vault. appendMode merges with
// existing content (matching the db interceptor's separator). On a non-append
// overwrite of differing content, PreviousContent is populated so callers can warn.
func writeMemoryVaultFile(ctx context.Context, vaultDir, workspace, path, content string, appendMode bool) (MemoryWriteResult, error) {
	scope := MemoryVaultSubdir(ctx)
	rel := normalizeToRelative(path, workspace)
	full, err := resolveMemoryVaultPath(vaultDir, scope, rel)
	if err != nil {
		return MemoryWriteResult{Handled: true}, err
	}

	unlock := vaultFileMu.lock(full)
	defer unlock()

	var previousContent string
	if existing, rerr := os.ReadFile(full); rerr == nil && len(existing) > 0 {
		if appendMode {
			content = string(existing) + "\n\n---\n\n" + content
		} else if string(existing) != content {
			previousContent = string(existing)
		}
	}

	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return MemoryWriteResult{Handled: true}, fmt.Errorf("memory vault: mkdir: %w", err)
	}
	if err := atomicWriteFile(full, []byte(content), 0o644); err != nil {
		return MemoryWriteResult{Handled: true}, err
	}
	return MemoryWriteResult{Handled: true, PreviousContent: previousContent}, nil
}

// readMemoryVaultFile reads a memory file from the vault. A missing file/scope is
// cold-start: returns ("", true, nil) — handled-but-empty, never an OS error,
// matching the db path's graceful behavior.
func readMemoryVaultFile(ctx context.Context, vaultDir, workspace, path string) (string, bool, error) {
	scope := MemoryVaultSubdir(ctx)
	rel := normalizeToRelative(path, workspace)
	full, err := resolveMemoryVaultPath(vaultDir, scope, rel)
	if err != nil {
		return "", true, err
	}
	data, err := os.ReadFile(full)
	if err != nil {
		if os.IsNotExist(err) {
			return "", true, nil
		}
		return "", true, fmt.Errorf("memory vault: read: %w", err)
	}
	return string(data), true, nil
}

// listMemoryVaultFiles lists every file in the scope folder (recursively). A
// missing scope folder is cold-start: returns ("", true, nil).
func listMemoryVaultFiles(ctx context.Context, vaultDir string) (string, bool, error) {
	scope := MemoryVaultSubdir(ctx)
	scopeDir := filepath.Join(vaultDir, scope)

	var files []string
	err := filepath.WalkDir(scopeDir, func(p string, d os.DirEntry, werr error) error {
		if werr != nil {
			if os.IsNotExist(werr) {
				return filepath.SkipDir
			}
			return werr
		}
		if d.IsDir() {
			return nil
		}
		if rel, rerr := filepath.Rel(scopeDir, p); rerr == nil {
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return "", true, fmt.Errorf("memory vault: list: %w", err)
	}
	sort.Strings(files)
	var sb strings.Builder
	for _, f := range files {
		fmt.Fprintf(&sb, "[FILE] %s\n", f)
	}
	return sb.String(), true, nil
}

// ReadMemoryVaultIndexForScope returns the scope's index file (LONGTERM.md)
// content, truncated to the given line/byte caps (the auto-inject budget). The
// second return is true when truncation occurred; cold-start returns ("", false,
// nil). It takes an explicit scope (rather than deriving from ctx) because the
// auto-injector's ctx does not carry the tool peerKind/chatID keys, so it must
// pass the scope computed from the request via MemoryVaultSubdirFor.
func ReadMemoryVaultIndexForScope(vaultDir, scope string, maxLines, maxBytes int) (string, bool, error) {
	full, err := resolveMemoryVaultPath(vaultDir, scope, memoryVaultIndexFile)
	if err != nil {
		return "", false, err
	}
	data, rerr := os.ReadFile(full)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return "", false, nil
		}
		return "", false, rerr
	}
	if len(data) == 0 {
		return "", false, nil
	}
	return truncateForBudget(string(data), maxLines, maxBytes)
}

// WrapUntrustedMemory wraps memory content (especially group-shared, multi-writer
// memory) with the external/untrusted-content security markers so the model treats
// it as reference data, not instructions — mitigating persistent prompt injection
// via poisoned shared memory.
func WrapUntrustedMemory(content string) string {
	return wrapExternalContent(content, "Long-term memory", true)
}

// truncateForBudget caps content to maxLines and maxBytes, appending a marker when
// it trims. Returns (text, truncated, nil).
func truncateForBudget(content string, maxLines, maxBytes int) (string, bool, error) {
	truncated := false
	if maxLines > 0 {
		lines := strings.SplitAfter(content, "\n")
		if len(lines) > maxLines {
			content = strings.Join(lines[:maxLines], "")
			truncated = true
		}
	}
	if maxBytes > 0 && len(content) > maxBytes {
		// Back off to a UTF-8 rune boundary so we never emit a half multi-byte
		// character — critical here, since CJK is the whole reason for the vault.
		cut := maxBytes
		for cut > 0 && !utf8.RuneStart(content[cut]) {
			cut--
		}
		content = content[:cut]
		truncated = true
	}
	if truncated {
		content = strings.TrimRight(content, "\n") + "\n\n[…truncated: read LONGTERM.md / topics / journal for the rest…]\n"
	}
	return content, truncated, nil
}

// grepMemoryVault does a case-insensitive substring search over the scope's
// markdown files and returns a formatted, capped result block. Cold-start / no
// match returns "". This is the vault-mode replacement for Postgres FTS (which
// cannot segment CJK).
func grepMemoryVault(ctx context.Context, vaultDir, query string, maxResults int) (string, error) {
	if maxResults <= 0 {
		maxResults = 20
	}
	scope := MemoryVaultSubdir(ctx)
	scopeDir := filepath.Join(vaultDir, scope)
	needle := strings.ToLower(strings.TrimSpace(query))
	if needle == "" {
		return "", nil
	}

	type hit struct {
		file string
		line int
		text string
	}
	var hits []hit
	err := filepath.WalkDir(scopeDir, func(p string, d os.DirEntry, werr error) error {
		if werr != nil {
			if os.IsNotExist(werr) {
				return filepath.SkipDir
			}
			return werr
		}
		if d.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil // skip unreadable file, keep scanning
		}
		rel, _ := filepath.Rel(scopeDir, p)
		for i, ln := range strings.Split(string(data), "\n") {
			if strings.Contains(strings.ToLower(ln), needle) {
				hits = append(hits, hit{file: filepath.ToSlash(rel), line: i + 1, text: strings.TrimSpace(ln)})
				if len(hits) >= maxResults {
					return filepath.SkipAll
				}
			}
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("memory vault: grep: %w", err)
	}
	if len(hits) == 0 {
		return "", nil
	}
	var sb strings.Builder
	for _, h := range hits {
		fmt.Fprintf(&sb, "%s:%d: %s\n", h.file, h.line, h.text)
	}
	return sb.String(), nil
}
