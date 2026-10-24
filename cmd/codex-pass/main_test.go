package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"codex-pass/internal/requestlog"
)

func TestResolveListenAddress(t *testing.T) {
	tests := []struct {
		name string
		cli  string
		env  string
		want string
	}{
		{name: "default", want: "127.0.0.1:8317"},
		{name: "environment", env: "127.0.0.1:9000", want: "127.0.0.1:9000"},
		{name: "cli overrides environment", cli: "0.0.0.0:9000", env: "127.0.0.1:9000", want: "0.0.0.0:9000"},
		{name: "whitespace is unset", cli: "  ", env: "127.0.0.1:9000", want: "127.0.0.1:9000"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := resolveListenAddress(test.cli, test.env); got != test.want {
				t.Fatalf("resolveListenAddress(%q, %q) = %q, want %q", test.cli, test.env, got, test.want)
			}
		})
	}
}

func TestRunSummaryPrintsGlobalAndModelTotals(t *testing.T) {
	directory := t.TempDir()
	store, err := requestlog.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, time.June, 9, 10, 0, 0, 0, time.UTC)
	if err := store.Begin(requestlog.Request{RequestID: "one", StartedAt: started, Endpoint: "responses", Model: "gpt-6-luna", ReasoningEffort: "high"}); err != nil {
		t.Fatal(err)
	}
	input := int64(12)
	cacheWrite := int64(3)
	cacheRead := int64(4)
	output := int64(5)
	reasoning := int64(6)
	if err := store.Finish(requestlog.Completion{
		RequestID:        "one",
		FinishedAt:       started.Add(time.Second),
		InputTokens:      &input,
		CacheWriteTokens: &cacheWrite,
		CacheReadTokens:  &cacheRead,
		OutputTokens:     &output,
		ReasoningTokens:  &reasoning,
		Outcome:          requestlog.OutcomeSuccess,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	var outputBuffer, errorBuffer bytes.Buffer
	if code := runSummary(directory, "", &outputBuffer, &errorBuffer); code != 0 {
		t.Fatalf("runSummary code = %d, stderr = %s", code, errorBuffer.String())
	}
	want := `Usage Summary (Range: all)

Request Counts
+------------+----------+---------+--------+----------+------------+
| Model      | Requests | Success | Failed | Canceled | Unfinished |
+------------+----------+---------+--------+----------+------------+
| All models |        1 |       1 |      0 |        0 |          0 |
| gpt-6-luna |        1 |       1 |      0 |        0 |          0 |
+------------+----------+---------+--------+----------+------------+

Token Usage
+------------+-------+-------+-------------+------------+--------+-----------+
| Model      | Total | Input | Cache Write | Cache Read | Output | Reasoning |
+------------+-------+-------+-------------+------------+--------+-----------+
| All models |    17 |    12 |           3 |          4 |      5 |         6 |
| gpt-6-luna |    17 |    12 |           3 |          4 |      5 |         6 |
+------------+-------+-------+-------------+------------+--------+-----------+
`
	if got := outputBuffer.String(); got != want {
		t.Fatalf("summary output =\n%s\nwant:\n%s", got, want)
	}
}

func TestFormatNumber(t *testing.T) {
	tests := map[int64]string{
		-1234567: "-1,234,567",
		0:        "0",
		999:      "999",
		1000:     "1,000",
		6589:     "6,589",
	}
	for input, want := range tests {
		if got := formatNumber(input); got != want {
			t.Errorf("formatNumber(%d) = %q, want %q", input, got, want)
		}
	}
}

func TestFormatHumanNumber(t *testing.T) {
	tests := map[int64]string{
		-1234567:   "-1.23M",
		0:          "0",
		999:        "999",
		1000:       "1K",
		1270:       "1.27K",
		2410000:    "2.41M",
		2340000000: "2.34B",
		999999:     "1M",
	}
	for input, want := range tests {
		if got := formatHumanNumber(input); got != want {
			t.Errorf("formatHumanNumber(%d) = %q, want %q", input, got, want)
		}
	}
}

func TestPrintSummaryWithHumanReadableNumbers(t *testing.T) {
	summary := requestlog.Summary{
		Total: requestlog.Aggregate{
			Requests:         1270,
			Successes:        1270,
			InputTokens:      2410000,
			CacheWriteTokens: 1270,
			CacheReadTokens:  2340000000,
			OutputTokens:     1000,
			ReasoningTokens:  999,
		},
	}

	var output bytes.Buffer
	printSummaryWithOptions(&output, "all", summary, true, nil)
	got := output.String()
	for _, value := range []string{"1.27K", "2.41M", "2.34B", "1K"} {
		if !strings.Contains(got, value) {
			t.Errorf("human-readable summary does not contain %q:\n%s", value, got)
		}
	}
	if strings.Contains(got, "2,410,000") {
		t.Errorf("human-readable summary contains comma-formatted number:\n%s", got)
	}
}

func TestRunSummaryReportsMissingDatabase(t *testing.T) {
	var outputBuffer, errorBuffer bytes.Buffer
	if code := runSummary(t.TempDir(), "", &outputBuffer, &errorBuffer); code != 2 {
		t.Fatalf("runSummary code = %d, want 2", code)
	}
	if !strings.Contains(errorBuffer.String(), "does not exist") {
		t.Fatalf("missing database error = %s", errorBuffer.String())
	}
}

func TestQueryCommandsDoNotRequireKeyOrAuthFile(t *testing.T) {
	directory := t.TempDir()
	store, err := requestlog.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, arguments := range [][]string{
		{"sum", "--log", directory},
		{"--log", directory, "sum"},
		{"--time", "1h", "sum", "--log", directory, "--human"},
		{"req", "--log", directory},
		{"--log", directory, "req", "--asc"},
		{"--time", "2m", "req", "--log", directory, "--human", "--desc"},
		{"req", "--log", directory, "--num", "10"},
	} {
		if code := run(arguments); code != 0 {
			t.Fatalf("run(%q) code = %d, want 0", arguments, code)
		}
	}
}
