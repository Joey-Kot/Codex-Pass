package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"codex-pass/internal/requestlog"
)

func TestPriceReportsUsePerRequestCostsAndOneTotal(t *testing.T) {
	directory := t.TempDir()
	store, err := requestlog.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	start := time.Date(2026, time.October, 24, 20, 0, 0, 0, time.Local)
	for index, usage := range []struct {
		id, model             string
		input, cached, output int64
	}{
		{id: "short-a", model: "gpt-6-astra", input: 200000, output: 1000},
		{id: "short-b", model: "gpt-6-astra", input: 200000, cached: 100000, output: 2000},
		{id: "long", model: "gpt-6-astra", input: 272001, cached: 20000, output: 1000},
		{id: "threshold", model: "gpt-6-astra", input: 272000, cached: 20000, output: 1000},
		{id: "zero", model: "gpt-6-astra"},
		{id: "tiny", model: "gpt-6-luna", input: 1, cached: 1},
	} {
		started := start.Add(time.Duration(index+1) * time.Second)
		if err := store.Begin(requestlog.Request{RequestID: usage.id, StartedAt: started, Endpoint: "responses", Model: usage.model}); err != nil {
			t.Fatal(err)
		}
		completion := requestlog.Completion{
			RequestID: usage.id, FinishedAt: started.Add(time.Second), Outcome: requestlog.OutcomeSuccess,
			InputTokens: &usage.input, CacheReadTokens: &usage.cached, OutputTokens: &usage.output,
			ReasoningTokens: &usage.output,
		}
		if index == 0 {
			completion.CacheWriteTokens = tokenPointer(100)
			completion.Outcome = requestlog.OutcomeFailure
		}
		if err := store.Finish(completion); err != nil {
			t.Fatal(err)
		}
	}
	for index, started := range []time.Time{start.Add(-time.Millisecond), start.Add(time.Minute)} {
		id := []string{"outside-before", "outside-end"}[index]
		if err := store.Begin(requestlog.Request{RequestID: id, StartedAt: started, Endpoint: "responses", Model: "excluded-model"}); err != nil {
			t.Fatal(err)
		}
	}
	options := commonOptions{
		logDirectory: directory, timeRange: "2026-10-24-20-00,2026-10-24-20-00",
		priceFile: filepath.Join("..", "..", "prices.example.json"),
	}
	wantModelPrices := map[string]string{
		"All models": "USD 10.995", "gpt-6-astra": "USD 10.995", "gpt-6-luna": "USD 0.00", "Sum": "USD 10.995",
	}
	for _, human := range []bool{false, true} {
		options.humanReadable = human
		var output, errorOutput bytes.Buffer
		if code := runSummaryWithOptions(options, &output, &errorOutput); code != 0 {
			t.Fatalf("sum code = %d: %s", code, errorOutput.String())
		}
		assertModelPrices(t, readTableRows(t, output.String(), "Token Usage"), wantModelPrices)
		counts := readTableRows(t, output.String(), "Request Counts")
		if slices.Contains(counts[0], "Price") || counts[len(counts)-1][0] == "Sum" {
			t.Fatalf("sum request counts unexpectedly changed:\n%s", output.String())
		}
		if strings.Contains(output.String(), "partial") || strings.Contains(output.String(), "excluded-model") {
			t.Fatalf("excluded requests affected prices:\n%s", output.String())
		}
		for _, ascending := range []bool{false, true} {
			output.Reset()
			errorOutput.Reset()
			if code := runRequestLogs(options, requestOptions{ascending: ascending}, &output, &errorOutput); code != 0 {
				t.Fatalf("req code = %d: %s", code, errorOutput.String())
			}
			assertModelPrices(t, readTableRows(t, output.String(), "Request Counts"), wantModelPrices)
			rows := readTableRows(t, output.String(), "Request Logs")
			want := []string{"USD 0.00", "USD 0.00", "USD 2.59", "USD 5.155", "USD 1.20", "USD 2.05"}
			if ascending {
				slices.Reverse(want)
			}
			if len(rows) != len(want)+2 || rows[0][len(rows[0])-1] != "Price" {
				t.Fatalf("unexpected request table: %q", rows)
			}
			for index, price := range want {
				row := rows[index+1]
				if row[len(row)-1] != price {
					t.Errorf("request %d price = %q, want %q", index, row[len(row)-1], price)
				}
			}
			assertPriceFooter(t, rows[len(rows)-1], "USD 10.995")
		}
	}

	for _, test := range []struct {
		name          string
		arguments     []string
		wantPrices    []string
		wantTotal     string
		wantModels    map[string]string
		wantCountTail []string
	}{
		{
			name: "number without time", arguments: []string{"req", "--log", directory, "--num", "2", "--price", options.priceFile, "--human"},
			wantPrices: []string{"-", "USD 0.00"}, wantTotal: "USD 0.00 (partial)",
			wantModels:    map[string]string{"All models": "USD 0.00 (partial)", "excluded-model": "- (partial)", "gpt-6-luna": "USD 0.00", "Sum": "USD 0.00 (partial)"},
			wantCountTail: []string{"2", "1", "0", "0", "1"},
		},
		{
			name: "number with time", arguments: []string{"--time", options.timeRange, "req", "--log", directory, "--num=2", "--price", options.priceFile},
			wantPrices: []string{"USD 0.00", "USD 0.00"}, wantTotal: "USD 0.00",
			wantModels:    map[string]string{"All models": "USD 0.00", "gpt-6-astra": "USD 0.00", "gpt-6-luna": "USD 0.00", "Sum": "USD 0.00"},
			wantCountTail: []string{"2", "2", "0", "0", "0"},
		},
		{
			name: "number with ascending order", arguments: []string{"req", "--log", directory, "--time", options.timeRange, "--num", "2", "--asc", "--price", options.priceFile},
			wantPrices: []string{"USD 2.05", "USD 1.20"}, wantTotal: "USD 3.25",
			wantModels:    map[string]string{"All models": "USD 3.25", "gpt-6-astra": "USD 3.25", "Sum": "USD 3.25"},
			wantCountTail: []string{"2", "1", "1", "0", "0"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output, errorOutput bytes.Buffer
			parsed, err := parseCommandLine(test.arguments, &errorOutput)
			if err != nil {
				t.Fatal(err)
			}
			if code := runRequestLogs(parsed.common, parsed.request, &output, &errorOutput); code != 0 {
				t.Fatalf("req code = %d: %s", code, errorOutput.String())
			}
			counts := readTableRows(t, output.String(), "Request Counts")
			assertModelPrices(t, counts, test.wantModels)
			if !slices.Equal(counts[1][1:6], test.wantCountTail) {
				t.Fatalf("limited request counts = %q, want %q", counts[1][1:6], test.wantCountTail)
			}
			rows := readTableRows(t, output.String(), "Request Logs")
			if len(rows) != len(test.wantPrices)+2 {
				t.Fatalf("request table exceeded limit: %q", rows)
			}
			for index, want := range test.wantPrices {
				row := rows[index+1]
				if row[len(row)-1] != want {
					t.Fatalf("limited request %d = %q, want price %q", index, row, want)
				}
			}
			assertPriceFooter(t, rows[len(rows)-1], test.wantTotal)
			if !strings.Contains(output.String(), "Limit: 2") {
				t.Fatalf("request limit missing from report title:\n%s", output.String())
			}
		})
	}

	for index, model := range []string{"unknown-model", "gpt-6-astra", "gpt-6-luna"} {
		id := []string{"unknown-price", "missing-cache-usage", "unfinished"}[index]
		started := start.Add(time.Duration(index+10) * time.Second)
		if err := store.Begin(requestlog.Request{RequestID: id, StartedAt: started, Endpoint: "responses", Model: model}); err != nil {
			t.Fatal(err)
		}
		if index == 2 {
			continue
		}
		completion := requestlog.Completion{RequestID: id, FinishedAt: started.Add(time.Second), Outcome: requestlog.OutcomeSuccess, InputTokens: tokenPointer(1), OutputTokens: tokenPointer(1)}
		if index == 0 {
			completion.CacheReadTokens = tokenPointer(0)
		}
		if err := store.Finish(completion); err != nil {
			t.Fatal(err)
		}
	}
	wantPartial := map[string]string{
		"All models": "USD 10.995 (partial)", "gpt-6-astra": "USD 10.995 (partial)", "gpt-6-luna": "USD 0.00 (partial)",
		"unknown-model": "- (partial)", "Sum": "USD 10.995 (partial)",
	}
	for _, command := range []string{"sum", "req"} {
		var output, errorOutput bytes.Buffer
		var code int
		if command == "sum" {
			code = runSummaryWithOptions(options, &output, &errorOutput)
			assertModelPrices(t, readTableRows(t, output.String(), "Token Usage"), wantPartial)
		} else {
			code = runRequestLogs(options, requestOptions{}, &output, &errorOutput)
			assertModelPrices(t, readTableRows(t, output.String(), "Request Counts"), wantPartial)
			rows := readTableRows(t, output.String(), "Request Logs")
			for _, row := range rows[1:4] {
				if row[len(row)-1] != "-" {
					t.Errorf("unpriced request = %q", row)
				}
			}
			assertPriceFooter(t, rows[len(rows)-1], "USD 10.995 (partial)")
		}
		if code != 0 || !strings.Contains(output.String(), "exclude 3 request(s)") {
			t.Fatalf("%s code = %d, output = %s, error = %s", command, code, output.String(), errorOutput.String())
		}
	}
	options.timeRange = "2026-10-25,2026-10-25"
	var output, errorOutput bytes.Buffer
	if code := runRequestLogs(options, requestOptions{}, &output, &errorOutput); code != 0 {
		t.Fatalf("empty req code = %d: %s", code, errorOutput.String())
	}
	assertModelPrices(t, readTableRows(t, output.String(), "Request Counts"), map[string]string{"All models": "USD 0.00", "Sum": "USD 0.00"})
	rows := readTableRows(t, output.String(), "Request Logs")
	if len(rows) != 2 {
		t.Fatalf("empty priced logs = %q", rows)
	}
	assertPriceFooter(t, rows[1], "USD 0.00")
}

