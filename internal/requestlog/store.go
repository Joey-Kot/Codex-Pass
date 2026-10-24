// Package requestlog persists request-level usage accounting without storing
// request or response content.
package requestlog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const (
	// DatabaseFilename is the fixed SQLite filename created below --log's
	// directory argument.
	DatabaseFilename = "codex-pass.sqlite"

	schemaVersion = 1
	writeTimeout  = 5 * time.Second
)

// Outcome is the terminal state of a proxied request.
type Outcome string

const (
	OutcomeInProgress Outcome = "in_progress"
	OutcomeSuccess    Outcome = "success"
	OutcomeFailure    Outcome = "failure"
	OutcomeCanceled   Outcome = "canceled"
)

// Request is the information available when a request begins. Model and
// ReasoningEffort originate from the client request and may be empty.
type Request struct {
	RequestID       string
	StartedAt       time.Time
	Endpoint        string
	Model           string
	ReasoningEffort string
}

// RequestMetadata is learned from a request body after the in-progress row
// has been created.
type RequestMetadata struct {
	RequestID       string
	Model           string
	ReasoningEffort string
}

// Completion is the information available when a request ends. A nil token
// means that the upstream did not provide a valid value for that field.
type Completion struct {
	RequestID        string
	FinishedAt       time.Time
	Model            string
	ReasoningEffort  string
	InputTokens      *int64
	CacheWriteTokens *int64
	CacheReadTokens  *int64
	OutputTokens     *int64
	ReasoningTokens  *int64
	Outcome          Outcome
	ErrorKind        string
}

// Recorder is the small persistence surface used by the proxy. It allows
// proxy tests to use an in-memory recorder without depending on SQLite.
type Recorder interface {
	Begin(Request) error
	UpdateMetadata(RequestMetadata) error
	Finish(Completion) error
	Err() error
}

// Store owns the SQLite connection used by a proxy process. database/sql is
// limited to one connection so concurrent proxy requests serialize writes;
// WAL still allows separate sum and req processes to read the database.
type Store struct {
	db   *sql.DB
	path string

	mu       sync.RWMutex
	writeErr error
}

// Open creates the log directory and database when necessary.
func Open(directory string) (*Store, error) {
	return open(directory, true)
}

// OpenExisting opens an existing database for sum or req. It refuses to create
// an empty database so a misspelled --log directory cannot look like zero use.
func OpenExisting(directory string) (*Store, error) {
	path, err := databasePath(directory)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("log database does not exist: %s", path)
		}
		return nil, fmt.Errorf("stat log database: %w", err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("log database path is a directory: %s", path)
	}
	return open(directory, false)
}

func open(directory string, createDirectory bool) (*Store, error) {
	path, err := databasePath(directory)
	if err != nil {
		return nil, err
	}
	if createDirectory {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("create log directory: %w", err)
		}
	}

	dsn := (&url.URL{Scheme: "file", Path: path}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open log database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := initialize(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if createDirectory {
		if err := os.Chmod(path, 0o600); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("set log database permissions: %w", err)
		}
	}
	return &Store{db: db, path: path}, nil
}

func databasePath(directory string) (string, error) {
	directory = strings.TrimSpace(directory)
	if directory == "" {
		return "", errors.New("log directory is empty")
	}
	return filepath.Join(filepath.Clean(directory), DatabaseFilename), nil
}

func initialize(db *sql.DB) error {
	if _, err := db.Exec(`PRAGMA busy_timeout = 5000`); err != nil {
		return fmt.Errorf("configure log database busy timeout: %w", err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode = WAL`); err != nil {
		return fmt.Errorf("enable log database WAL mode: %w", err)
	}
	if _, err := db.Exec(`PRAGMA synchronous = FULL`); err != nil {
		return fmt.Errorf("configure log database synchronous mode: %w", err)
	}

	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read log database schema version: %w", err)
	}
	if version > schemaVersion {
		return fmt.Errorf("log database schema version %d is newer than supported version %d", version, schemaVersion)
	}
	if version == schemaVersion {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin log database schema transaction: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS request_logs (
			request_id TEXT PRIMARY KEY,
			started_at_ms INTEGER NOT NULL,
			finished_at_ms INTEGER,
			endpoint TEXT NOT NULL,
			model TEXT,
			input_tokens INTEGER,
			cache_write_tokens INTEGER,
			cache_read_tokens INTEGER,
			output_tokens INTEGER,
			reasoning_tokens INTEGER,
			reasoning_effort TEXT,
			outcome TEXT NOT NULL CHECK (outcome IN ('in_progress', 'success', 'failure', 'canceled')),
			error_kind TEXT,
			CHECK (input_tokens IS NULL OR input_tokens >= 0),
			CHECK (cache_write_tokens IS NULL OR cache_write_tokens >= 0),
			CHECK (cache_read_tokens IS NULL OR cache_read_tokens >= 0),
			CHECK (output_tokens IS NULL OR output_tokens >= 0),
			CHECK (reasoning_tokens IS NULL OR reasoning_tokens >= 0)
		)
	`); err != nil {
		return fmt.Errorf("create request log table: %w", err)
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS request_logs_started_at_idx ON request_logs(started_at_ms)`); err != nil {
		return fmt.Errorf("create request log time index: %w", err)
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS request_logs_model_idx ON request_logs(model)`); err != nil {
		return fmt.Errorf("create request log model index: %w", err)
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return fmt.Errorf("set log database schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit log database schema transaction: %w", err)
	}
	return nil
}

// Path returns the SQLite database path.
func (s *Store) Path() string { return s.path }

// Err reports the first persistence error observed after successful opening.
// A non-nil error makes later Begin calls fail rather than silently dropping
// accounting records.
func (s *Store) Err() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.writeErr
}

