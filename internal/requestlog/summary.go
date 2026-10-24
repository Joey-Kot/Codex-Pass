package requestlog

import (
	"context"
	"database/sql"
	"fmt"
)

// Summary holds totals for every matching request and a breakdown by model.
type Summary struct {
	Total   Aggregate
	ByModel []ModelAggregate
}

// Aggregate is the set of counters displayed by sum and req.
type Aggregate struct {
	Requests    int64
	Successes   int64
	Failures    int64
	Canceled    int64
	InProgress  int64
	InputTokens int64

	CacheWriteTokens int64
	CacheReadTokens  int64
	OutputTokens     int64
	ReasoningTokens  int64
}

// ModelAggregate contains an Aggregate for one actual or requested model.
type ModelAggregate struct {
	Model string
	Aggregate
}

// RequestUsage contains only the fields needed to price one request. Nil token
// fields preserve missing usage values rather than treating them as zero.
type RequestUsage struct {
	Model           string
	InputTokens     *int64
	CacheReadTokens *int64
	OutputTokens    *int64
}

// Summary calculates both the global and per-model aggregates in one
// consistent time range. The range filters on request start time.
func (s *Store) Summary(timeRange TimeRange) (Summary, error) {
	return s.SummaryWithUsage(timeRange, nil)
}

// SummaryWithUsage also visits each matching request's usage in the same read
// snapshot, without retaining request rows or sorting them. A nil visitor skips
// the usage scan. The visitor must not query the same Store while it is running.
func (s *Store) SummaryWithUsage(timeRange TimeRange, visit func(RequestUsage) error) (Summary, error) {
	where, arguments, err := requestLogWhere(timeRange)
	if err != nil {
		return Summary{}, err
	}
	tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Summary{}, fmt.Errorf("begin request log summary transaction: %w", err)
	}
	defer tx.Rollback()

	result, err := querySummary(tx, where, arguments)
	if err != nil {
		return Summary{}, err
	}
	if visit != nil {
		if err := visitRequestUsage(tx, where, arguments, visit); err != nil {
			return Summary{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Summary{}, fmt.Errorf("commit request log summary transaction: %w", err)
	}
	return result, nil
}

func visitRequestUsage(tx *sql.Tx, where string, arguments []any, visit func(RequestUsage) error) error {
	rows, err := tx.Query(`SELECT COALESCE(model, '(unknown)'), input_tokens, cache_read_tokens, output_tokens
		FROM request_logs`+where, arguments...)
	if err != nil {
		return fmt.Errorf("query request usage: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var usage RequestUsage
		if err := rows.Scan(&usage.Model, &usage.InputTokens, &usage.CacheReadTokens, &usage.OutputTokens); err != nil {
			return fmt.Errorf("scan request usage: %w", err)
		}
		if err := visit(usage); err != nil {
			return fmt.Errorf("process request usage: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate request usage: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close request usage: %w", err)
	}
	return nil
}

func querySummary(tx *sql.Tx, where string, arguments []any) (Summary, error) {
	query := `SELECT ` + aggregateColumns + ` FROM request_logs` + where
	var total Aggregate
	if err := tx.QueryRow(query, arguments...).Scan(aggregateDestinations(&total)...); err != nil {
		return Summary{}, fmt.Errorf("query request log totals: %w", err)
	}

	rows, err := tx.Query(`SELECT COALESCE(model, '(unknown)') AS model, `+aggregateColumns+` FROM request_logs`+where+` GROUP BY COALESCE(model, '(unknown)') ORDER BY model COLLATE NOCASE`, arguments...)
	if err != nil {
		return Summary{}, fmt.Errorf("query request log model totals: %w", err)
	}
	result := Summary{Total: total}
	for rows.Next() {
		var row ModelAggregate
		if err := rows.Scan(append([]any{&row.Model}, aggregateDestinations(&row.Aggregate)...)...); err != nil {
			_ = rows.Close()
			return Summary{}, fmt.Errorf("scan request log model totals: %w", err)
		}
		result.ByModel = append(result.ByModel, row)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return Summary{}, fmt.Errorf("iterate request log model totals: %w", err)
	}
	if err := rows.Close(); err != nil {
		return Summary{}, fmt.Errorf("close request log model totals: %w", err)
	}
	return result, nil
}

const aggregateColumns = `
	COUNT(*) AS requests,
	COALESCE(SUM(CASE WHEN outcome = 'success' THEN 1 ELSE 0 END), 0) AS successes,
	COALESCE(SUM(CASE WHEN outcome = 'failure' THEN 1 ELSE 0 END), 0) AS failures,
	COALESCE(SUM(CASE WHEN outcome = 'canceled' THEN 1 ELSE 0 END), 0) AS canceled,
	COALESCE(SUM(CASE WHEN outcome = 'in_progress' THEN 1 ELSE 0 END), 0) AS in_progress,
	COALESCE(SUM(input_tokens), 0) AS input_tokens,
	COALESCE(SUM(cache_write_tokens), 0) AS cache_write_tokens,
	COALESCE(SUM(cache_read_tokens), 0) AS cache_read_tokens,
	COALESCE(SUM(output_tokens), 0) AS output_tokens,
	COALESCE(SUM(reasoning_tokens), 0) AS reasoning_tokens`

func aggregateDestinations(target *Aggregate) []any {
	return []any{
		&target.Requests,
		&target.Successes,
		&target.Failures,
		&target.Canceled,
		&target.InProgress,
		&target.InputTokens,
		&target.CacheWriteTokens,
		&target.CacheReadTokens,
		&target.OutputTokens,
		&target.ReasoningTokens,
	}
}

func requestLogWhere(timeRange TimeRange) (string, []any, error) {
	if timeRange.All {
		return "", nil, nil
	}
	if timeRange.Start.IsZero() || timeRange.End.IsZero() || !timeRange.End.After(timeRange.Start) {
		return "", nil, fmt.Errorf("invalid request log time range")
	}
	return " WHERE started_at_ms >= ? AND started_at_ms < ?", []any{timeRange.Start.UTC().UnixMilli(), timeRange.End.UTC().UnixMilli()}, nil
}
