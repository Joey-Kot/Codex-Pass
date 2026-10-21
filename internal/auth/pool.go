package auth

import (
	"context"
	"errors"
	"hash/fnv"
)

// Pool contains independent auth files. It deliberately does not rotate
// requests: selection is deterministic to preserve cache and session affinity.
type Pool struct {
	credentials []*Credential
	refresh     RefreshConfig
}

func NewPool(credentials []*Credential, refresh RefreshConfig) (*Pool, error) {
	if len(credentials) == 0 {
		return nil, errors.New("auth pool requires at least one credential")
	}
	return &Pool{credentials: credentials, refresh: refresh.normalized()}, nil
}

func (p *Pool) Len() int { return len(p.credentials) }

// Select uses prompt_cache_key when available; otherwise session-id. This makes
// one cache key stable even when callers accidentally vary their session id.
func (p *Pool) Select(promptCacheKey, sessionID string) (index int, credential *Credential) {
	if len(p.credentials) == 1 {
		return 0, p.credentials[0]
	}
	key := ""
	if promptCacheKey != "" {
		key = "cache:" + promptCacheKey
	} else if sessionID != "" {
		key = "session:" + sessionID
	}
	if key == "" {
		return 0, p.credentials[0]
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(key))
	index = int(hash.Sum64() % uint64(len(p.credentials)))
	return index, p.credentials[index]
}

func (p *Pool) EnsureFresh(ctx context.Context, credential *Credential) error {
	return credential.EnsureFresh(ctx, p.refresh)
}

func (p *Pool) Refresh(ctx context.Context, credential *Credential) error {
	return credential.Refresh(ctx, p.refresh)
}
