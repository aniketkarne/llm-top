package redactor

import "testing"

func TestRedactsOpenAIKey(t *testing.T) {
	r := New()
	in := "Authorization: Bearer sk-abcdefghijklmnopqrstuv"
	out := r.Apply(in)
	if out == in {
		t.Fatalf("expected redaction; got same: %q", out)
	}
}

func TestRedactsBearer(t *testing.T) {
	r := New()
	in := "Authorization: Bearer abcdefghijklmnop1234"
	out := r.Apply(in)
	if out == in {
		t.Fatalf("expected redaction")
	}
}

func TestPreservesPlainText(t *testing.T) {
	r := New()
	in := "the quick brown fox jumps over the lazy dog"
	if out := r.Apply(in); out != in {
		t.Fatalf("expected unchanged; got %q", out)
	}
}

func TestMaskHeaderAuth(t *testing.T) {
	r := New()
	got := r.MaskHeader("Authorization", "Bearer sk-abc123")
	if got != "***" {
		t.Fatalf("authorization: want ***, got %q", got)
	}
	got = r.MaskHeader("X-Custom", "value")
	if got != "value" {
		t.Fatalf("custom: want unchanged, got %q", got)
	}
}

func TestNilRedactorSafe(t *testing.T) {
	var r *Redactor
	if out := r.Apply("sk-abc"); out != "sk-abc" {
		t.Fatalf("nil should passthrough, got %q", out)
	}
}
