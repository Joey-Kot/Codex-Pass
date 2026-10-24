package requestlog

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// LogEntry contains one request's timing and usage. Nil fields preserve
// missing completion times or usage values rather than presenting them as zero.
type LogEntry struct {
	RequestID       string
	StartedAt       time.Time
	FinishedAt      *time.Time
	Model           string
	InputTokens     *int64
	CacheReadTokens *int64
	OutputTokens    *int64
	ReasoningTokens *int64
}

// RequestLogs holds the request counts and entries from the same read snapshot.
type RequestLogs struct {
	Summary Summary
	Entries []LogEntry
}

// Logs filters by start time, sorts, then applies limit (zero means no limit).
// The default order is newest first; ascending selects oldest first. Request IDs
// break start-time ties. Counts and entries cover the same selected requests.
func (s *Store) Logs(timeRange TimeRange, ascending bool, limit int) (RequestLogs, error) {
	if limit < 0 {
		return RequestLogs{}, fmt.Errorf("request log limit must not be negative")
	}
	where, arguments, err := requestLogWhere(timeRange)
	if err != nil {
		return RequestLogs{}, err
	}
	direction := "DESC"
	if ascending {
		direction = "ASC"
	}
	if limit > 0 {
		where = ` WHERE request_id IN (SELECT request_id FROM request_logs` + where +
			` ORDER BY started_at_ms ` + direction + `, request_id ` + direction + ` LIMIT ?)`
		arguments = append(arguments, limit)
	}
	tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return RequestLogs{}, fmt.Errorf("begin request log listing transaction: %w", err)
	}
	defer tx.Rollback()

	summary, err := querySummary(tx, where, arguments)
	if err != nil {
		return RequestLogs{}, err
	}
	rows, err := tx.Query(`SELECT request_id, started_at_ms, finished_at_ms,
		COALESCE(model, '(unknown)'), input_tokens, cache_read_tokens, output_tokens, reasoning_tokens
		FROM request_logs`+where+` ORDER BY started_at_ms `+direction+`, request_id `+direction, arguments...)
	if err != nil {
		return RequestLogs{}, fmt.Errorf("query request logs: %w", err)
	}
	defer rows.Close()
	result := RequestLogs{Summary: summary}
	for rows.Next() {
		var entry LogEntry
		var startedAt int64
		var finishedAt sql.NullInt64
		if err := rows.Scan(&entry.RequestID, &startedAt, &finishedAt, &entry.Model,
			&entry.InputTokens, &entry.CacheReadTokens, &entry.OutputTokens, &entry.ReasoningTokens); err != nil {
			return RequestLogs{}, fmt.Errorf("scan request log: %w", err)
		}
		entry.StartedAt = time.UnixMilli(startedAt)
		if finishedAt.Valid {
			value := time.UnixMilli(finishedAt.Int64)
			entry.FinishedAt = &value
		}
		result.Entries = append(result.Entries, entry)
	}
	if err := rows.Err(); err != nil {
		return RequestLogs{}, fmt.Errorf("iterate request logs: %w", err)
	}
	if err := rows.Close(); err != nil {
		return RequestLogs{}, fmt.Errorf("close request logs: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return RequestLogs{}, fmt.Errorf("commit request log listing transaction: %w", err)
	}
	return result, nil
}
