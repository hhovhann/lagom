package gateway

import (
	"lagom/internal/providers"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The prompt-injection guard is a cheap, deterministic screen that runs before any
// model call or cache lookup. It looks for the commodity attacks (instruction
// override, system-prompt exfiltration, forged role markers, jailbreak personas)
// in every non-system message, because that is where untrusted text arrives
// (users, retrieved documents, tool output).
//
// It is NOT a complete defence. It does not catch paraphrases, other languages,
// encoded payloads or attacks hidden in images. Treat it as one layer: it makes
// attacks visible (flag mode) or stops the obvious ones (block mode), and the
// structural defences (judge isolation, fail-closed grading, spend cap, auth)
// still apply. A model-based classifier can sit behind the same interface later.

// scanLimit bounds work per message (bytes): the head and the tail are scanned,
// since injected text is usually appended to or prepended to the real content.
const scanLimit = 16 << 10

type guardRule struct {
	name  string
	hints []string // cheap substring prefilter: the regex only runs if one is present
	re    *regexp.Regexp
}

var guardRules = []guardRule{
	{"override", []string{"ignore", "disregard", "forget", "override", "bypass"}, regexp.MustCompile(`\b(ignore|disregard|forget|override|bypass)\b[^.\n]{0,40}\b(previous|prior|above|earlier|preceding|all|any|your|the)\b[^.\n]{0,30}\b(instructions?|prompts?|rules?|guidelines?|directions?|context|polic(y|ies))\b`)},
	{"override", []string{"instruction"}, regexp.MustCompile(`\b(new|updated|real|actual) instructions?\s*:`)},
	{"override", []string{"from now on"}, regexp.MustCompile(`\bfrom now on,? (you|ignore|only|always|never)\b`)},
	{"persona", []string{"you are now", "you are no longer"}, regexp.MustCompile(`\byou are (now|no longer)\b`)},
	{"persona", []string{"act as", "behave as", "respond as"}, regexp.MustCompile(`\b(act|behave|respond) as (if )?(you (are|were)|an? )?(unrestricted|unfiltered|jailbroken|dan)\b`)},
	{"persona", []string{" mode", "do anything now", "jailbreak"}, regexp.MustCompile(`\b(developer|god|sudo|admin) mode\b|\bdo anything now\b|\bjailbreak\b`)},
	{"exfiltration", []string{"prompt", "instruction", "message", "rules"}, regexp.MustCompile(`\b(reveal|show|print|repeat|output|display|leak|tell me|give me|what (is|are))\b[^.\n]{0,30}\b(your|the)\b[^.\n]{0,20}\b(system|hidden|initial|original|secret) ?(prompt|instructions?|message|rules)\b`)},
	{"exfiltration", []string{"key", "credential", "password", "token"}, regexp.MustCompile(`\b(api[ _-]?keys?|secret keys?|credentials|passwords?|bearer tokens?)\b[^.\n]{0,30}\b(reveal|show|print|send|output|leak|exfiltrate)\b|\b(reveal|show|print|send|output|leak|exfiltrate)\b[^.\n]{0,30}\b(api[ _-]?keys?|secret keys?|credentials|passwords?|bearer tokens?)\b`)},
	{"role-spoof", []string{"system:", "assistant:", "developer:"}, regexp.MustCompile(`(^|\n)\s*(#{1,4}\s*)?(system|assistant|developer)\s*:`)},
	{"role-spoof", []string{"<|", "[inst", "[/inst", "<<sys", "system>"}, regexp.MustCompile(`<\|(im_start|im_end|system|endoftext)\|>|\[/?inst\]|<<sys>>|</?system>`)},
}

func (r guardRule) maybe(text string) bool {
	for _, h := range r.hints {
		if strings.Contains(text, h) {
			return true
		}
	}
	return false
}

// invisible reports runes attackers use to split keywords (zero-width and soft hyphen).
func invisible(r rune) bool {
	switch r {
	case '\u200b', '\u200c', '\u200d', '\u200e', '\u200f', '\u2060', '\ufeff', '\u00ad':
		return true
	}
	return false
}

// normalise lowercases, drops invisible runes and collapses runs of spaces/tabs in one pass.
// Newlines are kept: the role-marker rules depend on line starts.
func normalise(s string) string {
	if len(s) > 2*scanLimit {
		s = s[:scanLimit] + "\n" + s[len(s)-scanLimit:]
	}
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		switch {
		case invisible(r):
			continue
		case r == ' ' || r == '\t' || r == '\r' || r == '\f' || r == '\v':
			if !space {
				b.WriteByte(' ')
			}
			space = true
			continue
		}
		space = false
		if r < utf8.RuneSelf {
			if 'A' <= r && r <= 'Z' {
				r += 'a' - 'A'
			}
			b.WriteByte(byte(r))
		} else {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// Guard scans messages and reports which rule families matched (sorted, unique).
type Guard struct{ mode string }

func newGuard(mode string) *Guard { return &Guard{mode: mode} }

func (g *Guard) enabled() bool { return g != nil && (g.mode == "flag" || g.mode == "block") }
func (g *Guard) blocks() bool  { return g != nil && g.mode == "block" }

// Scan checks every message except those with role "system": the system prompt is
// written by the application owner and is trusted by definition.
func (g *Guard) Scan(msgs []providers.Message) []string {
	if !g.enabled() {
		return nil
	}
	hit := map[string]bool{}
	for _, m := range msgs {
		if m.Role == "system" {
			continue
		}
		text := normalise(string(m.Content))
		for _, r := range guardRules {
			if !hit[r.name] && r.maybe(text) && r.re.MatchString(text) {
				hit[r.name] = true
			}
		}
	}
	if len(hit) == 0 {
		return nil
	}
	out := make([]string, 0, len(hit))
	for k := range hit {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
