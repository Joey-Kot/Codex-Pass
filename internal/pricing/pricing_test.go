package pricing

import (
	"math/big"
	"path/filepath"
	"strings"
	"testing"
)

const validPrices = `{
  "currency": "USD",
  "unit": "per_token",
  "service_tier": "standard",
  "context_threshold": 272000,
  "models": {
    "test-model": {
      "short_context": {"input": 0.00001, "cached_input": 0.000001, "output": 0.00005},
      "long_context": {"input": 0.00002, "cached_input": 0.000002, "output": 0.000075}
    }
  }
}`

func TestCostUsesEachRequestsContextAndTokenCategories(t *testing.T) {
	table, err := decode(strings.NewReader(validPrices))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name                  string
		input, cached, output int64
		want                  string
	}{
		{name: "short cached input", input: 56510, cached: 49520, output: 500, want: "0.14442"},
		{name: "threshold is short", input: 272000, cached: 20000, output: 1000, want: "2.59"},
		{name: "above threshold is long", input: 272001, cached: 20000, output: 1000, want: "5.15502"},
		{name: "cached input counts toward threshold", input: 300000, cached: 290000, output: 1000, want: "0.855"},
		{name: "all cached", input: 1000, cached: 1000, want: "0.001"},
		{name: "output does not change context tier", input: 1, output: 300000, want: "15.00001"},
		{name: "zero usage", want: "0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := table.Cost("test-model", &test.input, &test.cached, &test.output)
			want, _ := new(big.Rat).SetString(test.want)
			if got == nil || got.Cmp(want) != 0 {
				t.Fatalf("cost = %v, want exactly %s", got, test.want)
			}
		})
	}
	for _, test := range []struct {
		model                 string
		input, cached, output *int64
	}{
		{model: "unknown", input: intPointer(1), cached: intPointer(0), output: intPointer(0)},
		{model: "test-model", cached: intPointer(0), output: intPointer(0)},
		{model: "test-model", input: intPointer(1), output: intPointer(0)},
		{model: "test-model", input: intPointer(1), cached: intPointer(0)},
		{model: "test-model", input: intPointer(1), cached: intPointer(2), output: intPointer(0)},
		{model: "test-model", input: intPointer(1), cached: intPointer(0), output: intPointer(-1)},
	} {
		if got := table.Cost(test.model, test.input, test.cached, test.output); got != nil {
			t.Errorf("missing or invalid pricing/usage produced %v", got)
		}
	}
}

func TestPriceFormattingRoundsForDisplay(t *testing.T) {
	for _, test := range []struct {
		rate string
		want string
	}{
		{rate: "0.1", want: "USD 0.30"},
		{rate: "1e-9", want: "USD 0.00"},
		{rate: "1.2345E-5", want: "USD 0.00004"},
		{rate: "0.33333", want: "USD 0.99999"},
		{rate: "0.333333", want: "USD 1.00"},
		{rate: "0.333334", want: "USD 1.00"},
		{rate: "1.23456", want: "USD 3.704"},
		{rate: "1.2e2", want: "USD 360.00"},
		{rate: "0", want: "USD 0.00"},
	} {
		t.Run(test.rate, func(t *testing.T) {
			input := strings.Replace(validPrices, `"input": 0.00001`, `"input": `+test.rate, 1)
			table, err := decode(strings.NewReader(input))
			if err != nil {
				t.Fatal(err)
			}
			cost := table.Cost("test-model", intPointer(3), intPointer(0), intPointer(0))
			if got := table.Format(cost); got != test.want {
				t.Fatalf("formatted cost = %q, want %q", got, test.want)
			}
		})
	}
}

func TestPriceTableRejectsInvalidData(t *testing.T) {
	for _, test := range []struct {
		name, old, replacement, wantError string
	}{
		{name: "missing currency", old: `"currency": "USD",`, wantError: "currency is required"},
		{name: "wrong unit", old: `"per_token"`, replacement: `"per_million_tokens"`, wantError: "expected per_token"},
		{name: "missing service tier", old: `"service_tier": "standard",`, wantError: "service_tier is required"},
		{name: "missing threshold", old: `"context_threshold": 272000,`, wantError: "positive integer"},
		{name: "zero threshold", old: "272000", replacement: "0", wantError: "positive integer"},
		{name: "fractional threshold", old: "272000", replacement: "272000.5", wantError: "invalid price table JSON"},
		{name: "missing rate", old: `"input": 0.00001,`, wantError: "short_context.input must be a non-negative number"},
		{name: "negative rate", old: `"input": 0.00001`, replacement: `"input": -0.00001`, wantError: "non-negative number"},
		{name: "null rate", old: `"input": 0.00001`, replacement: `"input": null`, wantError: "non-negative number"},
		{name: "quoted rate", old: `"input": 0.00001`, replacement: `"input": "0.00001"`, wantError: "non-negative number"},
		{name: "unknown field", old: `"cached_input"`, replacement: `"cache_input"`, wantError: "unknown field"},
		{name: "missing long context", old: `"long_context": {"input": 0.00002, "cached_input": 0.000002, "output": 0.000075}`, replacement: `"long_context": null`, wantError: "long_context is required"},
		{name: "empty model name", old: `"test-model"`, replacement: `""`, wantError: "model name must not be empty"},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := strings.Replace(validPrices, test.old, test.replacement, 1)
			if input == validPrices {
				t.Fatal("test did not modify the fixture")
			}
			if _, err := decode(strings.NewReader(input)); err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("error = %v, want %q", err, test.wantError)
			}
		})
	}
	for _, input := range []string{"{", "[]", "null", validPrices + " {}", validPrices + " invalid"} {
		if _, err := decode(strings.NewReader(input)); err == nil {
			t.Errorf("invalid JSON unexpectedly accepted: %s", input)
		}
	}
}

func TestLoadExamplePriceTable(t *testing.T) {
	table, err := Load(filepath.Join("..", "..", "prices.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	if table.Currency != "USD" || table.ServiceTier != "standard" || table.ContextThreshold != 272000 {
		t.Fatalf("unexpected example price table: %#v", table)
	}
	cost := table.Cost("gpt-6-luna", intPointer(1), intPointer(1), intPointer(0))
	if cost == nil || cost.Cmp(big.NewRat(1, 100000000)) != 0 {
		t.Fatalf("smallest example rate = %v, want exactly 0.00000001", cost)
	}
	if got := table.Format(nil); got != "-" {
		t.Fatalf("missing cost = %q", got)
	}
}

func intPointer(value int64) *int64 { return &value }
