package main

import (
	"bytes"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"codex-pass/internal/requestlog"
)

func TestRunRequestLogs(t *testing.T) {
	originalLocal := time.Local
	time.Local = time.FixedZone("test", 8*60*60)
	t.Cleanup(func() { time.Local = originalLocal })
	directory := t.TempDir()
	store, err := requestlog.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	start := time.Date(2026, time.October, 24, 20, 51, 37, 123000000, time.Local)
	for _, record := range []requestlog.Request{
		{RequestID: "outside", StartedAt: start.Add(-24 * time.Hour), Model: "excluded-model"},
		{RequestID: "first", StartedAt: start, Model: "gpt-6-astra"},
		{RequestID: "second", StartedAt: start.Add(2 * time.Minute), Model: "gpt-6-sol"},
		{RequestID: "unfinished", StartedAt: start.Add(4 * time.Minute)},
	} {
		record.Endpoint = "responses"
		if err := store.Begin(record); err != nil {
			t.Fatal(err)
		}
	}
	for _, completion := range []requestlog.Completion{
		{
			RequestID: "first", FinishedAt: start.Add(10 * time.Second), Outcome: requestlog.OutcomeSuccess,
			InputTokens: tokenPointer(56510), CacheReadTokens: tokenPointer(49520), OutputTokens: tokenPointer(500), ReasoningTokens: tokenPointer(200),
		},
		{
			RequestID: "second", FinishedAt: start.Add(210 * time.Second), Outcome: requestlog.OutcomeFailure,
			InputTokens: tokenPointer(2410000), CacheReadTokens: tokenPointer(0), OutputTokens: tokenPointer(1270), ReasoningTokens: tokenPointer(1000),
		},
	} {
		if err := store.Finish(completion); err != nil {
			t.Fatal(err)
		}
	}

	for _, test := range []struct {
		name      string
		human     bool
		ascending bool
	}{
		{name: "default"},
		{name: "human", human: true},
		{name: "ascending", ascending: true},
		{name: "human ascending", human: true, ascending: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output, errors bytes.Buffer
			options := commonOptions{logDirectory: directory, timeRange: "2026-10-24,2026-10-24", humanReadable: test.human}
			if code := runRequestLogs(options, requestOptions{ascending: test.ascending}, &output, &errors); code != 0 {
				t.Fatalf("runRequestLogs code = %d: %s", code, errors.String())
			}
			wantCounts := [][]string{
				{"Model", "Requests", "Success", "Failed", "Canceled", "Unfinished"},
				{"All models", "3", "1", "1", "0", "1"},
				{"(unknown)", "1", "0", "0", "0", "1"},
				{"gpt-6-astra", "1", "1", "0", "0", "0"},
				{"gpt-6-sol", "1", "0", "1", "0", "0"},
			}
			if got := readTableRows(t, output.String(), "Request Counts"); !reflect.DeepEqual(got, wantCounts) {
				t.Fatalf("request counts = %q, want %q", got, wantCounts)
			}
			wantLogs := [][]string{
				{"Request time", "Time taken", "Model", "Input", "Output", "Cache hit"},
				{"2026-10-24 20:55:37", "-", "(unknown)", "- ( - )", "- ( - )", "-"},
				{"2026-10-24 20:53:37", "1.5 minutes", "gpt-6-sol", "2,410,000 ( 0 )", "1,270 ( 1,000 )", "0.00%"},
				{"2026-10-24 20:51:37", "10 seconds", "gpt-6-astra", "56,510 ( 49,520 )", "500 ( 200 )", "87.63%"},
			}
			if test.human {
				wantLogs[2][3], wantLogs[2][4] = "2.41M ( 0 )", "1.27K ( 1K )"
				wantLogs[3][3] = "56.51K ( 49.52K )"
			}
			if test.ascending {
				slices.Reverse(wantLogs[1:])
			}
			if got := readTableRows(t, output.String(), "Request Logs"); !reflect.DeepEqual(got, wantLogs) {
				t.Fatalf("request logs = %q, want %q\n%s", got, wantLogs, output.String())
			}
			if strings.Contains(output.String(), "Token Usage") || strings.Contains(output.String(), "excluded-model") {
				t.Fatalf("unexpected table or excluded request in output:\n%s", output.String())
			}
		})
	}
	var output, errors bytes.Buffer
	if code := runRequestLogs(commonOptions{logDirectory: directory, timeRange: "2026-10-25,2026-10-25"}, requestOptions{}, &output, &errors); code != 0 {
		t.Fatalf("empty range code = %d: %s", code, errors.String())
	}
	counts := readTableRows(t, output.String(), "Request Counts")
	if len(counts) != 2 || !reflect.DeepEqual(counts[1], []string{"All models", "0", "0", "0", "0", "0"}) || len(readTableRows(t, output.String(), "Request Logs")) != 1 {
		t.Fatalf("empty range output:\n%s", output.String())
	}
}

