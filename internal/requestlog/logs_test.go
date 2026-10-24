package requestlog

import (
	"reflect"
	"testing"
	"time"
)

func TestLogsFiltersOrdersAndPreservesUsage(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	start := time.Date(2026, time.October, 24, 12, 51, 37, 123000000, time.UTC)
	end := start.Add(62 * time.Second)
	for _, record := range []Request{
		{RequestID: "before", StartedAt: start.Add(-time.Millisecond), Model: "excluded"},
		{RequestID: "old", StartedAt: start, Model: "requested-model"},
		{RequestID: "tie-a", StartedAt: start.Add(30 * time.Second), Model: "model-b"},
		{RequestID: "tie-z", StartedAt: start.Add(30 * time.Second), Model: "model-b"},
		{RequestID: "new", StartedAt: start.Add(61 * time.Second)},
		{RequestID: "end", StartedAt: end, Model: "excluded"},
	} {
		record.Endpoint = "responses"
		if err := store.Begin(record); err != nil {
			t.Fatal(err)
		}
	}
	for _, completion := range []Completion{
		{
			RequestID: "old", FinishedAt: start.Add(10123 * time.Millisecond), Model: "actual-model", Outcome: OutcomeSuccess,
			InputTokens: int64Pointer(56510), CacheReadTokens: int64Pointer(49520), OutputTokens: int64Pointer(500), ReasoningTokens: int64Pointer(200),
		},
		{RequestID: "tie-a", FinishedAt: end, Outcome: OutcomeFailure, InputTokens: int64Pointer(0), CacheReadTokens: int64Pointer(0)},
		{RequestID: "tie-z", FinishedAt: end, Outcome: OutcomeCanceled},
	} {
		if err := store.Finish(completion); err != nil {
			t.Fatal(err)
		}
	}

	for _, test := range []struct {
		name      string
		ascending bool
		wantIDs   []string
	}{
		{name: "newest first", wantIDs: []string{"new", "tie-z", "tie-a", "old"}},
		{name: "oldest first", ascending: true, wantIDs: []string{"old", "tie-a", "tie-z", "new"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			logs, err := store.Logs(TimeRange{Start: start, End: end}, test.ascending, 0)
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			byID := make(map[string]LogEntry)
			for _, entry := range logs.Entries {
				ids = append(ids, entry.RequestID)
				byID[entry.RequestID] = entry
			}
			if !reflect.DeepEqual(ids, test.wantIDs) {
				t.Fatalf("request IDs = %q, want %q", ids, test.wantIDs)
			}
			wantTotal := Aggregate{Requests: 4, Successes: 1, Failures: 1, Canceled: 1, InProgress: 1, InputTokens: 56510, CacheReadTokens: 49520, OutputTokens: 500, ReasoningTokens: 200}
			if logs.Summary.Total != wantTotal {
				t.Fatalf("request counts and usage = %#v, want %#v", logs.Summary.Total, wantTotal)
			}
			if len(logs.Summary.ByModel) != 3 || logs.Summary.ByModel[0].Model != "(unknown)" || logs.Summary.ByModel[1].Model != "actual-model" || logs.Summary.ByModel[2].Requests != 2 {
				t.Fatalf("model counts = %#v", logs.Summary.ByModel)
			}
			old := byID["old"]
			if !old.StartedAt.Equal(start) || old.FinishedAt == nil || old.FinishedAt.Sub(old.StartedAt) != 10123*time.Millisecond || old.Model != "actual-model" {
				t.Fatalf("stored timing and model = %#v", old)
			}
			for index, value := range []*int64{old.InputTokens, old.CacheReadTokens, old.OutputTokens, old.ReasoningTokens} {
				want := []int64{56510, 49520, 500, 200}[index]
				if value == nil || *value != want {
					t.Fatalf("token field %d = %v, want %d", index, value, want)
				}
			}
			zero := byID["tie-a"]
			if zero.InputTokens == nil || *zero.InputTokens != 0 || zero.CacheReadTokens == nil || *zero.CacheReadTokens != 0 || zero.OutputTokens != nil {
				t.Fatalf("zero and missing fields were not preserved: %#v", zero)
			}
			unfinished := byID["new"]
			if unfinished.FinishedAt != nil || unfinished.Model != "(unknown)" || unfinished.InputTokens != nil || unfinished.CacheReadTokens != nil || unfinished.OutputTokens != nil || unfinished.ReasoningTokens != nil {
				t.Fatalf("unfinished request = %#v", unfinished)
			}
		})
	}
	for _, test := range []struct {
		name      string
		timeRange TimeRange
		ascending bool
		limit     int
		wantIDs   []string
		wantTotal Aggregate
	}{
		{
			name: "latest without time range", timeRange: TimeRange{All: true}, limit: 2,
			wantIDs: []string{"end", "new"}, wantTotal: Aggregate{Requests: 2, InProgress: 2},
		},
		{
			name: "time range before limit with tied timestamps", timeRange: TimeRange{Start: start, End: end}, limit: 2,
			wantIDs: []string{"new", "tie-z"}, wantTotal: Aggregate{Requests: 2, Canceled: 1, InProgress: 1},
		},
		{
			name: "ascending before limit", timeRange: TimeRange{Start: start, End: end}, ascending: true, limit: 2,
			wantIDs: []string{"old", "tie-a"},
			wantTotal: Aggregate{Requests: 2, Successes: 1, Failures: 1, InputTokens: 56510,
				CacheReadTokens: 49520, OutputTokens: 500, ReasoningTokens: 200},
		},
		{
			name: "limit exceeds matching requests", timeRange: TimeRange{Start: start, End: end}, limit: 10,
			wantIDs: []string{"new", "tie-z", "tie-a", "old"},
			wantTotal: Aggregate{Requests: 4, Successes: 1, Failures: 1, Canceled: 1, InProgress: 1,
				InputTokens: 56510, CacheReadTokens: 49520, OutputTokens: 500, ReasoningTokens: 200},
		},
		{
			name: "empty range with limit", timeRange: TimeRange{Start: end.Add(time.Millisecond), End: end.Add(time.Second)}, limit: 2,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			logs, err := store.Logs(test.timeRange, test.ascending, test.limit)
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, entry := range logs.Entries {
				ids = append(ids, entry.RequestID)
			}
			if !reflect.DeepEqual(ids, test.wantIDs) || logs.Summary.Total != test.wantTotal {
				t.Fatalf("IDs = %q, totals = %#v; want IDs = %q, totals = %#v", ids, logs.Summary.Total, test.wantIDs, test.wantTotal)
			}
			var modelRequests int64
			for _, model := range logs.Summary.ByModel {
				modelRequests += model.Requests
			}
			if modelRequests != int64(len(ids)) {
				t.Fatalf("model counts include requests beyond the limit: %#v", logs.Summary.ByModel)
			}
		})
	}
	if _, err := store.Logs(TimeRange{All: true}, false, -1); err == nil {
		t.Fatal("negative limit unexpectedly succeeded")
	}
	all, err := store.Logs(TimeRange{All: true}, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Entries) != 6 || all.Summary.Total.Requests != 6 {
		t.Fatalf("unfiltered request count = %d, entries = %d", all.Summary.Total.Requests, len(all.Entries))
	}
	empty, err := store.Logs(TimeRange{Start: end.Add(time.Millisecond), End: end.Add(time.Second)}, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Entries) != 0 || empty.Summary.Total != (Aggregate{}) || len(empty.Summary.ByModel) != 0 {
		t.Fatalf("empty range = %#v", empty)
	}
	if _, err := store.Logs(TimeRange{Start: end, End: start}, false, 0); err == nil {
		t.Fatal("reversed time range unexpectedly succeeded")
	}
}
