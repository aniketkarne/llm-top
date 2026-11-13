package pricing

import (
	"math"
	"testing"
)

// currentModels is the full expected default set — every row must be
// present with the exact rate, and the count must match so an
// accidental addition or removal fails loudly. Prices verified
// 2026-11-13 against the OpenAI and Anthropic price sheets.
var currentModels = map[string]Rate{
	"gpt-6-luna":        {InputPer1M: 0.10, OutputPer1M: 0.50},
	"gpt-6.1-terra":     {InputPer1M: 1.00, OutputPer1M: 5.00},
	"gpt-6.1-sol":       {InputPer1M: 2.00, OutputPer1M: 10.00},
	"claude-haiku-5-5":  {InputPer1M: 0.10, OutputPer1M: 0.50},
	"claude-sonnet-5-5": {InputPer1M: 2.00, OutputPer1M: 10.00},
	"claude-opus-5-5":   {InputPer1M: 4.00, OutputPer1M: 20.00},
}

func TestDefaultsContainCurrentModels(t *testing.T) {
	d := Defaults()
	for m, want := range currentModels {
		in, out, ok := d.Lookup(m)
		if !ok {
			t.Errorf("expected default for %q, got missing", m)
			continue
		}
		if in != want.InputPer1M || out != want.OutputPer1M {
			t.Errorf("%q rate: got (%v,%v), want (%v,%v)",
				m, in, out, want.InputPer1M, want.OutputPer1M)
		}
	}
	// Guard against accidental default additions: the table is
	// exactly the verified set, nothing more.
	if len(d.rates) != len(currentModels) {
		t.Errorf("default table has %d entries, want %d", len(d.rates), len(currentModels))
	}
	// Retired and legacy models must not be priced — unknown models
	// render "$?", never a fabricated number from stale data.
	for _, m := range []string{
		"gpt-5.5", "gpt-5.5-mini", "claude-sonnet-5",
		"gpt-4o", "gpt-4o-mini", "gpt-3.5-turbo", "claude-3-haiku",
	} {
		if _, _, ok := d.Lookup(m); ok {
			t.Errorf("expected no default for retired/legacy model %q", m)
		}
	}
	// Dotted and vendor-prefixed spellings are not guessed at.
	for _, m := range []string{"anthropic/claude-sonnet-5-5", "claude-sonnet-5.5"} {
		if _, _, ok := d.Lookup(m); ok {
			t.Errorf("expected no default for non-native spelling %q", m)
		}
	}
}

func TestLookupUnknown(t *testing.T) {
	if _, _, ok := Defaults().Lookup("does-not-exist"); ok {
		t.Fatal("unknown model should return ok=false")
	}
}

func TestWithOverridesMerge(t *testing.T) {
	d := Defaults()
	o := d.With(map[string]Rate{
		"gpt-6-luna":   {InputPer1M: 9.99, OutputPer1M: 19.99}, // override
		"custom-model": {InputPer1M: 1.23, OutputPer1M: 4.56},  // new
	})
	in, _, ok := o.Lookup("gpt-6-luna")
	if !ok || in != 9.99 {
		t.Errorf("override not applied: got in=%v ok=%v", in, ok)
	}
	if _, _, ok := o.Lookup("custom-model"); !ok {
		t.Errorf("new entry missing")
	}
	// Defaults table must be untouched.
	in, _, ok = d.Lookup("gpt-6-luna")
	if !ok || in != 0.10 {
		t.Errorf("defaults mutated: got in=%v", in)
	}
}

func TestWithEmptyMapReturnsEquivalentTable(t *testing.T) {
	d := Defaults()
	o := d.With(nil)
	for model := range currentModels {
		di, do, dok := d.Lookup(model)
		oi, oo, ook := o.Lookup(model)
		if !dok || !ook || di != oi || do != oo {
			t.Errorf("With(nil) should preserve %q", model)
		}
	}
}

func TestCostMath(t *testing.T) {
	d := Defaults()
	// gpt-6.1-sol: 1M in + 1M out -> 2 + 10 = $12.00
	c, ok := d.Cost("gpt-6.1-sol", 1_000_000, 1_000_000)
	if !ok {
		t.Fatal("cost: ok=false")
	}
	if math.Abs(c-12.0) > 1e-9 {
		t.Errorf("cost math: got %v, want 12.0", c)
	}
	// gpt-6-luna: 8,421 in + 734 out -> 0.10 * 8.421e-3 + 0.50 * 734e-6
	c, ok = d.Cost("gpt-6-luna", 8421, 734)
	if !ok {
		t.Fatal("luna cost: ok=false")
	}
	want := 0.10*8.421/1000.0 + 0.50*0.734/1000.0
	if math.Abs(c-want) > 1e-9 {
		t.Errorf("luna cost: got %v, want %v", c, want)
	}
	// Unknown model returns false, no panic.
	if _, ok := d.Cost("gpt-4o", 100, 100); ok {
		t.Error("unknown model should not return a cost")
	}
	// Zero tokens is fine and returns 0.
	c, ok = d.Cost("gpt-6.1-sol", 0, 0)
	if !ok || c != 0 {
		t.Errorf("zero-tokens cost: got %v ok=%v", c, ok)
	}
}

func TestTopLevelLookup(t *testing.T) {
	// The package-level Lookup should behave identically to Defaults().
	in1, out1, ok1 := Lookup("gpt-6-luna")
	d := Defaults()
	in2, out2, ok2 := d.Lookup("gpt-6-luna")
	if in1 != in2 || out1 != out2 || ok1 != ok2 {
		t.Errorf("package Lookup differs from Defaults.Lookup: (%v,%v,%v) vs (%v,%v,%v)",
			in1, out1, ok1, in2, out2, ok2)
	}
}
