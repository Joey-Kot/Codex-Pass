package sse

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

var (
	ErrIncomplete = errors.New("upstream SSE stream ended before response.completed")
	ErrProtocol   = errors.New("invalid Responses SSE event")
	ErrTooLarge   = errors.New("upstream SSE stream exceeds aggregation size limit")
)

// TerminalError preserves a failed or incomplete upstream event without
// incorrectly reporting the result as a completed response.
type TerminalError struct {
	Kind string
	Data json.RawMessage
}

func (e *TerminalError) Error() string { return "upstream SSE terminal event: " + e.Kind }

type Collector struct {
	MaxBytes int64
	OnEvent  func(Event)
}

// Collect chooses the final response.completed.response JSON object verbatim.
// It reconstructs an absent, null, or empty output field from item.done events,
// and only when a completed event is still present.
func (c Collector) Collect(reader io.Reader) (json.RawMessage, error) {
	counted := &countingReader{reader: reader, limit: c.MaxBytes}
	parser := NewParser(counted, c.MaxBytes)
	var outputItems []json.RawMessage
	for {
		event, err := parser.Next()
		if err != nil {
			if errors.Is(err, errReadLimit) {
				return nil, ErrTooLarge
			}
			if errors.Is(err, ErrEventTooLarge) {
				return nil, ErrTooLarge
			}
			if errors.Is(err, io.EOF) {
				return nil, ErrIncomplete
			}
			return nil, err
		}
		if c.OnEvent != nil {
			c.OnEvent(event)
		}
		if bytes.Equal(bytes.TrimSpace(event.Data), []byte("[DONE]")) {
			return nil, ErrIncomplete
		}
		if len(bytes.TrimSpace(event.Data)) == 0 {
			continue
		}
		if event.Event != "" && !knownEvent(event.Event) {
			// Extension events are allowed by SSE. They are intentionally not
			// treated as a protocol failure just because their payload is not
			// a Responses JSON object.
			continue
		}
		var envelope struct {
			Type     string          `json:"type"`
			Response json.RawMessage `json:"response"`
			Item     json.RawMessage `json:"item"`
		}
		if err := json.Unmarshal(event.Data, &envelope); err != nil {
			return nil, fmt.Errorf("%w: event %q contains invalid JSON: %v", ErrProtocol, event.Event, err)
		}
		kind := envelope.Type
		if kind == "" {
			kind = event.Event
		}
		if !knownEvent(kind) {
			continue
		}
		switch kind {
		case "response.output_item.done":
			if len(envelope.Item) != 0 && json.Valid(envelope.Item) {
				outputItems = append(outputItems, append(json.RawMessage(nil), envelope.Item...))
			}
		case "response.completed":
			if !isJSONObject(envelope.Response) {
				return nil, fmt.Errorf("%w: response.completed does not contain a response object", ErrProtocol)
			}
			return addFallbackOutput(envelope.Response, outputItems)
		case "response.failed", "response.incomplete", "error":
			return nil, &TerminalError{Kind: kind, Data: append(json.RawMessage(nil), event.Data...)}
		}
	}
}

func knownEvent(kind string) bool {
	switch kind {
	case "response.created", "response.in_progress", "response.output_item.added", "response.output_item.done", "response.content_part.done", "response.output_text.delta", "response.completed", "response.failed", "response.incomplete", "error":
		return true
	default:
		return false
	}
}

var errReadLimit = errors.New("read limit exceeded")

type countingReader struct {
	reader io.Reader
	limit  int64
	read   int64
}

func (r *countingReader) Read(buffer []byte) (int, error) {
	if r.limit > 0 && r.read >= r.limit {
		return 0, errReadLimit
	}
	if r.limit > 0 && int64(len(buffer)) > r.limit-r.read {
		buffer = buffer[:r.limit-r.read]
	}
	n, err := r.reader.Read(buffer)
	r.read += int64(n)
	return n, err
}

func isJSONObject(value json.RawMessage) bool {
	return len(bytes.TrimSpace(value)) > 1 && bytes.TrimSpace(value)[0] == '{' && json.Valid(value)
}

func addFallbackOutput(response json.RawMessage, items []json.RawMessage) (json.RawMessage, error) {
	if len(items) == 0 {
		return append(json.RawMessage(nil), response...), nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(response, &object); err != nil {
		return nil, fmt.Errorf("%w: decode completed response: %v", ErrProtocol, err)
	}
	if existing, found := object["output"]; found && !isMissingOrEmptyOutput(existing) {
		return append(json.RawMessage(nil), response...), nil
	}
	output, err := json.Marshal(items)
	if err != nil {
		return nil, fmt.Errorf("%w: encode reconstructed output: %v", ErrProtocol, err)
	}
	object["output"] = output
	completed, err := json.Marshal(object)
	if err != nil {
		return nil, fmt.Errorf("%w: encode completed response: %v", ErrProtocol, err)
	}
	return completed, nil
}

func isMissingOrEmptyOutput(value json.RawMessage) bool {
	value = bytes.TrimSpace(value)
	if len(value) == 0 || bytes.Equal(value, []byte("null")) {
		return true
	}
	var output []json.RawMessage
	return json.Unmarshal(value, &output) == nil && output != nil && len(output) == 0
}
