package debug

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

var (
	bearerPattern = regexp.MustCompile(`(?i)(bearer[[:space:]]+)[A-Za-z0-9._~+/=-]+`)
	skPattern     = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]+`)
	jwtPattern    = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]*\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)
	jsonSecret    = regexp.MustCompile(`(?i)("(?:access_token|refresh_token|id_token|api_key|authorization)"[[:space:]]*:[[:space:]]*")[^"]*(")`)
)

func sensitiveKey(key string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
	switch normalized {
	case "authorization", "proxy_authorization", "access_token", "refresh_token", "id_token", "api_key", "openai_api_key":
		return true
	default:
		return false
	}
}

// RedactText intentionally works on complete logical log values. Streamed SSE
// logging happens after parsing one full SSE event, so a secret cannot cross a
// log-chunk boundary and bypass this function.
func RedactText(value string) string {
	value = jsonSecret.ReplaceAllString(value, `${1}[REDACTED]${2}`)
	value = bearerPattern.ReplaceAllString(value, `${1}[REDACTED]`)
	value = skPattern.ReplaceAllString(value, "[REDACTED]")
	return jwtPattern.ReplaceAllString(value, "[REDACTED]")
}

// RedactValue recursively removes credential values while keeping request and
// response business data useful for diagnostics.
func RedactValue(value any) any {
	switch typed := value.(type) {
	case string:
		return RedactText(typed)
	case []byte:
		return RedactText(string(typed))
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			if sensitiveKey(key) {
				result[key] = "[REDACTED]"
				continue
			}
			result[key] = RedactValue(item)
		}
		return result
	case map[string][]string:
		result := make(map[string]any, len(typed))
		for key, items := range typed {
			if sensitiveKey(key) {
				result[key] = "[REDACTED]"
				continue
			}
			clean := make([]string, len(items))
			for index, item := range items {
				clean[index] = RedactText(item)
			}
			result[key] = clean
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for index, item := range typed {
			result[index] = RedactValue(item)
		}
		return result
	default:
		return value
	}
}

func Headers(headers http.Header) map[string][]string {
	result := make(map[string][]string, len(headers))
	for key, values := range headers {
		copied := make([]string, len(values))
		copy(copied, values)
		result[key] = copied
	}
	return result
}

func Error(err error) string {
	if err == nil {
		return ""
	}
	return RedactText(fmt.Sprint(err))
}
