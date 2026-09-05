package redactor

import "testing"

// --- Hardening: edge-case inputs ---

func TestRedactEmpty(t *testing.T) {
	r := New()
	if got := r.Apply(""); got != "" {
		t.Errorf("empty input: want '', got %q", got)
	}
}

func TestRedactNilSafe(t *testing.T) {
	// Nil receiver must not panic.
	var r *Redactor
	defer func() {
		if rec := recover(); rec != nil {
			t.Errorf("nil Redactor.Apply panicked: %v", rec)
		}
	}()
	got := r.Apply("hello sk-1234567890abcdefghij")
	if got != "hello sk-1234567890abcdefghij" {
		// nil receiver is a pass-through, so unchanged.
		t.Errorf("nil redactor should be pass-through, got %q", got)
	}
}

func TestRedactMultipleKeysInOneText(t *testing.T) {
	r := New()
	in := "key1=sk-abcdefghij1234567890 and sk-zyxwvu9876543210abcdef"
	got := r.Apply(in)
	if got == in {
		t.Errorf("expected redaction of OpenAI keys, got unchanged: %q", got)
	}
	// Both keys must be redacted.
	if contains(got, "sk-abcdefghij1234567890") {
		t.Errorf("first key not redacted: %q", got)
	}
	if contains(got, "sk-zyxwvu9876543210abcdef") {
		t.Errorf("second key not redacted: %q", got)
	}
}

func TestRedactBearerCapitalization(t *testing.T) {
	r := New()
	cases := []string{
		"Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.payload.signature",
		"authorization: bearer eyJhbGciOiJIUzI1NiJ9.payload.signature",
		"AUTHORIZATION: BEARER eyJhbGciOiJIUzI1NiJ9.payload.signature",
	}
	for _, in := range cases {
		got := r.Apply(in)
		if got == in {
			t.Errorf("bearer variant not redacted: %q", got)
		}
	}
}

func TestRedactInJSONBody(t *testing.T) {
	r := New()
	in := `{"Authorization": "Bearer sk-abcdef1234567890abcdef", "data": "ok"}`
	got := r.Apply(in)
	if contains(got, "sk-abcdef1234567890abcdef") {
		t.Errorf("key inside JSON not redacted: %q", got)
	}
}

func TestRedactInUnicodeText(t *testing.T) {
	r := New()
	in := "API key is sk-abcdefghij1234567890 — 日本語 🚀"
	got := r.Apply(in)
	if contains(got, "sk-abcdefghij1234567890") {
		t.Errorf("key in unicode not redacted: %q", got)
	}
	if !contains(got, "日本語") {
		t.Errorf("unicode text mangled: %q", got)
	}
}

func TestRedactHugeInput(t *testing.T) {
	r := New()
	// 1 MB of benign text plus a single embedded secret.
	huge := make([]byte, 0, 1024*1024+200)
	benign := "this is a normal sentence. "
	for i := 0; i < 1024*1024/len(benign); i++ {
		huge = append(huge, benign...)
	}
	huge = append(huge, []byte("key=sk-redactmeplease1234567890")...)
	got := r.Apply(string(huge))
	if contains(got, "sk-redactmeplease1234567890") {
		t.Errorf("huge input: secret not redacted")
	}
}

func TestRedactPreservesSurroundingStructure(t *testing.T) {
	r := New()
	in := "[before] sk-abcdefghij1234567890 [after]"
	got := r.Apply(in)
	if !contains(got, "[before]") || !contains(got, "[after]") {
		t.Errorf("surrounding markers mangled: %q", got)
	}
}

func TestMaskHeaderAuthVariants(t *testing.T) {
	r := New()
	cases := []string{
		"x-api-key: sk-abcdefghij1234567890",
		"X-Api-Key: sk-abcdefghij1234567890",
		"X-API-KEY: sk-abcdefghij1234567890",
	}
	for _, in := range cases {
		got := r.Apply(in)
		if contains(got, "sk-abcdefghij1234567890") {
			t.Errorf("variant %q: not redacted: %q", in, got)
		}
	}
}

// --- helpers ---

func contains(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}