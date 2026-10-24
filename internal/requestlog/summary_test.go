package requestlog

import (
	"errors"
	"testing"
	"time"
)

func TestSummaryWithUsageKeepsOneSnapshotDuringWrites(t *testing.T) {
	directory := t.TempDir()
	writer, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	start := time.Date(2026, time.October, 24, 12, 0, 0, 0, time.UTC)
	for _, record := range []Request{
		{RequestID: "a", Model: "pending-a", StartedAt: start},
		{RequestID: "b", Model: "pending-b", StartedAt: start.Add(time.Second)},
		{RequestID: "zero", Model: "zero-model", StartedAt: start.Add(2 * time.Second)},
		{RequestID: "outside", Model: "excluded", StartedAt: start.Add(time.Minute)},
	} {
		record.Endpoint = "responses"
		if err := writer.Begin(record); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Finish(Completion{
		RequestID: "zero", FinishedAt: start.Add(3 * time.Second), Outcome: OutcomeSuccess,
		InputTokens: int64Pointer(0), CacheReadTokens: int64Pointer(0), OutputTokens: int64Pointer(0),
	}); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenExisting(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	timeRange := TimeRange{Start: start, End: start.Add(time.Minute)}
	seen := make(map[string]RequestUsage)
	updated := false
	summary, err := reader.SummaryWithUsage(timeRange, func(usage RequestUsage) error {
		if !updated {
			updated = true
			for _, id := range []string{"a", "b"} {
				if err := writer.Finish(Completion{
					RequestID: id, FinishedAt: start.Add(5 * time.Second), Model: "resolved-" + id, Outcome: OutcomeSuccess,
					InputTokens: int64Pointer(10), CacheReadTokens: int64Pointer(5), OutputTokens: int64Pointer(3),
				}); err != nil {
					return err
				}
			}
			if err := writer.Begin(Request{RequestID: "late", Model: "late-model", StartedAt: start.Add(30 * time.Second), Endpoint: "responses"}); err != nil {
				return err
			}
		}
		if _, duplicate := seen[usage.Model]; duplicate {
			t.Fatalf("usage was visited more than once for %q", usage.Model)
		}
		seen[usage.Model] = usage
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := (Aggregate{Requests: 3, Successes: 1, InProgress: 2}); summary.Total != want {
		t.Fatalf("snapshot totals = %#v, want %#v", summary.Total, want)
	}
	if len(seen) != 3 || len(summary.ByModel) != 3 {
		t.Fatalf("snapshot usage = %#v, model counts = %#v", seen, summary.ByModel)
	}
	for _, model := range []string{"pending-a", "pending-b"} {
		usage, found := seen[model]
		if !found || usage.InputTokens != nil || usage.CacheReadTokens != nil || usage.OutputTokens != nil {
			t.Fatalf("missing snapshot usage for %s was changed by the writer: %#v", model, usage)
		}
	}
	zero, found := seen["zero-model"]
	if !found || zero.InputTokens == nil || *zero.InputTokens != 0 || zero.CacheReadTokens == nil || *zero.CacheReadTokens != 0 || zero.OutputTokens == nil || *zero.OutputTokens != 0 {
		t.Fatalf("explicit zero usage was not preserved: %#v", zero)
	}
	later, err := reader.Summary(timeRange)
	if err != nil {
		t.Fatal(err)
	}
	if later.Total.Requests != 4 || later.Total.Successes != 3 || later.Total.InProgress != 1 || later.Total.InputTokens != 20 {
		t.Fatalf("writer changes were not visible after the read transaction: %#v", later.Total)
	}
}

func TestSummaryWithUsageReleasesTransactionOnVisitorError(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Begin(Request{RequestID: "one", StartedAt: time.Now(), Endpoint: "responses"}); err != nil {
		t.Fatal(err)
	}
	wantError := errors.New("stop visiting")
	_, err = store.SummaryWithUsage(TimeRange{All: true}, func(usage RequestUsage) error {
		if usage.Model != "(unknown)" {
			t.Errorf("missing model = %q, want (unknown)", usage.Model)
		}
		return wantError
	})
	if !errors.Is(err, wantError) {
		t.Fatalf("visitor error = %v, want %v", err, wantError)
	}
	// The store has one connection: a leaked read transaction would block here.
	summary, err := store.Summary(TimeRange{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Total.Requests != 1 {
		t.Fatalf("summary after callback error = %#v", summary.Total)
	}
}
