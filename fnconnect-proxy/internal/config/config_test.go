package config

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestConcurrentConfigWriters(t *testing.T) {
	if path := os.Getenv("FNCONNECT_CONFIG_TEST_WRITER"); path != "" {
		for i := 0; i < 12; i++ {
			if _, err := Update(context.Background(), path, func(c *Config) error { c.UID++; return nil }); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Save(path, Default()); err != nil {
		t.Fatal(err)
	}
	var writers []*exec.Cmd
	for i := 0; i < 4; i++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestConcurrentConfigWriters$")
		cmd.Env = append(os.Environ(), "FNCONNECT_CONFIG_TEST_WRITER="+path)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		writers = append(writers, cmd)
	}
	for _, cmd := range writers {
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UID != 48 {
		t.Fatalf("lost updates: uid=%d", cfg.UID)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("config permissions: %v", info.Mode())
		}
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".fnconnect-config-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary files left: %v, %v", matches, err)
	}
}

func TestRefreshCannotOverwriteLogin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	original := Default()
	original.FNOSToken = "old"
	if err := Save(path, original); err != nil {
		t.Fatal(err)
	}
	base, _ := Load(path)
	login := base
	login.FNOSToken = "new-login"
	if err := Save(path, login); err != nil {
		t.Fatal(err)
	}
	base.FNOSToken = "stale-refresh"
	if _, err := SaveIfUnchanged(context.Background(), path, base.UpdatedAt, base); !errors.Is(err, ErrChanged) {
		t.Fatalf("expected revision conflict: %v", err)
	}
	got, _ := Load(path)
	if got.FNOSToken != "new-login" {
		t.Fatal("refresh overwrote login")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Update(ctx, path, func(c *Config) error { c.FNOSToken = "cancelled"; return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled update: %v", err)
	}
}

func TestLockCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.lock")
	unlock, err := lockConfig(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if release, err := lockConfig(ctx, path); err == nil {
		release()
		t.Fatal("acquired held lock")
	} else if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
