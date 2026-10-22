package sse

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestParserSupportsMultilineCRLFCommentsAndChunks(t *testing.T) {
	input := ": keepalive\r\nevent: response.completed\r\ndata: {\r\ndata:  \"type\": \"response.completed\"\r\ndata: }\r\n\r\n"
	parser := NewParser(&chunkReader{data: []byte(input), sizes: []int{1, 2, 5, 3, 8}}, 1024)
	event, err := parser.Next()
	if err != nil {
		t.Fatal(err)
	}
	if event.Event != "response.completed" {
		t.Fatalf("event = %q", event.Event)
	}
	if got, want := string(event.Data), "{\n \"type\": \"response.completed\"\n}"; got != want {
		t.Fatalf("data = %q, want %q", got, want)
	}
	if _, err := parser.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("second Next error = %v, want EOF", err)
	}
}

func TestParserLargeEventAndLimit(t *testing.T) {
	large := strings.Repeat("x", 256<<10)
	input := []byte("event: large\ndata: {\"payload\":\"" + large + "\"}\n\n")
	parser := NewParser(&chunkReader{data: input, sizes: []int{7, 1, 65537, 13}}, int64(len(input)+1))
	event, err := parser.Next()
	if err != nil || len(event.Data) < len(large) {
		t.Fatalf("large event result len=%d err=%v", len(event.Data), err)
	}
	parser = NewParser(bytes.NewReader(input), 1024)
	if _, err := parser.Next(); !errors.Is(err, ErrEventTooLarge) {
		t.Fatalf("limit error = %v", err)
	}
}

func TestParserResumesAfterOversizedEvent(t *testing.T) {
	stream := "event: oversized\n" +
		"data: " + strings.Repeat("x", 2048) + "\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_after_large\"}}\n\n"
	parser := NewParser(strings.NewReader(stream), 512)
	if _, err := parser.Next(); !errors.Is(err, ErrEventTooLarge) {
		t.Fatalf("first event error = %v, want ErrEventTooLarge", err)
	}
	event, err := parser.Next()
	if err != nil {
		t.Fatal(err)
	}
	if event.Event != "response.completed" || !bytes.Contains(event.Data, []byte(`"resp_after_large"`)) {
		t.Fatalf("event after oversized data = %#v", event)
	}
}

func TestCollectorCompletedAndFallbackOutput(t *testing.T) {
	stream := "event: response.output_item.done\n" +
		"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"item_1\",\"type\":\"message\"}}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":5,\"input_tokens_details\":{\"cached_tokens\":0}}}}\n\n"
	result, err := (Collector{MaxBytes: 1 << 20}).Collect(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(result, &response); err != nil {
		t.Fatal(err)
	}
	if response["id"] != "resp_1" || response["usage"].(map[string]any)["input_tokens"].(float64) != 5 {
		t.Fatalf("completed response changed: %s", result)
	}
	if output := response["output"].([]any); len(output) != 1 || output[0].(map[string]any)["id"] != "item_1" {
		t.Fatalf("fallback output not reconstructed: %s", result)
	}
}

func TestCollectorFailureAndInterruptedStream(t *testing.T) {
	failure := "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\"}}\n\n"
	_, err := (Collector{}).Collect(strings.NewReader(failure))
	var terminal *TerminalError
	if !errors.As(err, &terminal) || terminal.Kind != "response.failed" {
		t.Fatalf("failure error = %#v", err)
	}
	_, err = (Collector{}).Collect(strings.NewReader("event: response.created\ndata: {\"type\":\"response.created\"}\n\n"))
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("interruption error = %v", err)
	}
}

func TestCollectorIgnoresUnknownExtensionEvent(t *testing.T) {
	stream := "event: proxy.keepalive\ndata: this is not JSON\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp\",\"status\":\"completed\",\"output\":[]}}\n\n"
	result, err := (Collector{}).Collect(strings.NewReader(stream))
	if err != nil || !bytes.Contains(result, []byte(`"id":"resp"`)) {
		t.Fatalf("result=%s err=%v", result, err)
	}
}

type chunkReader struct {
	data   []byte
	sizes  []int
	index  int
	offset int
}

func (r *chunkReader) Read(buffer []byte) (int, error) {
	if r.offset >= len(r.data) {
		return 0, io.EOF
	}
	size := r.sizes[r.index%len(r.sizes)]
	r.index++
	if size > len(buffer) {
		size = len(buffer)
	}
	if size > len(r.data)-r.offset {
		size = len(r.data) - r.offset
	}
	copy(buffer, r.data[r.offset:r.offset+size])
	r.offset += size
	return size, nil
}
