// Package pricing answers "how much did this cost in money", the question the
// token counts alone cannot answer. It is deliberately OPT-IN and
// hand-declared: bh has no price catalog (the OpenAI /models endpoint does not
// publish prices, and the provider behind it may be anything), so the numbers
// come from the configuration, per model.
//
// Prices are USD per MILLION tokens, because that is how every provider
// publishes them (and keeps the decimals readable: 2.5 = US$ 2.50 per 1M).
// Absent model = zero price = no cost shown, never a guessed number.
package pricing

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Price is a model's price per million tokens. CachedIn/PromptCacheRead cost
// applies only to the part of the prompt the provider served from its cache
// (usage.prompt_tokens_details.cached_tokens); when it is 0 the full prompt
// price is used for the whole prompt, which is the safe assumption for
// providers that do not report a cache.
type Price struct {
	InUSDPerM   float64 `json:"in_usd_per_million"`
	OutUSDPerM  float64 `json:"out_usd_per_million"`
	CacheUSDPerM float64 `json:"cache_usd_per_million,omitempty"`
}

// Tabela is a set of model prices. Optional: bh runs without it.
type Tabela struct {
	Modelos map[string]Price `json:"modelos"`
}

// Empty reports whether there is any price to apply.
func (t Tabela) Empty() bool { return len(t.Modelos) == 0 }

// Price looks a model up, accepting an exact id ("gpt-4o-mini") or the
// provider/id form the catalog uses ("openai/gpt-4o-mini"): the segment after
// the last "/" is tried too, so MixerMoonshot-style ids resolve.
func (t Tabela) Price(model string) (Price, bool) {
	if t.Empty() || strings.TrimSpace(model) == "" {
		return Price{}, false
	}
	if p, ok := t.Modelos[model]; ok {
		return p, true
	}
	if i := strings.LastIndex(model, "/"); i >= 0 && i+1 < len(model) {
		if p, ok := t.Modelos[model[i+1:]]; ok {
			return p, true
		}
	}
	return Price{}, false
}

// Cost returns the cost in USD of a usage with the model's price. Zero when
// the model has no declared price (never estimated).
type Cost struct {
	USD      float64
	HasPrice bool
}

// Calc computes the cost of one request. promptIn is the part of the prompt NOT
// served from cache, cached the part that was: a provider reporting no cache
// should pass cached=0 and everything is billed at the full prompt price.
func (p Price) Calc(promptIn, cached, output int) Cost {
	if p.InUSDPerM == 0 && p.OutUSDPerM == 0 {
		return Cost{}
	}
	perM := 1_000_000.0
	usd := float64(promptIn) / perM * p.InUSDPerM
	if cached > 0 {
		rate := p.CacheUSDPerM
		if rate == 0 {
			rate = p.InUSDPerM
		}
		usd += float64(cached) / perM * rate
	}
	usd += float64(output) / perM * p.OutUSDPerM
	return Cost{USD: usd, HasPrice: true}
}

// Load reads an optional prices.json from the harness home. A missing file is
// NOT an error: bh works with no prices at all (tokens remain the metric).
func Load(home string) (Tabela, error) {
	var t Tabela
	path := filepath.Join(home, "prices.json")
	b, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		return t, nil
	case err != nil:
		return t, err
	}
	if err := json.Unmarshal(b, &t); err != nil {
		return t, fmt.Errorf("prices %s: %w", path, err)
	}
	return t, nil
}

// Formato renders a cost the way OpenCode does in its sidebar ("$0.12"): two
// decimals below a dollar, four above, so a heavy session does not read "$0.00".
func Formato(usd float64) string {
	switch {
	case usd <= 0:
		return ""
	case usd < 1:
		return fmt.Sprintf("$%.2f", usd)
	case usd < 100:
		return fmt.Sprintf("$%.2f", usd)
	default:
		return fmt.Sprintf("$%.2fk", usd/1000)
	}
}