func TestPriceTotalsWithNoKnownCosts(t *testing.T) {
	table, err := loadPriceTable(filepath.Join("..", "..", "prices.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	prices := calculatePrices(table, []requestlog.LogEntry{{RequestID: "missing", Model: "gpt-6-astra"}})
	if got := prices.total.format(table); got != "- (partial)" {
		t.Fatalf("unknown total = %q", got)
	}
	if got := prices.table.Format(prices.byRequest["missing"]); got != "-" {
		t.Fatalf("unknown request price = %q", got)
	}
}

func TestQueriesRejectUnreadableOrInvalidPriceFiles(t *testing.T) {
	directory := t.TempDir()
	invalid := filepath.Join(directory, "invalid.json")
	if err := os.WriteFile(invalid, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{invalid, filepath.Join(directory, "missing.json")} {
		options := commonOptions{logDirectory: directory, priceFile: path}
		for _, command := range []string{"sum", "req"} {
			var output, errorOutput bytes.Buffer
			var code int
			if command == "sum" {
				code = runSummaryWithOptions(options, &output, &errorOutput)
			} else {
				code = runRequestLogs(options, requestOptions{}, &output, &errorOutput)
			}
			if code != 2 || output.Len() != 0 || !strings.Contains(errorOutput.String(), "invalid --price") {
				t.Fatalf("%s code = %d, output = %s, error = %s", command, code, output.String(), errorOutput.String())
			}
		}
	}
}

func assertModelPrices(t *testing.T, rows [][]string, want map[string]string) {
	t.Helper()
	if len(rows) != len(want)+1 || rows[0][len(rows[0])-1] != "Price" {
		t.Fatalf("unexpected model price rows: %q", rows)
	}
	for _, row := range rows[1:] {
		if price, exists := want[row[0]]; !exists || row[len(row)-1] != price {
			t.Errorf("model price row = %q, expected prices %v", row, want)
		}
	}
	assertPriceFooter(t, rows[len(rows)-1], want["Sum"])
}

func assertPriceFooter(t *testing.T, row []string, want string) {
	t.Helper()
	if row[0] != "Sum" || row[len(row)-1] != want {
		t.Fatalf("price footer = %q, want Sum with %s", row, want)
	}
	for _, cell := range row[1 : len(row)-1] {
		if cell != "" {
			t.Fatalf("price footer contains a non-price total: %q", row)
		}
	}
}
