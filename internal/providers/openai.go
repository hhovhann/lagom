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

// OpenAI speaks the OpenAI chat-completions protocol. The same adapter serves
// OpenAI, Ollama (/v1), vLLM and the bundled mock server.
type OpenAI struct {
	base        string
	key         string
	forwardMeta bool
	tokensField string
	hc          *http.Client
}

func NewOpenAI(baseURL, apiKey string, forwardMeta bool, maxTokensField string) *OpenAI {
	return &OpenAI{
		base:        strings.TrimRight(baseURL, "/"),
		key:         apiKey,
		forwardMeta: forwardMeta,
		tokensField: maxTokensField,
		hc:          newClient(),
	}
}

type oaiBody struct {
	Model               string         `json:"model"`
	Messages            []Message      `json:"messages"`
	Stream              bool           `json:"stream,omitempty"`
	StreamOptions       *oaiStreamOpts `json:"stream_options,omitempty"`
	MaxTokens           int            `json:"max_tokens,omitempty"`
	MaxCompletionTokens int            `json:"max_completion_tokens,omitempty"`
	Temperature         *float64       `json:"temperature,omitempty"`
}

type oaiStreamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

type oaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

func (o *OpenAI) Chat(ctx context.Context, c *Call, model string, onDelta func(string)) (*Result, error) {
	stream := onDelta != nil
	body := oaiBody{Model: model, Messages: c.Messages, Stream: stream, Temperature: c.Temperature}
	if o.tokensField == "max_completion_tokens" {
		body.MaxCompletionTokens = c.MaxTokens
	} else {
		body.MaxTokens = c.MaxTokens
	}
	if stream {
		body.StreamOptions = &oaiStreamOpts{IncludeUsage: true}
	}
	payload, err := json.Marshal(&body)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.base+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if o.key != "" {
		req.Header.Set("Authorization", "Bearer "+o.key)
	}
	if o.forwardMeta {
		for k, v := range c.Meta {
			req.Header.Set("X-Lagom-Mock-"+k, v)
		}
	}

	start := time.Now()
	resp, err := o.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("openai-compatible: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	res := &Result{Model: model}
	if !stream {
		var out struct {
			Choices []struct {
				Message struct {
					Content Content `json:"content"`
				} `json:"message"`
			} `json:"choices"`
			Usage oaiUsage `json:"usage"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return nil, fmt.Errorf("decode response: %w", err)
		}
		if len(out.Choices) > 0 {
			res.Text = string(out.Choices[0].Message.Content)
		}
		res.Usage = Usage{PromptTokens: out.Usage.PromptTokens, CompletionTokens: out.Usage.CompletionTokens}
		res.Latency = time.Since(start)
		res.TTFT = res.Latency
		o.fillUsage(res, c)
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
		if len(data) == 0 {
			continue
		}
		if bytes.Equal(data, []byte("[DONE]")) {
			break
		}
		var ch struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *oaiUsage `json:"usage"`
		}
		if err := json.Unmarshal(data, &ch); err != nil {
			continue
		}
		if ch.Usage != nil {
			res.Usage = Usage{PromptTokens: ch.Usage.PromptTokens, CompletionTokens: ch.Usage.CompletionTokens}
		}
		if len(ch.Choices) > 0 && ch.Choices[0].Delta.Content != "" {
			d := ch.Choices[0].Delta.Content
			if res.TTFT == 0 {
				res.TTFT = time.Since(start)
			}
			sb.WriteString(d)
			onDelta(d)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read stream: %w", err)
	}
	res.Text = sb.String()
	res.Latency = time.Since(start)
	o.fillUsage(res, c)
	return res, nil
}

func (o *OpenAI) fillUsage(res *Result, c *Call) {
	if res.Usage.PromptTokens == 0 {
		res.Usage.PromptTokens = approxTokens(promptText(c.Messages))
	}
	if res.Usage.CompletionTokens == 0 {
		res.Usage.CompletionTokens = approxTokens(res.Text)
	}
}

// Embed calls POST /embeddings (OpenAI-compatible; LM Studio, Ollama and Gemini's compatibility layer serve it).
func (o *OpenAI) Embed(ctx context.Context, model, text string) ([]float64, error) {
	payload, err := json.Marshal(map[string]string{"model": model, "input": text})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.base+"/embeddings", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if o.key != "" {
		req.Header.Set("Authorization", "Bearer "+o.key)
	}
	resp, err := o.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("embeddings: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("embeddings: decode: %w", err)
	}
	if len(out.Data) == 0 || len(out.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("embeddings: empty response")
	}
	return out.Data[0].Embedding, nil
}
