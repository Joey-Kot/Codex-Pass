package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
)

const (
	DefaultCodexUpstreamURL  = "https://chatgpt.com/backend-api/codex"
	DefaultUpstreamURL       = DefaultCodexUpstreamURL + "/responses"
	DefaultModelsUpstreamURL = DefaultCodexUpstreamURL + "/models"
)

var hopByHopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"TE", "Trailer", "Transfer-Encoding", "Upgrade",
}

func cloneForwardHeaders(source http.Header) http.Header {
	headers := source.Clone()
	for _, name := range hopByHopHeaders {
		headers.Del(name)
	}
	// Headers named by Connection are also hop-by-hop.
	for _, name := range source.Values("Connection") {
		headers.Del(name)
	}
	headers.Del("Authorization")
	headers.Del("ChatGPT-Account-ID")
	headers.Del("Content-Length")
	headers.Set("Accept", "text/event-stream")
	headers.Set("Content-Type", "application/json")
	// Avoid net/http's implicit gzip negotiation, which would otherwise
	// transparently decompress the upstream body before raw SSE forwarding.
	if headers.Get("Accept-Encoding") == "" {
		headers.Set("Accept-Encoding", "identity")
	}
	return headers
}

func responseHeaders(source http.Header) http.Header {
	headers := source.Clone()
	for _, name := range hopByHopHeaders {
		headers.Del(name)
	}
	headers.Del("Content-Length")
	return headers
}

type FieldChange struct {
	Path   string `json:"path"`
	Before any    `json:"before"`
	After  any    `json:"after"`
}

type JSONDiff struct {
	ChangedFields []FieldChange `json:"changed_fields"`
	RemovedFields []FieldChange `json:"removed_fields"`
	AddedFields   []FieldChange `json:"added_fields"`
}

// DiffJSON compares decoded JSON values, so changing insignificant formatting
// for a non-stream request never appears as a business-field rewrite.
func DiffJSON(before, after []byte) (JSONDiff, error) {
	var beforeValue, afterValue any
	if err := json.Unmarshal(before, &beforeValue); err != nil {
		return JSONDiff{}, fmt.Errorf("decode original JSON for diff: %w", err)
	}
	if err := json.Unmarshal(after, &afterValue); err != nil {
		return JSONDiff{}, fmt.Errorf("decode upstream JSON for diff: %w", err)
	}
	diff := JSONDiff{ChangedFields: []FieldChange{}, RemovedFields: []FieldChange{}, AddedFields: []FieldChange{}}
	diffValues("$", beforeValue, afterValue, &diff)
	return diff, nil
}

func diffValues(path string, before, after any, diff *JSONDiff) {
	beforeMap, beforeIsMap := before.(map[string]any)
	afterMap, afterIsMap := after.(map[string]any)
	if beforeIsMap && afterIsMap {
		keys := make(map[string]struct{}, len(beforeMap)+len(afterMap))
		for key := range beforeMap {
			keys[key] = struct{}{}
		}
		for key := range afterMap {
			keys[key] = struct{}{}
		}
		ordered := make([]string, 0, len(keys))
		for key := range keys {
			ordered = append(ordered, key)
		}
		sort.Strings(ordered)
		for _, key := range ordered {
			beforeChild, beforeOK := beforeMap[key]
			afterChild, afterOK := afterMap[key]
			childPath := path + "." + key
			switch {
			case !beforeOK:
				diff.AddedFields = append(diff.AddedFields, FieldChange{Path: childPath, After: afterChild})
			case !afterOK:
				diff.RemovedFields = append(diff.RemovedFields, FieldChange{Path: childPath, Before: beforeChild})
			default:
				diffValues(childPath, beforeChild, afterChild, diff)
			}
		}
		return
	}
	beforeArray, beforeIsArray := before.([]any)
	afterArray, afterIsArray := after.([]any)
	if beforeIsArray && afterIsArray {
		length := len(beforeArray)
		if len(afterArray) > length {
			length = len(afterArray)
		}
		for index := 0; index < length; index++ {
			childPath := fmt.Sprintf("%s[%d]", path, index)
			switch {
			case index >= len(beforeArray):
				diff.AddedFields = append(diff.AddedFields, FieldChange{Path: childPath, After: afterArray[index]})
			case index >= len(afterArray):
				diff.RemovedFields = append(diff.RemovedFields, FieldChange{Path: childPath, Before: beforeArray[index]})
			default:
				diffValues(childPath, beforeArray[index], afterArray[index], diff)
			}
		}
		return
	}
	if !jsonValueEqual(before, after) {
		diff.ChangedFields = append(diff.ChangedFields, FieldChange{Path: path, Before: before, After: after})
	}
}

func jsonValueEqual(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}
