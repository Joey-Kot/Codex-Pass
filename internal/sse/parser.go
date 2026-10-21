// Package sse provides a size-safe Server-Sent Events parser. It intentionally
// does not use bufio.Scanner, whose default token limit is too small for model
// response events.
package sse

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
)

var ErrEventTooLarge = errors.New("SSE event exceeds configured size limit")

type Event struct {
	Event string
	Data  []byte
	ID    string
}

type Parser struct {
	reader        *bufio.Reader
	maxEventBytes int64
}

// NewParser accepts a zero limit for an unlimited event size. Aggregating
// callers should pass their aggregate limit; transparent stream forwarding
// does not parse the delivery path.
func NewParser(reader io.Reader, maxEventBytes int64) *Parser {
	return &Parser{reader: bufio.NewReader(reader), maxEventBytes: maxEventBytes}
}

// Next returns one dispatched SSE event. Comments and keepalives are ignored.
// A final event without its trailing blank line is returned before io.EOF.
func (p *Parser) Next() (Event, error) {
	var event Event
	var dataLines [][]byte
	hasField := false
	for {
		line, err := p.reader.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimSuffix(line, "\n")
			line = strings.TrimSuffix(line, "\r")
			if line == "" {
				if hasField {
					event.Data = bytes.Join(dataLines, []byte("\n"))
					return event, nil
				}
			} else if !strings.HasPrefix(line, ":") {
				field, value := splitField(line)
				switch field {
				case "event":
					event.Event = value
					hasField = true
				case "data":
					dataLines = append(dataLines, []byte(value))
					hasField = true
					if p.maxEventBytes > 0 && eventSize(dataLines) > p.maxEventBytes {
						return Event{}, ErrEventTooLarge
					}
				case "id":
					if !strings.ContainsRune(value, '\x00') {
						event.ID = value
					}
					hasField = true
				case "retry":
					// Retry is a stream-level reconnection hint and has no bearing
					// on a one-shot Responses request.
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) && hasField {
				event.Data = bytes.Join(dataLines, []byte("\n"))
				return event, nil
			}
			if errors.Is(err, io.EOF) {
				return Event{}, io.EOF
			}
			return Event{}, fmt.Errorf("read SSE stream: %w", err)
		}
	}
}

func splitField(line string) (field, value string) {
	field, value, found := strings.Cut(line, ":")
	if !found {
		return line, ""
	}
	if strings.HasPrefix(value, " ") {
		value = value[1:]
	}
	return field, value
}

func eventSize(lines [][]byte) int64 {
	var total int64
	for _, line := range lines {
		total += int64(len(line))
	}
	if len(lines) > 1 {
		total += int64(len(lines) - 1)
	}
	return total
}
