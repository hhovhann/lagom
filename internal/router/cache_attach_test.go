package router

import (
	"testing"

	"lagom/internal/providers"
)

func TestCacheKeySeparatesDocuments(t *testing.T) {
	msg := func(data string) []providers.Message {
		return []providers.Message{{Role: "user", Content: "Extract the totals.",
			Attachments: []providers.Attachment{{MediaType: "application/pdf", Data: data}}}}
	}
	if CacheKey(msg("AAAA"), 100, "") == CacheKey(msg("BBBB"), 100, "") {
		t.Error("two different PDFs with the same prompt must not share a cache entry")
	}
	if CacheKey(msg("AAAA"), 100, "") != CacheKey(msg("AAAA"), 100, "") {
		t.Error("the same PDF must hit the same entry")
	}
}
