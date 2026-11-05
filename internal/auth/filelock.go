package auth

import (
	"context"
	"fmt"
	"os"
	"time"
)

// Keep the lock file in place so it retains its identity across auth file
// replacements and while other processes are waiting on it.
func lockAuthFile(ctx context.Context, path string) (release func(), err error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open auth refresh lock: %w", err)
	}
	defer func() {
		if release == nil {
			_ = file.Close()
		}
	}()

	retry := time.NewTicker(50 * time.Millisecond)
	defer retry.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("wait for auth refresh lock %q: %w", path, err)
		}
		locked, err := tryLockAuthFile(file)
		if err != nil {
			return nil, fmt.Errorf("acquire auth refresh lock %q: %w", path, err)
		}
		if locked {
			return func() {
				unlockAuthFile(file)
				_ = file.Close()
			}, nil
		}
		select {
		case <-ctx.Done():
		case <-retry.C:
		}
	}
}
