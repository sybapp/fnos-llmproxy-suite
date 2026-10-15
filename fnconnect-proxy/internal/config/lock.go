package config

import (
	"context"
	"fnconnect-proxy/internal/filelock"
)

func lockConfig(ctx context.Context, path string) (func(), error) {
	return filelock.Acquire(ctx, path)
}
