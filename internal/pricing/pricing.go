// Package pricing holds the model -> USD-per-million-token rates used
// to estimate the cost of a captured request. The defaults cover the
// models that ship with llm-top; users can extend or override them via
// the Table.With helper or the --pricing CLI flag in Step 3.
//
// Prices are expressed per 1,000,000 tokens (input and output are
// billed separately). All prices are in US dollars.
package pricing

// Rate describes the cost to bill a model per one million tokens,
// split between input and output.
type Rate struct {
	InputPer1M  float64
	OutputPer1M float64
}

// Table is an immutable map of model name to Rate. The zero value is
// usable but has no entries; callers should start from Defaults.
type Table struct {
	rates map[string]Rate
}

// Defaults returns the built-in pricing table. As of 2026-09-13 the
// covered models are:
//
//	gpt-5.5        $5.00 in  / $30.00 out per 1M
//	gpt-5.5-mini   $0.50 in  /  $2.00 out per 1M
//	claude-sonnet-5 $2.00 in  / $10.00 out per 1M
//
// No 4o or older models are included by design.
func Defaults() Table {
	return Table{rates: map[string]Rate{
		"gpt-5.5":         {InputPer1M: 5.00, OutputPer1M: 30.00},
		"gpt-5.5-mini":    {InputPer1M: 0.50, OutputPer1M: 2.00},
		"claude-sonnet-5": {InputPer1M: 2.00, OutputPer1M: 10.00},
	}}
}

// IsZero reports whether the table has no entries. Used by callers that
// want to know whether they should fall back to Defaults.
func (t Table) IsZero() bool {
	return len(t.rates) == 0
}
func (t Table) Lookup(model string) (in, out float64, ok bool) {
	if t.rates == nil {
		return 0, 0, false
	}
	r, ok := t.rates[model]
	if !ok {
		return 0, 0, false
	}
	return r.InputPer1M, r.OutputPer1M, true
}

// With returns a new Table where the supplied overrides win over any
// existing entry for the same model. Existing entries are preserved.
func (t Table) With(overrides map[string]Rate) Table {
	out := Table{rates: make(map[string]Rate, len(t.rates)+len(overrides))}
	for k, v := range t.rates {
		out.rates[k] = v
	}
	for k, v := range overrides {
		out.rates[k] = v
	}
	return out
}

// Cost computes the USD cost for a single request that used
// promptTokens input and outputTokens output tokens. Returns (0,
// false) when the model is not in the table so callers can render
// "$?" rather than fabricating a number.
//
// The math is:
//
//	cost = promptTokens / 1e6 * inputPer1M +
//	       outputTokens / 1e6 * outputPer1M
func (t Table) Cost(model string, promptTokens, outputTokens int) (float64, bool) {
	in, out, ok := t.Lookup(model)
	if !ok {
		return 0, false
	}
	cost := float64(promptTokens)/1_000_000.0*in +
		float64(outputTokens)/1_000_000.0*out
	return cost, true
}

// Lookup is a convenience wrapper around Defaults().Lookup. Useful
// when callers don't need to customize the table.
func Lookup(model string) (in, out float64, ok bool) {
	return Defaults().Lookup(model)
}