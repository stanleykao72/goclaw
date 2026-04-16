package esmithkm

import (
	"testing"
	"time"
)

func TestDedupCache_FirstSeenIsFalseSecondSeenIsTrue(t *testing.T) {
	c := newDedupCache(time.Hour)
	if c.SeenOrMark("audio:msg-1") {
		t.Errorf("first SeenOrMark should be false")
	}
	if !c.SeenOrMark("audio:msg-1") {
		t.Errorf("second SeenOrMark should be true")
	}
	// Different keys are independent.
	if c.SeenOrMark("audio:msg-2") {
		t.Errorf("different key should be false on first call")
	}
}

func TestDedupCache_ExpiredEntryIsForgotten(t *testing.T) {
	c := newDedupCache(10 * time.Millisecond)
	c.SeenOrMark("audio:msg-1")
	time.Sleep(20 * time.Millisecond)
	if c.SeenOrMark("audio:msg-1") {
		t.Errorf("expired entry should not register as seen")
	}
}

func TestDedupCache_EmptyKeyIsNotCached(t *testing.T) {
	c := newDedupCache(time.Hour)
	if c.SeenOrMark("") {
		t.Errorf("empty key should never register as seen")
	}
	if c.SeenOrMark("") {
		t.Errorf("empty key should still not register on second call")
	}
}
