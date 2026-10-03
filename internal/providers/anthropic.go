package providers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Anthropic speaks the Messages API (POST /v1/messages).
//
// Notes on the wire format, so the gateway stays correct as models change:
//   - system prompts are a top-level field, not a message role;
//   - max_tokens is required;
//   - sampling parameters (temperature etc.) are rejected by the newest models,
//     so they are deliberately never sent;
//   - newer models think by default, so cheap routes can set output_config.effort
//     (per-model "effort" in the config); Haiku 4.5 does not accept it;
//   - a refusal arrives as HTTP 200 with stop_reason "refusal" and is surfaced
//     as an error so the cascade can escalate.
type Anthropic struct {
	base string
	key  string
	hc   *http.Client
}

func NewAnthropic(baseURL, apiKey string) *Anthropic {
	if baseURL == "" {
		baseURL = "https://anthropic.com"
	}
	return &Anthropic{base: strings.TrimRight(baseURL, "/"), key: apiKey, hc: newClient()}
}

type anthropicReq struct {
	Model        string         `json:"model"`
	MaxTokens    int            `json:"max_tokens"`
	System       string         `json:"system,omitempty"`
	Messages     []anthropicMsg `json:"messages"`
	Stream       bool           `json:"stream,omitempty"`
	OutputConfig *anthropicOutC `json:"output_config,omitempty"`
}

// anthropicMsg is a message in Anthropic's shape: a plain string, or content blocks
// when documents or images are attached (documents first, then the text).
type anthropicMsg struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

func toAnthropicMsg(m Message) anthropicMsg {
	if len(m.Attachments) == 0 {
		return anthropicMsg{Role: m.Role, Content: string(m.Content)}
	}
	blocks := make([]map[string]any, 0, len(m.Attachments)+1)
	for _, a := range m.Attachments {
		kind := "image"
		if a.IsPDF() {
			kind = "document"
		}
		blocks = append(blocks, map[string]any{"type": kind, "source": map[string]string{"type": "base64", "media_type": a.MediaType, "data": a.Data}})
	}
	blocks = append(blocks, map[string]any{"type": "text", "text": string(m.Content)})
	return anthropicMsg{Role: m.Role, Content: blocks}
}

type anthropicOutC struct {
	Effort string `json:"effort"`
}

func (a *Anthropic) Chat(ctx context.Context, c *Call, model string, onDelta func(string)) (*Result, error) {
	stream := onDelta != nil
	body := anthropicReq{Model: model, MaxTokens: c.MaxTokens, Stream: stream}
	if body.MaxTokens == 0 {
		body.MaxTokens = 1024
	}
	var sys []string
	for _, m := range c.Messages {
		if m.Role == "system" || m.Role == "developer" {
			sys = append(sys, string(m.Content))
			continue
		}
		body.Messages = append(body.Messages, toAnthropicMsg(m))
	}
	body.System = strings.Join(sys, "\n\n")
	if c.Effort != "" {
		body.OutputConfig = &anthropicOutC{Effort: c.Effort}
	}
	payload, err := json.Marshal(&body)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+"/v1/messages", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", a.key)
	req.Header.Set("anthropic-version", "2023-06-01")

	start := time.Now()
	resp, err := a.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("anthropic: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	res := &Result{Model: model}
	if !stream {
		var out struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			StopReason string `json:"stop_reason"`
			Usage      struct {
				InputTokens         int `json:"input_tokens"`
				OutputTokens        int `json:"output_tokens"`
				CacheCreationTokens int `json:"cache_creation_input_tokens"`
				CacheReadTokens     int `json:"cache_read_input_tokens"`
			} `json:"usage"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return nil, fmt.Errorf("decode response: %w", err)
		}
		if out.StopReason == "refusal" {
			return nil, fmt.Errorf("anthropic: model refused (stop_reason=refusal)")
		}
		var sb strings.Builder
		for _, b := range out.Content {
			if b.Type == "text" {
				sb.WriteString(b.Text)
			}
		}
		res.Text = sb.String()
		res.Usage = Usage{
			PromptTokens:     out.Usage.InputTokens + out.Usage.CacheCreationTokens + out.Usage.CacheReadTokens,
			CompletionTokens: out.Usage.OutputTokens,
		}
		res.Latency = time.Since(start)
		res.TTFT = res.Latency
		return res, nil
	}

	var sb strings.Builder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[5:])
		var ev struct {
			Type    string `json:"type"`
			Message struct {
				Usage struct {
					InputTokens int `json:"input_tokens"`
				} `json:"usage"`
			} `json:"message"`
			Delta struct {
				Type       string `json:"type"`
				Text       string `json:"text"`
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "message_start":
			res.Usage.PromptTokens = ev.Message.Usage.InputTokens
		case "content_block_delta":
			if ev.Delta.Type == "text_delta" && ev.Delta.Text != "" {
				if res.TTFT == 0 {
					res.TTFT = time.Since(start)
				}
				sb.WriteString(ev.Delta.Text)
				onDelta(ev.Delta.Text)
			}
		case "message_delta":
			if ev.Usage.OutputTokens > 0 {
				res.Usage.CompletionTokens = ev.Usage.OutputTokens
			}
			if ev.Delta.StopReason == "refusal" {
				return nil, fmt.Errorf("anthropic: model refused (stop_reason=refusal)")
			}
		case "error":
			return nil, fmt.Errorf("anthropic stream error: %s", ev.Error.Message)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read stream: %w", err)
	}
	res.Text = sb.String()
	res.Latency = time.Since(start)
	if res.Usage.CompletionTokens == 0 {
		res.Usage.CompletionTokens = approxTokens(res.Text)
	}
	return res, nil
}
