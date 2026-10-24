// Package pricing estimates request costs using a user-provided per-token price
// table. Decimal rates and totals are kept exact until they are displayed.
package pricing

import (
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"os"
	"strings"
)

type Table struct {
	Currency         string
	Unit             string
	ServiceTier      string
	ContextThreshold int64
	models           map[string]modelRates
}

type modelRates struct {
	short, long rates
}

type rates struct {
	input, cachedInput, output *big.Rat
}

type rateFields struct {
	Input       json.RawMessage `json:"input"`
	CachedInput json.RawMessage `json:"cached_input"`
	Output      json.RawMessage `json:"output"`
}

type tableFields struct {
	Currency         string `json:"currency"`
	Unit             string `json:"unit"`
	ServiceTier      string `json:"service_tier"`
	ContextThreshold *int64 `json:"context_threshold"`
	Models           map[string]struct {
		Short *rateFields `json:"short_context"`
		Long  *rateFields `json:"long_context"`
	} `json:"models"`
}

func Load(path string) (*Table, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open price table: %w", err)
	}
	defer file.Close()
	table, err := decode(file)
	if err != nil {
		return nil, fmt.Errorf("read price table %q: %w", path, err)
	}
	return table, nil
}

func decode(reader io.Reader) (*Table, error) {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	var fields tableFields
	if err := decoder.Decode(&fields); err != nil {
		return nil, fmt.Errorf("invalid price table JSON: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("price table must contain exactly one JSON object")
	}
	if strings.TrimSpace(fields.Currency) == "" {
		return nil, fmt.Errorf("currency is required")
	}
	if fields.Unit != "per_token" {
		return nil, fmt.Errorf("unsupported price unit %q: expected per_token", fields.Unit)
	}
	if strings.TrimSpace(fields.ServiceTier) == "" {
		return nil, fmt.Errorf("service_tier is required")
	}
	if fields.ContextThreshold == nil || *fields.ContextThreshold <= 0 {
		return nil, fmt.Errorf("context_threshold must be a positive integer")
	}
	if len(fields.Models) == 0 {
		return nil, fmt.Errorf("models must contain at least one model")
	}
	table := &Table{
		Currency: strings.TrimSpace(fields.Currency), Unit: fields.Unit, ServiceTier: strings.TrimSpace(fields.ServiceTier),
		ContextThreshold: *fields.ContextThreshold, models: make(map[string]modelRates),
	}
	for model, fields := range fields.Models {
		if strings.TrimSpace(model) == "" {
			return nil, fmt.Errorf("model name must not be empty")
		}
		short, err := parseRates(fields.Short, model+".short_context")
		if err != nil {
			return nil, err
		}
		long, err := parseRates(fields.Long, model+".long_context")
		if err != nil {
			return nil, err
		}
		table.models[model] = modelRates{short: short, long: long}
	}
	return table, nil
}

func parseRates(fields *rateFields, path string) (rates, error) {
	if fields == nil {
		return rates{}, fmt.Errorf("%s is required", path)
	}
	var result rates
	for _, field := range []struct {
		name   string
		raw    json.RawMessage
		target **big.Rat
	}{
		{name: "input", raw: fields.Input, target: &result.input},
		{name: "cached_input", raw: fields.CachedInput, target: &result.cachedInput},
		{name: "output", raw: fields.Output, target: &result.output},
	} {
		text := strings.TrimSpace(string(field.raw))
		if text == "" || (text[0] != '-' && (text[0] < '0' || text[0] > '9')) {
			return rates{}, fmt.Errorf("%s.%s must be a non-negative number", path, field.name)
		}
		value, ok := new(big.Rat).SetString(text)
		if !ok || value.Sign() < 0 {
			return rates{}, fmt.Errorf("%s.%s must be a non-negative number", path, field.name)
		}
		*field.target = value
	}
	return result, nil
}

// Cost returns nil if the model or required usage is missing or invalid.
// Cached tokens are part of input; reasoning tokens are already part of output.
func (t *Table) Cost(model string, input, cached, output *int64) *big.Rat {
	modelRates, ok := t.models[model]
	if !ok || input == nil || cached == nil || output == nil || *input < 0 || *cached < 0 || *output < 0 || *cached > *input {
		return nil
	}
	rates := modelRates.short
	if *input > t.ContextThreshold {
		rates = modelRates.long
	}
	cost := new(big.Rat).Mul(big.NewRat(*input-*cached, 1), rates.input)
	cost.Add(cost, new(big.Rat).Mul(big.NewRat(*cached, 1), rates.cachedInput))
	cost.Add(cost, new(big.Rat).Mul(big.NewRat(*output, 1), rates.output))
	return cost
}

// Format rounds amounts below one to at most five decimal places and other
// amounts to at most three, keeping at least two. Calculated costs stay exact.
func (t *Table) Format(amount *big.Rat) string {
	if amount == nil {
		return "-"
	}
	precision := 3
	if amount.Cmp(big.NewRat(1, 1)) < 0 {
		precision = 5
	}
	text := amount.FloatString(precision)
	point := strings.IndexByte(text, '.')
	for len(text) > point+3 && strings.HasSuffix(text, "0") {
		text = strings.TrimSuffix(text, "0")
	}
	return t.Currency + " " + text
}