func TestRequestLogsReportsQueryErrors(t *testing.T) {
	for _, test := range []struct {
		timeRange string
		wantError string
	}{
		{wantError: "does not exist"},
		{timeRange: "invalid", wantError: "invalid --time"},
	} {
		var output, errors bytes.Buffer
		code := runRequestLogs(commonOptions{logDirectory: t.TempDir(), timeRange: test.timeRange}, requestOptions{}, &output, &errors)
		if code != 2 || !strings.Contains(errors.String(), test.wantError) || output.Len() != 0 {
			t.Fatalf("code = %d, error = %q, output = %q", code, errors.String(), output.String())
		}
	}
}

func TestFormatRequestDuration(t *testing.T) {
	start := time.UnixMilli(1000)
	for _, test := range []struct {
		duration time.Duration
		want     string
	}{
		{duration: 0, want: "0 seconds"},
		{duration: time.Millisecond, want: "0 seconds"},
		{duration: time.Second, want: "1 second"},
		{duration: 1234 * time.Millisecond, want: "1.23 seconds"},
		{duration: 1450 * time.Millisecond, want: "1.45 seconds"},
		{duration: 1456 * time.Millisecond, want: "1.46 seconds"},
		{duration: 59999 * time.Millisecond, want: "60 seconds"},
		{duration: time.Minute, want: "1 minute"},
		{duration: 87 * time.Second, want: "1.45 minutes"},
		{duration: 90 * time.Second, want: "1.5 minutes"},
		{duration: time.Hour, want: "60 minutes"},
		{duration: -time.Millisecond, want: "-"},
	} {
		end := start.Add(test.duration)
		if got := formatRequestDuration(start, &end); got != test.want {
			t.Errorf("duration %s = %q, want %q", test.duration, got, test.want)
		}
	}
	if got := formatRequestDuration(start, nil); got != "-" {
		t.Errorf("missing finish time = %q, want -", got)
	}
}

func TestMissingAndZeroRequestUsage(t *testing.T) {
	for _, test := range []struct {
		input, cached *int64
		wantTokens    string
		wantHit       string
	}{
		{wantTokens: "- ( - )", wantHit: "-"},
		{input: tokenPointer(1000), wantTokens: "1K ( - )", wantHit: "-"},
		{cached: tokenPointer(10), wantTokens: "- ( 10 )", wantHit: "-"},
		{input: tokenPointer(0), cached: tokenPointer(0), wantTokens: "0 ( 0 )", wantHit: "-"},
		{input: tokenPointer(1000), cached: tokenPointer(0), wantTokens: "1K ( 0 )", wantHit: "0.00%"},
		{input: tokenPointer(1000), cached: tokenPointer(1000), wantTokens: "1K ( 1K )", wantHit: "100.00%"},
	} {
		if got := formatRequestTokens(test.input, test.cached, formatHumanNumber); got != test.wantTokens {
			t.Errorf("tokens = %q, want %q", got, test.wantTokens)
		}
		if got := formatCacheHit(test.input, test.cached); got != test.wantHit {
			t.Errorf("cache hit = %q, want %q", got, test.wantHit)
		}
	}
}

func tokenPointer(value int64) *int64 { return &value }

func readTableRows(t *testing.T, output, heading string) [][]string {
	t.Helper()
	_, table, found := strings.Cut(output, "\n"+heading+"\n")
	if !found {
		t.Fatalf("missing table %q:\n%s", heading, output)
	}
	table, _, _ = strings.Cut(table, "\n\n")
	var rows [][]string
	for _, line := range strings.Split(table, "\n") {
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for index := range cells {
			cells[index] = strings.TrimSpace(cells[index])
		}
		rows = append(rows, cells)
	}
	return rows
}
