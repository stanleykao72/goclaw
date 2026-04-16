package esmithkm

// dedup.go provides a small in-memory TTL cache used to detect LINE webhook
// resends. LINE retries the same Event (with the same Message.ID) when our
// webhook handler does not return 200 OK in time, or when its retry policy
// fires for any other reason. The retry happens within minutes — usually
// well under one hour. We do not need persistent dedup; an in-process map
// is enough to catch the common case.
//
// The dedup cache is shared between the audio path (key = LINE message ID)
// and the GDrive link path (key = "gdrive:" + URL). Each entry has its own
// expiry; expired entries are lazy-evicted on Seen() lookups so the map
// stays small without a background goroutine.

import (
	"sync"
	"time"
)

const dedupDefaultTTL = 1 * time.Hour

// dedupCache is goroutine-safe.
type dedupCache struct {
	mu      sync.Mutex
	entries map[string]time.Time // key → expiry
	ttl     time.Duration
}

func newDedupCache(ttl time.Duration) *dedupCache {
	if ttl <= 0 {
		ttl = dedupDefaultTTL
	}
	return &dedupCache{
		entries: make(map[string]time.Time),
		ttl:     ttl,
	}
}

// SeenOrMark returns true if `key` was already seen within the TTL window.
// On a miss, it atomically marks the key as seen and returns false.
func (c *dedupCache) SeenOrMark(key string) bool {
	if key == "" {
		return false
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()

	// Lazy eviction — keeps the map small without a goroutine.
	if exp, ok := c.entries[key]; ok {
		if now.Before(exp) {
			return true
		}
		delete(c.entries, key)
	}
	c.entries[key] = now.Add(c.ttl)

	// Opportunistic cleanup of other expired entries to bound memory.
	// Cheap because we only sweep when the map gets non-trivially large.
	if len(c.entries) > 256 {
		for k, exp := range c.entries {
			if now.After(exp) {
				delete(c.entries, k)
			}
		}
	}
	return false
}

// audioMessageKey builds the dedup key for a LINE audio message.
func audioMessageKey(messageID string) string {
	return "audio:" + messageID
}

// gdriveURLKey builds the dedup key for a GDrive URL.
func gdriveURLKey(url string) string {
	return "gdrive:" + url
}
