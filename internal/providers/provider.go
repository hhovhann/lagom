// Package providers contains the upstream adapters. Each adapter normalises
// one wire format into Call / Result, including streaming and usage.
package providers

import (
	"context"
	"net"
	"net/http"
	"time"
)

// Provider performs one chat completion. If onDelta is non-nil the response is
// streamed and every text delta is passed to it as it arrives; the returned
// Result still carries the full text and the final usage.
type Provider interface {
	Chat(ctx context.Context, c *Call, model string, onDelta func(string)) (*Result, error)
}

// Embedder turns text into a vector. Only OpenAI-compatible servers implement it.
type Embedder interface {
	Embed(ctx context.Context, model, text string) ([]float64, error)
}

// sharedTransport is tuned for many concurrent upstream streams.
func newClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          8192,
		MaxIdleConnsPerHost:   2048,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 0, // per-attempt deadline comes from the context
		DisableCompression:    true,
		ForceAttemptHTTP2:     true,
	}}
}

// approxTokens is the fallback when an upstream does not report usage.
func approxTokens(s string) int {
	n := len(s) / 4
	if n < 1 {
		n = 1
	}
	return n
}

func promptText(msgs []Message) string {
	n := 0
	for _, m := range msgs {
		n += len(m.Content)
	}
	b := make([]byte, 0, n)
	for _, m := range msgs {
		b = append(b, m.Content...)
	}
	return string(b)
}
