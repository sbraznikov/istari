package claudecode

import "github.com/sbraznikov/istari/internal/harness"

// price is what a model charges, in US dollars per million tokens.
type price struct {
	input, output, cacheRead, cacheWrite float64
}

// prices are the models' API prices, which Claude Code's own total_cost_usd
// uses too. Taken on 2026-11-27 from Claude Code's total_cost_usd of
// single-model runs in istari's store; the cache write is Claude Code's
// one-hour cache, at twice the input price.
var prices = map[string]price{
	"claude-opus-5-5": {input: 4, output: 20, cacheRead: 0.20, cacheWrite: 8},
}

// highest is the highest rate of the table for each kind of token: the price
// of a model the table lacks, so that the estimate errs toward stopping.
var highest = func() price {
	var h price
	for _, p := range prices {
		h = price{input: max(h.input, p.input), output: max(h.output, p.output),
			cacheRead: max(h.cacheRead, p.cacheRead), cacheWrite: max(h.cacheWrite, p.cacheWrite)}
	}
	return h
}()

// estimate prices one model call by the table: the cost during a run, before
// Claude Code reports its own (spec section 7.5).
func estimate(model string, u harness.Usage) float64 {
	p, ok := prices[model]
	if !ok {
		p = highest
	}
	return (float64(u.InputTokens)*p.input + float64(u.OutputTokens)*p.output +
		float64(u.CacheReadTokens)*p.cacheRead + float64(u.CacheWriteTokens)*p.cacheWrite) / 1e6
}
