// Package providers holds the wire-neutral request/response types and the model adapters
// (OpenAI-compatible servers and Anthropic).
package providers

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Content accepts either a plain string or an OpenAI-style array of text parts.
type Content string

func (c *Content) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*c = Content(s)
		return nil
	}
	if string(b) == "null" {
		*c = ""
		return nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(b, &parts); err != nil {
		return err
	}
	var out string
	for _, p := range parts {
		if p.Type == "text" || p.Type == "" {
			out += p.Text
		}
	}
	*c = Content(out)
	return nil
}

// Attachment is a document or image sent with a message: a PDF or a picture, base64-encoded.
// It is never part of the text, so the verifier, the guard and the classifier see only text.
type Attachment struct {
	MediaType string // application/pdf, image/png, image/jpeg, image/gif or image/webp
	Data      string // standard base64, no data: prefix
	Name      string // file name, shown to the model by some providers
}

// IsPDF reports whether the attachment is a PDF (other attachments are images).
func (a Attachment) IsPDF() bool { return a.MediaType == "application/pdf" }

var attachmentTypes = map[string]bool{
	"application/pdf": true, "image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true,
}

// ParseDataURI splits "data:<mime>;base64,<data>" and checks the type and the encoding.
func ParseDataURI(uri, name string) (Attachment, error) {
	rest, ok := strings.CutPrefix(uri, "data:")
	if !ok {
		return Attachment{}, fmt.Errorf("only base64 data: URIs are supported for files and images, not links or file ids")
	}
	meta, data, ok := strings.Cut(rest, ",")
	mime, enc, _ := strings.Cut(meta, ";")
	if !ok || enc != "base64" {
		return Attachment{}, fmt.Errorf("file data must be a base64 data: URI")
	}
	if !attachmentTypes[mime] {
		return Attachment{}, fmt.Errorf("unsupported file type %q (use PDF, PNG, JPEG, GIF or WebP)", mime)
	}
	if _, err := base64.StdEncoding.DecodeString(data); err != nil {
		return Attachment{}, fmt.Errorf("file data is not valid base64")
	}
	return Attachment{MediaType: mime, Data: data, Name: name}, nil
}

type Message struct {
	Role        string       `json:"role"`
	Content     Content      `json:"content"`
	Attachments []Attachment `json:"-"`
}

// UnmarshalJSON reads OpenAI-style messages, including content parts of type "file"
// (PDF) and "image_url" given as base64 data URIs.
func (m *Message) UnmarshalJSON(b []byte) error {
	var raw struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	m.Role, m.Attachments = raw.Role, nil
	if len(raw.Content) > 0 {
		if err := json.Unmarshal(raw.Content, &m.Content); err != nil {
			return err
		}
	}
	if len(raw.Content) == 0 || raw.Content[0] != '[' {
		return nil
	}
	var parts []struct {
		Type string `json:"type"`
		File struct {
			Name string `json:"filename"`
			Data string `json:"file_data"`
			ID   string `json:"file_id"`
		} `json:"file"`
		Image struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(raw.Content, &parts); err != nil {
		return err
	}
	for _, p := range parts {
		var uri, name string
		switch p.Type {
		case "file":
			uri, name = p.File.Data, p.File.Name
			if uri == "" {
				return fmt.Errorf("file part needs file_data (a base64 data: URI); file ids are not supported")
			}
		case "image_url":
			uri = p.Image.URL
		default:
			continue
		}
		a, err := ParseDataURI(uri, name)
		if err != nil {
			return err
		}
		m.Attachments = append(m.Attachments, a)
	}
	return nil
}

// MarshalJSON writes the OpenAI wire shape: a plain string, or with attachments an
// array of parts (files and images first, then the text).
func (m Message) MarshalJSON() ([]byte, error) {
	if len(m.Attachments) == 0 {
		return json.Marshal(struct {
			Role    string  `json:"role"`
			Content Content `json:"content"`
		}{m.Role, m.Content})
	}
	parts := make([]map[string]any, 0, len(m.Attachments)+1)
	for i, a := range m.Attachments {
		uri := "data:" + a.MediaType + ";base64," + a.Data
		if a.IsPDF() {
			name := a.Name
			if name == "" {
				name = fmt.Sprintf("document-%d.pdf", i+1)
			}
			parts = append(parts, map[string]any{"type": "file", "file": map[string]string{"filename": name, "file_data": uri}})
		} else {
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]string{"url": uri}})
		}
	}
	parts = append(parts, map[string]any{"type": "text", "text": string(m.Content)})
	return json.Marshal(map[string]any{"role": m.Role, "content": parts})
}

// ChatRequest is the inbound OpenAI-compatible request.
type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Stream      bool      `json:"stream,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Temperature *float64  `json:"temperature,omitempty"`
}

// Call is one provider attempt: the request plus per-model settings.
type Call struct {
	Messages    []Message
	MaxTokens   int
	Temperature *float64
	Effort      string            // Anthropic output_config.effort, optional
	Meta        map[string]string // simulator hints; only forwarded to mock providers
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type Result struct {
	Text    string
	Usage   Usage
	Model   string
	Latency time.Duration
	TTFT    time.Duration
}
