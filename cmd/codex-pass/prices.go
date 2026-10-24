package main

import (
	"fmt"
	"io"
	"math/big"

	"codex-pass/internal/pricing"
	"codex-pass/internal/requestlog"
)

type priceTotal struct {
	amount  big.Rat
	priced  int
	missing int
}

func (total *priceTotal) add(cost *big.Rat) {
	if cost == nil {
		total.missing++
		return
	}
	total.amount.Add(&total.amount, cost)
	total.priced++
}

func (total *priceTotal) format(table *pricing.Table) string {
	text := table.Format(&total.amount)
	if total.missing != 0 {
		if total.priced == 0 {
			text = "-"
		}
		text += " (partial)"
	}
	return text
}

type reportPrices struct {
	table     *pricing.Table
	total     priceTotal
	byModel   map[string]*priceTotal
	byRequest map[string]*big.Rat
}

func newReportPrices(table *pricing.Table) *reportPrices {
	return &reportPrices{table: table, byModel: make(map[string]*priceTotal)}
}

func (prices *reportPrices) addUsage(usage requestlog.RequestUsage) *big.Rat {
	cost := prices.table.Cost(usage.Model, usage.InputTokens, usage.CacheReadTokens, usage.OutputTokens)
	prices.total.add(cost)
	if prices.byModel[usage.Model] == nil {
		prices.byModel[usage.Model] = &priceTotal{}
	}
	prices.byModel[usage.Model].add(cost)
	return cost
}

func loadPriceTable(path string) (*pricing.Table, error) {
	if path == "" {
		return nil, nil
	}
	table, err := pricing.Load(path)
	if err != nil {
		return nil, fmt.Errorf("invalid --price: %w", err)
	}
	return table, nil
}

func calculatePrices(table *pricing.Table, entries []requestlog.LogEntry) *reportPrices {
	if table == nil {
		return nil
	}
	prices := newReportPrices(table)
	prices.byRequest = make(map[string]*big.Rat, len(entries))
	for _, entry := range entries {
		prices.byRequest[entry.RequestID] = prices.addUsage(requestlog.RequestUsage{
			Model: entry.Model, InputTokens: entry.InputTokens, CacheReadTokens: entry.CacheReadTokens, OutputTokens: entry.OutputTokens,
		})
	}
	return prices
}

func appendModelPrices(headers []string, rows [][]string, prices *reportPrices) ([]string, [][]string) {
	if prices == nil {
		return headers, rows
	}
	headers = append(headers, "Price")
	for index, row := range rows {
		total := &prices.total
		if index != 0 {
			total = prices.byModel[row[0]]
		}
		rows[index] = append(row, total.format(prices.table))
	}
	rows = append(rows, priceSumRow(len(headers), prices))
	return headers, rows
}

func priceSumRow(columns int, prices *reportPrices) []string {
	row := make([]string, columns)
	row[0], row[columns-1] = "Sum", prices.total.format(prices.table)
	return row
}

func printPriceContext(output io.Writer, prices *reportPrices) {
	if prices != nil {
		fmt.Fprintf(output, "Pricing: %s, %s, service tier: %s\n\n", prices.table.Currency, prices.table.Unit, prices.table.ServiceTier)
	}
}

func printPriceNote(output io.Writer, prices *reportPrices) {
	if prices != nil && prices.total.missing != 0 {
		fmt.Fprintf(output, "\nPartial prices exclude %d request(s) with missing model rates or missing/invalid usage.\n", prices.total.missing)
	}
}
