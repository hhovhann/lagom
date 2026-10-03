package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var pdfB64 = base64.StdEncoding.EncodeToString([]byte("%PDF-1.4 fake"))

func TestMessageParsesFileAndImageParts(t *testing.T) {
	in := `{"role":"user","content":[
		{"type":"file","file":{"filename":"t12.pdf","file_data":"data:application/pdf;base64,` + pdfB64 + `"}},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,` + pdfB64 + `"}},
		{"type":"text","text":"Extract the totals."}]}`
	var m Message
	if err := json.Unmarshal([]byte(in), &m); err != nil {
		t.Fatal(err)
	}
	if m.Content != "Extract the totals." || len(m.Attachments) != 2 {
		t.Fatalf("got %+v", m)
	}
	if !m.Attachments[0].IsPDF() || m.Attachments[0].Name != "t12.pdf" || m.Attachments[1].MediaType != "image/png" {
		t.Errorf("attachments: %+v", m.Attachments)
	}
}

func TestMessageRejectsUnsafeOrUnsupportedFiles(t *testing.T) {
	for name, part := range map[string]string{
		"link":       `{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}`,
		"file id":    `{"type":"file","file":{"file_id":"file-123"}}`,
		"bad type":   `{"type":"file","file":{"file_data":"data:application/zip;base64,` + pdfB64 + `"}}`,
		"bad base64": `{"type":"file","file":{"file_data":"data:application/pdf;base64,@@@"}}`,
		"not base64": `{"type":"file","file":{"file_data":"data:application/pdf,plain"}}`,
	} {
		var m Message
		if err := json.Unmarshal([]byte(`{"role":"user","content":[`+part+`]}`), &m); err == nil {
			t.Errorf("%s should be rejected, not silently dropped", name)
		}
	}
}

func TestPlainMessagesKeepTheirShape(t *testing.T) {
	b, _ := json.Marshal(Message{Role: "user", Content: "hi"})
	if string(b) != `{"role":"user","content":"hi"}` {
		t.Errorf("got %s", b)
	}
}

func attached() *Call {
	return &Call{MaxTokens: 100, Messages: []Message{{
		Role: "user", Content: "Extract the totals.",
		Attachments: []Attachment{{MediaType: "application/pdf", Data: pdfB64, Name: "t12.pdf"}},
	}}}
}

func TestOpenAISendsPDFAsFilePart(t *testing.T) {
	var got struct {
		Messages []struct {
			Content []map[string]any `json:"content"`
		} `json:"messages"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":900,"completion_tokens":5}}`))
	}))
	defer srv.Close()
	res, err := NewOpenAI(srv.URL, "k", false, "").Chat(context.Background(), attached(), "m", nil)
	if err != nil || res.Text != "ok" || res.Usage.PromptTokens != 900 {
		t.Fatalf("%v %+v", err, res)
	}
	parts := got.Messages[0].Content
	if len(parts) != 2 || parts[0]["type"] != "file" || parts[1]["type"] != "text" || parts[1]["text"] != "Extract the totals." {
		t.Fatalf("parts: %v", parts)
	}
	f := parts[0]["file"].(map[string]any)
	if f["filename"] != "t12.pdf" || !strings.HasPrefix(f["file_data"].(string), "data:application/pdf;base64,") {
		t.Errorf("file part: %v", f)
	}
}

func TestAnthropicSendsPDFAsDocumentBlock(t *testing.T) {
	var got struct {
		Messages []struct {
			Content []map[string]any `json:"content"`
		} `json:"messages"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
		w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":900,"output_tokens":5}}`))
	}))
	defer srv.Close()
	c := attached()
	c.Messages = append(c.Messages, Message{Role: "user", Content: "second turn, plain"})
	if _, err := NewAnthropic(srv.URL, "k").Chat(context.Background(), c, "m", nil); err != nil {
		t.Fatal(err)
	}
	blocks := got.Messages[0].Content
	if len(blocks) != 2 || blocks[0]["type"] != "document" || blocks[1]["type"] != "text" {
		t.Fatalf("blocks: %v", blocks)
	}
	src := blocks[0]["source"].(map[string]any)
	if src["type"] != "base64" || src["media_type"] != "application/pdf" || src["data"] != pdfB64 {
		t.Errorf("source: %v", src)
	}
}

func TestAnthropicKeepsPlainMessagesAsStrings(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
		w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()
	if _, err := NewAnthropic(srv.URL, "k").Chat(context.Background(), call(), "m", nil); err != nil {
		t.Fatal(err)
	}
	msgs := got["messages"].([]any)
	if msgs[0].(map[string]any)["content"] != "hello" {
		t.Errorf("plain message must stay a string: %v", msgs[0])
	}
}
