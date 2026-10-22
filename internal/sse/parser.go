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
	var dataBytes int64
	hasField := false
	for {
		line, err := p.readLine()
		if errors.Is(err, ErrEventTooLarge) {
			if discardErr := p.discardEvent(); discardErr != nil && !errors.Is(discardErr, io.EOF) {
				return Event{}, fmt.Errorf("discard oversized SSE event: %w", discardErr)
			}
			return Event{}, ErrEventTooLarge
		}
		if err == nil || len(line) > 0 {
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
					additionalBytes := int64(len(value))
					if len(dataLines) > 0 {
						additionalBytes++
					}
					if p.maxEventBytes > 0 && dataBytes+additionalBytes > p.maxEventBytes {
						if discardErr := p.discardEvent(); discardErr != nil && !errors.Is(discardErr, io.EOF) {
							return Event{}, fmt.Errorf("discard oversized SSE event: %w", discardErr)
						}
						return Event{}, ErrEventTooLarge
					}
					dataLines = append(dataLines, []byte(value))
					dataBytes += additionalBytes
					hasField = true
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

// readLine never accumulates more than maxEventBytes when a size limit is in
// effect. ReadString would allocate an unbounded string for one malformed
// physical line before the event limit could be checked.
func (p *Parser) readLine() (string, error) {
	var line []byte
	for {
		fragment, err := p.reader.ReadSlice('\n')
		if len(fragment) > 0 {
			if p.maxEventBytes > 0 && int64(len(line))+int64(len(fragment)) > p.maxEventBytes {
				if errors.Is(err, bufio.ErrBufferFull) {
					if discardErr := p.discardPhysicalLine(); discardErr != nil && !errors.Is(discardErr, io.EOF) {
						return "", fmt.Errorf("discard oversized SSE line: %w", discardErr)
					}
				}
				return "", ErrEventTooLarge
			}
			line = append(line, fragment...)
		}
		if err == nil {
			return trimLine(line), nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return trimLine(line), err
	}
}

func (p *Parser) discardPhysicalLine() error {
	for {
		_, err := p.reader.ReadSlice('\n')
		if err == nil {
			return nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return err
	}
}

// discardEvent consumes the remaining lines of an oversized event so a caller
// may continue parsing the next event without being desynchronized.
func (p *Parser) discardEvent() error {
	for {
		blank, err := p.discardLine()
		if blank {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (p *Parser) discardLine() (bool, error) {
	blank := true
	for {
		fragment, err := p.reader.ReadSlice('\n')
		for _, character := range fragment {
			if character != '\r' && character != '\n' {
				blank = false
			}
		}
		if err == nil {
			return blank, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return blank, err
	}
}

func trimLine(line []byte) string {
	if len(line) > 0 && line[len(line)-1] == '\n' {
		line = line[:len(line)-1]
	}
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	return string(line)
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
