// Package debug writes request-correlated JSON Lines diagnostics. It has one
// writer goroutine so concurrent requests never interleave individual records.
package debug

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

type Logger struct {
	file   *os.File
	queue  chan map[string]any
	done   chan struct{}
	closed atomic.Bool

	mu  sync.Mutex
	err error

	acceptMu sync.RWMutex
}

func New(path string) (*Logger, error) {
	if path == "" {
		return nil, errors.New("debug log path is empty")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open debug log: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return nil, fmt.Errorf("set debug log permissions: %w", err)
	}
	logger := &Logger{file: file, queue: make(chan map[string]any, 1024), done: make(chan struct{})}
	go logger.writeLoop()
	return logger, nil
}

func (l *Logger) writeLoop() {
	defer close(l.done)
	encoder := json.NewEncoder(l.file)
	for record := range l.queue {
		if err := encoder.Encode(record); err != nil {
			l.setError(fmt.Errorf("write debug log: %w", err))
		}
	}
	if err := l.file.Sync(); err != nil {
		l.setError(fmt.Errorf("sync debug log: %w", err))
	}
	if err := l.file.Close(); err != nil {
		l.setError(fmt.Errorf("close debug log: %w", err))
	}
}

func (l *Logger) setError(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err == nil {
		l.err = err
	}
}

func (l *Logger) Err() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

// Log blocks only when the bounded writer queue is full. That deliberate
// backpressure means debug mode never silently drops a record.
func (l *Logger) Log(requestID, stage string, fields map[string]any) {
	if l == nil {
		return
	}
	l.acceptMu.RLock()
	defer l.acceptMu.RUnlock()
	if l.closed.Load() {
		return
	}
	record := make(map[string]any, len(fields)+3)
	record["timestamp"] = time.Now().UTC().Format(time.RFC3339Nano)
	record["request_id"] = requestID
	record["stage"] = stage
	for key, value := range fields {
		record[key] = RedactValue(value)
	}
	l.queue <- record
}

func (l *Logger) Close() error {
	if l == nil {
		return nil
	}
	l.acceptMu.Lock()
	if l.closed.CompareAndSwap(false, true) {
		close(l.queue)
	}
	l.acceptMu.Unlock()
	<-l.done
	return l.Err()
}
