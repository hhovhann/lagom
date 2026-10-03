package router

import (
	"testing"
	"time"

	"lagom/internal/providers"
)

func msgs(s string) []providers.Message {
	return []providers.Message{{Role: "user", Content: providers.Content(s)}}
}

func TestPutGet(t *testing.T) {
	c := NewCache(100, time.Minute)
	k := CacheKey(msgs("hi"), 0, "equals:x")
	if _, ok := c.Get(k); ok {
		t.Fatal("unexpected hit")
	}
	c.Put(k, CacheEntry{Text: "answer", Model: "m"})
	if e, ok := c.Get(k); !ok || e.Text != "answer" {
		t.Fatalf("miss after put: %+v %v", e, ok)
	}
}

func TestKeyCoversEverythingThatChangesTheAnswer(t *testing.T) {
	base := CacheKey(msgs("hi"), 10, "equals:x")
	for name, other := range map[string][32]byte{
		"content":   CacheKey(msgs("hello"), 10, "equals:x"),
		"maxTokens": CacheKey(msgs("hi"), 11, "equals:x"),
		"verifier":  CacheKey(msgs("hi"), 10, "equals:y"),
		"role":      CacheKey([]providers.Message{{Role: "system", Content: "hi"}}, 10, "equals:x"),
	} {
		if other == base {
			t.Errorf("key ignores %s", name)
		}
	}
	// message boundaries must matter: ["ab","c"] != ["a","bc"]
	a := CacheKey([]providers.Message{{Role: "user", Content: "ab"}, {Role: "user", Content: "c"}}, 0, "")
	b := CacheKey([]providers.Message{{Role: "user", Content: "a"}, {Role: "user", Content: "bc"}}, 0, "")
	if a == b {
		t.Error("key ignores message boundaries")
	}
}

func TestTTLAndClear(t *testing.T) {
	c := NewCache(100, 20*time.Millisecond)
	k := CacheKey(msgs("x"), 0, "")
	c.Put(k, CacheEntry{Text: "v"})
	time.Sleep(40 * time.Millisecond)
	if _, ok := c.Get(k); ok {
		t.Fatal("entry should have expired")
	}
	c.Put(k, CacheEntry{Text: "v"})
	c.Clear()
	if c.Len() != 0 {
		t.Fatal("clear failed")
	}
}

func TestBounded(t *testing.T) {
	c := NewCache(64, time.Minute)
	for i := 0; i < 5000; i++ {
		c.Put(CacheKey(msgs(string(rune(i))+"-"+time.Now().String()), i, ""), CacheEntry{Text: "v"})
	}
	if n := c.Len(); n > 64+shards {
		t.Fatalf("cache grew to %d entries", n)
	}
}