// Close releases the SQLite database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// Begin inserts an in-progress request record.
func (s *Store) Begin(record Request) error {
	if err := s.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(record.RequestID) == "" {
		return errors.New("request log request ID is empty")
	}
	if strings.TrimSpace(record.Endpoint) == "" {
		return errors.New("request log endpoint is empty")
	}
	if record.StartedAt.IsZero() {
		return errors.New("request log start time is empty")
	}
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO request_logs (
			request_id, started_at_ms, endpoint, model, reasoning_effort, outcome
		) VALUES (?, ?, ?, ?, ?, ?)
	`, record.RequestID, record.StartedAt.UTC().UnixMilli(), record.Endpoint, nullableText(record.Model), nullableText(record.ReasoningEffort), OutcomeInProgress)
	if err != nil {
		return s.rememberWriteError(fmt.Errorf("insert request log: %w", err))
	}
	return nil
}

// UpdateMetadata records request fields that are unavailable until after the
// body has been read. It only updates an in-progress row.
func (s *Store) UpdateMetadata(metadata RequestMetadata) error {
	if err := s.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(metadata.RequestID) == "" {
		return errors.New("request log request ID is empty")
	}
	if strings.TrimSpace(metadata.Model) == "" && strings.TrimSpace(metadata.ReasoningEffort) == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	result, err := s.db.ExecContext(ctx, `
		UPDATE request_logs
		SET
			model = COALESCE(?, model),
			reasoning_effort = COALESCE(?, reasoning_effort)
		WHERE request_id = ? AND outcome = ?
	`, nullableText(metadata.Model), nullableText(metadata.ReasoningEffort), metadata.RequestID, OutcomeInProgress)
	if err != nil {
		return s.rememberWriteError(fmt.Errorf("update request log metadata: %w", err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return s.rememberWriteError(fmt.Errorf("inspect request log metadata update: %w", err))
	}
	if affected != 1 {
		return s.rememberWriteError(fmt.Errorf("update request log metadata: no in-progress record for %q", metadata.RequestID))
	}
	return nil
}

// Finish updates an in-progress request with its terminal result.
func (s *Store) Finish(completion Completion) error {
	if err := s.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(completion.RequestID) == "" {
		return errors.New("request log request ID is empty")
	}
	if completion.FinishedAt.IsZero() {
		return errors.New("request log finish time is empty")
	}
	if completion.Outcome != OutcomeSuccess && completion.Outcome != OutcomeFailure && completion.Outcome != OutcomeCanceled {
		return fmt.Errorf("invalid request log terminal outcome %q", completion.Outcome)
	}
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	result, err := s.db.ExecContext(ctx, `
		UPDATE request_logs
		SET
			finished_at_ms = ?,
			model = COALESCE(?, model),
			reasoning_effort = COALESCE(?, reasoning_effort),
			input_tokens = ?,
			cache_write_tokens = ?,
			cache_read_tokens = ?,
			output_tokens = ?,
			reasoning_tokens = ?,
			outcome = ?,
			error_kind = ?
		WHERE request_id = ? AND outcome = ?
	`, completion.FinishedAt.UTC().UnixMilli(), nullableText(completion.Model), nullableText(completion.ReasoningEffort), nullableInt(completion.InputTokens), nullableInt(completion.CacheWriteTokens), nullableInt(completion.CacheReadTokens), nullableInt(completion.OutputTokens), nullableInt(completion.ReasoningTokens), completion.Outcome, nullableText(completion.ErrorKind), completion.RequestID, OutcomeInProgress)
	if err != nil {
		return s.rememberWriteError(fmt.Errorf("finish request log: %w", err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return s.rememberWriteError(fmt.Errorf("inspect request log update: %w", err))
	}
	if affected != 1 {
		return s.rememberWriteError(fmt.Errorf("finish request log: no in-progress record for %q", completion.RequestID))
	}
	return nil
}

func (s *Store) rememberWriteError(err error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr == nil {
		s.writeErr = err
	}
	return s.writeErr
}

func nullableText(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func nullableInt(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}
