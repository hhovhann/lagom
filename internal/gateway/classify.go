package gateway

import (
	"context"
	"lagom/internal/providers"
	"log"
	"regexp"
	"strings"
	"time"
)

var digitRun = regexp.MustCompile(`\d+`)

// classify names the task class when the caller did not send X-Lagom-Task.
// It is deliberately cheap (no model call): keyword heuristics on the last user
// message. Production would use caller-supplied task IDs, route/prompt
// templates, or a small learned classifier; the interface stays the same.
func classify(msgs []providers.Message) string {
	last := ""
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			last = strings.ToLower(string(msgs[i].Content))
			break
		}
	}
	switch {
	case strings.Contains(last, "json") || strings.Contains(last, "extract"):
		return "extract"
	case strings.Contains(last, "classif") || strings.Contains(last, "categor") || strings.Contains(last, "label"):
		return "classify"
	case len(digitRun.FindAllString(last, -1)) >= 2 &&
		(strings.Contains(last, "how many") || strings.Contains(last, "total") ||
			strings.Contains(last, "calculate") || strings.Contains(last, "remain")):
		return "reason"
	}
	return "general"
}

// taskOf names the task type: the caller's X-Lagom-Task if present; else the embedding
// classifier when configured (a prompt it is unsure about, or any failure, is
// "unclassified" and is held on the most capable model); else keyword heuristics. The
// classifier costs one embedding call, so it runs only when auto is true.
func (s *Server) taskOf(ctx context.Context, header string, msgs []providers.Message, auto bool) string {
	if header != "" {
		return header
	}
	if s.classifier == nil || !auto {
		return classify(msgs)
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	res, err := s.classifier.Classify(ctx, lastUser(msgs))
	if err != nil {
		log.Printf("classifier: %v", err)
		return unclassified
	}
	if res.Task == "" {
		s.unclassifiedN.Add(1)
		return unclassified
	}
	return res.Task
}

func lastUser(msgs []providers.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return string(msgs[i].Content)
		}
	}
	return ""
}
