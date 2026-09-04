// Package redactor removes common secrets from text before it is written
// to the on-screen log or persisted to the SQLite session dump.
package redactor

import (
	"regexp"
	"strings"
)

// Pattern describes a single secret-detection rule.
type Pattern struct {
	Name  string
	Regex *regexp.Regexp
	Repl  string // replacement template; "***" by default
}

// Default patterns cover common API key formats and bearer tokens.
var defaults = []Pattern{
	{
		Name:  "openai_key",
		Regex: regexp.MustCompile(`sk-[A-Za-z0-9]{20,}`),
		Repl:  "sk-***",
	},
	{
		Name:  "anthropic_key",
		Regex: regexp.MustCompile(`sk-ant-[A-Za-z0-9-]{20,}`),
		Repl:  "sk-ant-***",
	},
	{
		Name:  "bearer",
		Regex: regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]{16,}`),
		Repl:  "Bearer ***",
	},
	{
		Name:  "github_pat",
		Regex: regexp.MustCompile(`ghp_[A-Za-z0-9]{20,}`),
		Repl:  "ghp_***",
	},
	{
		Name:  "aws_access_key",
		Regex: regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
		Repl:  "AKIA***",
	},
	{
		Name:  "google_api_key",
		Regex: regexp.MustCompile(`AIza[0-9A-Za-z\-_]{35}`),
		Repl:  "AIza***",
	},
}

// Redactor holds an ordered list of patterns applied to text.
type Redactor struct {
	patterns []Pattern
}

// New returns a Redactor with the default patterns enabled.
func New() *Redactor {
	r := &Redactor{}
	r.patterns = append(r.patterns, defaults...)
	return r
}

// Add appends a custom pattern.
func (r *Redactor) Add(p Pattern) {
	r.patterns = append(r.patterns, p)
}

// Apply returns a copy of s with any matching secrets replaced.
func (r *Redactor) Apply(s string) string {
	if r == nil {
		return s
	}
	out := s
	for _, p := range r.patterns {
		out = p.Regex.ReplaceAllString(out, p.Repl)
	}
	return out
}

// MaskHeader scrubs an HTTP header value while preserving the name.
func (r *Redactor) MaskHeader(name, value string) string {
	lower := strings.ToLower(name)
	if lower == "authorization" || lower == "x-api-key" || lower == "api-key" {
		return "***"
	}
	return r.Apply(value)
}
