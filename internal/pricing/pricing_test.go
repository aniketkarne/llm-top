package pricing

import (
	"math"
	"testing"
)

func TestDefaultsContainThreeModels(t *testing.T) {
	d := Defaults()
	want := []string{"gpt-5.5", "gpt-5.5-mini", "claude-sonnet-5"}
	for _, m := range want {
		if _, _, ok := d.Lookup(m); !ok {
			t.Errorf("expected default for %q, got missing", m)
		}
	}
	// And nothing else — guard against accidental default additions.
	for _, m := range []string{"gpt-4o", "gpt-4o-mini", "gpt-3.5-turbo", "claude-3-haiku"} {
		if _, _, ok := d.Lookup(m); ok {
			t.Errorf("expected no default for legacy model %q", m)
		}
	}
}

func TestLookupUnknown(t *testing.T) {
	if _, _, ok := Defaults().Lookup("does-not-exist"); ok {
		t.Fatal("unknown model should return ok=false")
	}
}

func TestLookupValuesExact(t *testing.T) {
	in, out, ok := Defaults().Lookup("gpt-5.5")
	if !ok {
		t.Fatal("gpt-5.5 missing")
	}
	if in != 5.00 || out != 30.00 {
		t.Errorf("gpt-5.5 rate: got (%v,%v), want (5.00,30.00)", in, out)
	}
	in, out, ok = Defaults().Lookup("gpt-5.5-mini")
	if !ok || in != 0.50 || out != 2.00 {
		t.Errorf("gpt-5.5-mini rate: got (%v,%v) ok=%v", in, out, ok)
	}
	in, out, ok = Defaults().Lookup("claude-sonnet-5")
	if !ok || in != 2.00 || out != 10.00 {
		t.Errorf("claude-sonnet-5 rate: got (%v,%v) ok=%v", in, out, ok)
	}
}

func TestWithOverridesMerge(t *testing.T) {
	d := Defaults()
	o := d.With(map[string]Rate{
		"gpt-5.5":      {InputPer1M: 9.99, OutputPer1M: 19.99}, // override
		"custom-model": {InputPer1M: 1.23, OutputPer1M: 4.56},  // new
	})
	in, _, ok := o.Lookup("gpt-5.5")
	if !ok || in != 9.99 {
		t.Errorf("override not applied: got in=%v ok=%v", in, ok)
	}
	if _, _, ok := o.Lookup("custom-model"); !ok {
		t.Errorf("new entry missing")
	}
	// Defaults table must be untouched.
	in, _, ok = d.Lookup("gpt-5.5")
	if !ok || in != 5.00 {
		t.Errorf("defaults mutated: got in=%v", in)
	}
}

func TestWithEmptyMapReturnsEquivalentTable(t *testing.T) {
	d := Defaults()
	o := d.With(nil)
	for _, model := range []string{"gpt-5.5", "gpt-5.5-mini", "claude-sonnet-5"} {
		di, do, dok := d.Lookup(model)
		oi, oo, ook := o.Lookup(model)
		if !dok || !ook || di != oi || do != oo {
			t.Errorf("With(nil) should preserve %q", model)
		}
	}
}

func TestCostMath(t *testing.T) {
	d := Defaults()
	// gpt-5.5: 1M in + 1M out -> 5 + 30 = $35.00
	c, ok := d.Cost("gpt-5.5", 1_000_000, 1_000_000)
	if !ok {
		t.Fatal("cost: ok=false")
	}
	if math.Abs(c-35.0) > 1e-9 {
		t.Errorf("cost math: got %v, want 35.0", c)
	}
	// gpt-5.5-mini: 8,421 in + 734 out -> 0.50 * 8.421e-3 + 2.00 * 734e-6
	// = 4.2105e-3 + 1.468e-3 = 5.6785e-3
	c, ok = d.Cost("gpt-5.5-mini", 8421, 734)
	if !ok {
		t.Fatal("mini cost: ok=false")
	}
	want := 0.50*8.421/1000.0 + 2.00*0.734/1000.0
	if math.Abs(c-want) > 1e-9 {
		t.Errorf("mini cost: got %v, want %v", c, want)
	}
	// Unknown model returns false, no panic.
	if _, ok := d.Cost("gpt-4o", 100, 100); ok {
		t.Error("unknown model should not return a cost")
	}
	// Zero tokens is fine and returns 0.
	c, ok = d.Cost("gpt-5.5", 0, 0)
	if !ok || c != 0 {
		t.Errorf("zero-tokens cost: got %v ok=%v", c, ok)
	}
}

func TestTopLevelLookup(t *testing.T) {
	// The package-level Lookup should behave identically to Defaults().
	in1, out1, ok1 := Lookup("gpt-5.5")
	d := Defaults()
	in2, out2, ok2 := d.Lookup("gpt-5.5")
	if in1 != in2 || out1 != out2 || ok1 != ok2 {
		t.Errorf("package Lookup differs from Defaults.Lookup: (%v,%v,%v) vs (%v,%v,%v)",
			in1, out1, ok1, in2, out2, ok2)
	}
}