// Package cache is a sharded, TTL-bounded exact-match response cache.
package router

import (
	"crypto/sha256"
	"sync"
	"time"

	"lagom/internal/providers"
)

const shards = 32

type CacheEntry struct {
	Text    string
	Usage   providers.Usage
	Model   string
	CostUSD float64 // what producing this answer originally cost
	expires time.Time
}

type shard struct {
	mu sync.RWMutex
	m  map[[32]byte]CacheEntry
}

type Cache struct {
	sh      [shards]shard
	perShrd int
	ttl     time.Duration
}

func NewCache(maxEntries int, ttl time.Duration) *Cache {
	c := &Cache{perShrd: maxEntries/shards + 1, ttl: ttl}
	for i := range c.sh {
		c.sh[i].m = make(map[[32]byte]CacheEntry)
	}
	return c
}

// Key hashes everything that can change the answer. Callers pass the verifier
// spec too, so an answer verified under one spec is never reused under another.
func CacheKey(msgs []providers.Message, maxTokens int, verifySpec string) [32]byte {
	h := sha256.New()
	for _, m := range msgs {
		h.Write([]byte(m.Role))
		h.Write([]byte{0})
		h.Write([]byte(m.Content))
		for _, a := range m.Attachments {
			sum := sha256.Sum256([]byte(a.Data))
			h.Write([]byte(a.MediaType))
			h.Write(sum[:])
		}
		h.Write([]byte{1})
	}
	var b [8]byte
	for i := 0; i < 8; i++ {
		b[i] = byte(maxTokens >> (8 * i))
	}
	h.Write(b[:])
	h.Write([]byte(verifySpec))
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func (c *Cache) Get(k [32]byte) (CacheEntry, bool) {
	s := &c.sh[k[0]%shards]
	s.mu.RLock()
	e, ok := s.m[k]
	s.mu.RUnlock()
	if !ok || time.Now().After(e.expires) {
		return CacheEntry{}, false
	}
	return e, true
}

func (c *Cache) Put(k [32]byte, e CacheEntry) {
	e.expires = time.Now().Add(c.ttl)
	s := &c.sh[k[0]%shards]
	s.mu.Lock()
	if len(s.m) >= c.perShrd {
		for old := range s.m { // map iteration order is random: cheap random eviction
			delete(s.m, old)
			break
		}
	}
	s.m[k] = e
	s.mu.Unlock()
}

func (c *Cache) Clear() {
	for i := range c.sh {
		c.sh[i].mu.Lock()
		c.sh[i].m = make(map[[32]byte]CacheEntry)
		c.sh[i].mu.Unlock()
	}
}

func (c *Cache) Len() int {
	n := 0
	for i := range c.sh {
		c.sh[i].mu.RLock()
		n += len(c.sh[i].m)
		c.sh[i].mu.RUnlock()
	}
	return n
}
