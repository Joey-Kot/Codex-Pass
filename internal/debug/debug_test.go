package debug

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestRedaction(t *testing.T) {
	value := `{"access_token":"secret-access","refresh_token":"secret-refresh","normal":"sk-local-secret","jwt":"eyJhbGciOiJub25lIn0.eyJleHAiOjF9.sig"}`
	redacted := RedactText(value)
	for _, secret := range []string{"secret-access", "secret-refresh", "sk-local-secret", "eyJhbGci"} {
		if strings.Contains(redacted, secret) {
			t.Fatalf("secret %q remained in %s", secret, redacted)
		}
	}
	if got := RedactValue(map[string]any{"Authorization": "Bearer secret", "usage": map[string]any{"input_tokens": 0}}).(map[string]any); got["Authorization"] != "[REDACTED]" {
		t.Fatalf("authorization not redacted: %#v", got)
	}
}

func TestLoggerWritesAtomicJSONLinesWith0600Permissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "debug.jsonl")
	logger, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for index := range 100 {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			logger.Log("req", "UPSTREAM_SSE", map[string]any{"index": index, "authorization": "Bearer secret"})
		}(index)
	}
	group.Wait()
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("permissions = %o", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if lines := bytes.Count(data, []byte("\n")); lines != 100 {
		t.Fatalf("line count = %d", lines)
	}
	if bytes.Contains(data, []byte("secret")) || !bytes.Contains(data, []byte("[REDACTED]")) {
		t.Fatalf("log did not redact: %s", data)
	}
}
