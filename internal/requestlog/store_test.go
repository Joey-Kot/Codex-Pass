package requestlog

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStorePersistsAndSummarizesRequestUsage(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "usage")
	store, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	info, err := os.Stat(filepath.Join(directory, DatabaseFilename))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("database permissions = %o, want 600", info.Mode().Perm())
	}

	firstStart := time.Date(2026, time.June, 9, 10, 0, 0, 0, time.UTC)
	if err := store.Begin(Request{
		RequestID:       "first",
		StartedAt:       firstStart,
		Endpoint:        "responses",
		Model:           "requested-model",
		ReasoningEffort: "high",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Finish(Completion{
		RequestID:        "first",
		FinishedAt:       firstStart.Add(time.Second),
		Model:            "gpt-6-luna",
		InputTokens:      int64Pointer(100),
		CacheWriteTokens: int64Pointer(30),
		CacheReadTokens:  int64Pointer(40),
		OutputTokens:     int64Pointer(20),
		ReasoningTokens:  int64Pointer(10),
		Outcome:          OutcomeSuccess,
	}); err != nil {
		t.Fatal(err)
	}
	var storedStart, storedFinish, storedInput, storedCacheWrite, storedCacheRead, storedOutput, storedReasoning int64
	var storedModel, storedEffort string
	if err := store.db.QueryRow(`
		SELECT started_at_ms, finished_at_ms, model, input_tokens, cache_write_tokens, cache_read_tokens, output_tokens, reasoning_tokens, reasoning_effort
		FROM request_logs WHERE request_id = 'first'
	`).Scan(&storedStart, &storedFinish, &storedModel, &storedInput, &storedCacheWrite, &storedCacheRead, &storedOutput, &storedReasoning, &storedEffort); err != nil {
		t.Fatal(err)
	}
	if storedStart != firstStart.UnixMilli() || storedFinish != firstStart.Add(time.Second).UnixMilli() || storedModel != "gpt-6-luna" || storedEffort != "high" || storedInput != 100 || storedCacheWrite != 30 || storedCacheRead != 40 || storedOutput != 20 || storedReasoning != 10 {
		t.Fatalf("stored request values = start:%d finish:%d model:%q effort:%q tokens:%d/%d/%d/%d/%d", storedStart, storedFinish, storedModel, storedEffort, storedInput, storedCacheWrite, storedCacheRead, storedOutput, storedReasoning)
	}

	secondStart := firstStart.Add(24 * time.Hour)
	if err := store.Begin(Request{RequestID: "second", StartedAt: secondStart, Endpoint: "chat_completions", Model: "gpt-6-sol"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Finish(Completion{
		RequestID:   "second",
		FinishedAt:  secondStart.Add(time.Second),
		InputTokens: int64Pointer(7),
		Outcome:     OutcomeFailure,
		ErrorKind:   "upstream_http_status",
	}); err != nil {
		t.Fatal(err)
	}

	thirdStart := secondStart.Add(24 * time.Hour)
	if err := store.Begin(Request{RequestID: "third", StartedAt: thirdStart, Endpoint: "responses", Model: "gpt-6-luna"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Finish(Completion{
		RequestID:  "third",
		FinishedAt: thirdStart.Add(time.Second),
		Outcome:    OutcomeCanceled,
		ErrorKind:  "client_canceled",
	}); err != nil {
		t.Fatal(err)
	}

	fourthStart := thirdStart.Add(24 * time.Hour)
	if err := store.Begin(Request{RequestID: "fourth", StartedAt: fourthStart, Endpoint: "responses"}); err != nil {
		t.Fatal(err)
	}

	summary, err := store.Summary(TimeRange{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := summary.Total.Requests, int64(4); got != want {
		t.Fatalf("total requests = %d, want %d", got, want)
	}
	if got, want := summary.Total.Successes, int64(1); got != want {
		t.Fatalf("successes = %d, want %d", got, want)
	}
	if got, want := summary.Total.Failures, int64(1); got != want {
		t.Fatalf("failures = %d, want %d", got, want)
	}
	if got, want := summary.Total.Canceled, int64(1); got != want {
		t.Fatalf("canceled = %d, want %d", got, want)
	}
	if got, want := summary.Total.InProgress, int64(1); got != want {
		t.Fatalf("in progress = %d, want %d", got, want)
	}
	if got, want := summary.Total.InputTokens, int64(107); got != want {
		t.Fatalf("input tokens = %d, want %d", got, want)
	}
	if got, want := summary.Total.CacheWriteTokens, int64(30); got != want {
		t.Fatalf("cache write tokens = %d, want %d", got, want)
	}
	if got, want := summary.Total.CacheReadTokens, int64(40); got != want {
		t.Fatalf("cache read tokens = %d, want %d", got, want)
	}
	if got, want := summary.Total.OutputTokens, int64(20); got != want {
		t.Fatalf("output tokens = %d, want %d", got, want)
	}
	if got, want := summary.Total.ReasoningTokens, int64(10); got != want {
		t.Fatalf("reasoning tokens = %d, want %d", got, want)
	}
	if len(summary.ByModel) != 3 {
		t.Fatalf("model totals = %#v, want 3 rows", summary.ByModel)
	}
	byModel := make(map[string]Aggregate, len(summary.ByModel))
	for _, row := range summary.ByModel {
		byModel[row.Model] = row.Aggregate
	}
	if got := byModel["gpt-6-luna"]; got.Successes != 1 || got.Canceled != 1 || got.InputTokens != 100 {
		t.Fatalf("luna totals = %#v", got)
	}
	if got := byModel["gpt-6-sol"]; got.Failures != 1 || got.InputTokens != 7 {
		t.Fatalf("sol totals = %#v", got)
	}
	if got := byModel["(unknown)"]; got.InProgress != 1 {
		t.Fatalf("unknown totals = %#v", got)
	}

	ranged, err := store.Summary(TimeRange{Start: secondStart, End: thirdStart})
	if err != nil {
		t.Fatal(err)
	}
	if ranged.Total.Requests != 1 || ranged.Total.Failures != 1 || ranged.Total.InputTokens != 7 {
		t.Fatalf("ranged summary = %#v", ranged.Total)
	}
}

func TestStoreMarksDuplicateBeginAsPersistenceFailure(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	started := time.Now()
	request := Request{RequestID: "one", StartedAt: started, Endpoint: "responses"}
	if err := store.Begin(request); err != nil {
		t.Fatal(err)
	}
	if err := store.Begin(request); err == nil {
		t.Fatal("duplicate begin unexpectedly succeeded")
	}
	if store.Err() == nil {
		t.Fatal("duplicate begin did not mark store unavailable")
	}
}

func TestStorePersistsMetadataLearnedAfterBegin(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	started := time.Now()
	if err := store.Begin(Request{RequestID: "deferred-metadata", StartedAt: started, Endpoint: "responses"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateMetadata(RequestMetadata{RequestID: "deferred-metadata", Model: "requested-model", ReasoningEffort: "high"}); err != nil {
		t.Fatal(err)
	}
	var inProgressModel, inProgressEffort string
	if err := store.db.QueryRow(`SELECT model, reasoning_effort FROM request_logs WHERE request_id = 'deferred-metadata'`).Scan(&inProgressModel, &inProgressEffort); err != nil {
		t.Fatal(err)
	}
	if inProgressModel != "requested-model" || inProgressEffort != "high" {
		t.Fatalf("in-progress metadata = model %q effort %q", inProgressModel, inProgressEffort)
	}
	if err := store.Finish(Completion{
		RequestID:  "deferred-metadata",
		FinishedAt: started.Add(time.Second),
		Model:      "gpt-6-luna",
		Outcome:    OutcomeFailure,
		ErrorKind:  "upstream_request",
	}); err != nil {
		t.Fatal(err)
	}
	var model, reasoningEffort string
	if err := store.db.QueryRow(`SELECT model, reasoning_effort FROM request_logs WHERE request_id = 'deferred-metadata'`).Scan(&model, &reasoningEffort); err != nil {
		t.Fatal(err)
	}
	if model != "gpt-6-luna" || reasoningEffort != "high" {
		t.Fatalf("deferred metadata = model %q effort %q", model, reasoningEffort)
	}
}

func TestOpenExistingRequiresDatabase(t *testing.T) {
	if _, err := OpenExisting(t.TempDir()); err == nil {
		t.Fatal("OpenExisting unexpectedly created a database")
	}
}

func TestOpenExistingCanReadWhileWriterStoreIsOpen(t *testing.T) {
	directory := t.TempDir()
	writer, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	started := time.Now()
	if err := writer.Begin(Request{RequestID: "live", StartedAt: started, Endpoint: "responses", Model: "gpt-6-luna"}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Finish(Completion{RequestID: "live", FinishedAt: started.Add(time.Millisecond), Outcome: OutcomeSuccess}); err != nil {
		t.Fatal(err)
	}

	reader, err := OpenExisting(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	summary, err := reader.Summary(TimeRange{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Total.Successes != 1 {
		t.Fatalf("live summary = %#v", summary.Total)
	}
}

func int64Pointer(value int64) *int64 { return &value }
