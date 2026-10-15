package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Config struct {
	Server            string    `json:"server"`
	TargetURL         string    `json:"target_url"`
	Username          string    `json:"username,omitempty"`
	DeviceType        string    `json:"device_type"`
	DeviceName        string    `json:"device_name"`
	FNOSToken         string    `json:"fnos_token,omitempty"`
	FNOSTokenExpires  time.Time `json:"fnos_token_expires_at,omitempty"`
	LongToken         string    `json:"long_token,omitempty"`
	LongTokenExpires  time.Time `json:"long_token_expires_at,omitempty"`
	EntryToken        string    `json:"entry_token,omitempty"`
	EntryTokenExpires time.Time `json:"entry_token_expires_at,omitempty"`
	SignKey           string    `json:"sign_key,omitempty"`
	BackID            string    `json:"back_id,omitempty"`
	UID               int64     `json:"uid,omitempty"`
	Admin             bool      `json:"admin,omitempty"`
	Listen            string    `json:"listen"`
	UpdatedAt         time.Time `json:"updated_at"`
}

func Default() Config {
	return Config{
		DeviceType: "Browser",
		DeviceName: "fnconnect-proxy",
		Listen:     "127.0.0.1:13431",
	}
}

func DefaultPath() (string, error) {
	if p := strings.TrimSpace(os.Getenv("FNCONNECT_PROXY_CONFIG")); p != "" {
		return p, nil
	}
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "fnconnect-proxy", "config.json"), nil
}

func Load(path string) (Config, error) {
	cfg := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return cfg, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, err
	}
	if cfg.DeviceType == "" {
		cfg.DeviceType = "Browser"
	}
	if cfg.DeviceName == "" {
		cfg.DeviceName = "fnconnect-proxy"
	}
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:13431"
	}
	return cfg, nil
}

// Update serializes all cooperating writers, including login, logout and refresh.
func Update(ctx context.Context, path string, change func(*Config) error) (Config, error) {
	if err := ctx.Err(); err != nil {
		return Config{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return Config{}, err
	}
	unlock, err := lockConfig(ctx, path+".lock")
	if err != nil {
		return Config{}, err
	}
	defer unlock()
	if err := ctx.Err(); err != nil {
		return Config{}, err
	}
	cfg, err := Load(path)
	if err != nil {
		return Config{}, err
	}
	previous := cfg.UpdatedAt
	if err := change(&cfg); err != nil {
		return Config{}, err
	}
	cfg.UpdatedAt = time.Now().UTC()
	if !cfg.UpdatedAt.After(previous) {
		cfg.UpdatedAt = previous.Add(time.Nanosecond)
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return Config{}, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".fnconnect-config-*")
	if err != nil {
		return Config{}, err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(append(b, '\n')); err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err != nil {
		return Config{}, err
	}
	if closeErr != nil {
		return Config{}, closeErr
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

var ErrChanged = errors.New("configuration changed during refresh")

// SaveIfUnchanged checks the revision while holding the same lock as Save.
func SaveIfUnchanged(ctx context.Context, path string, expected time.Time, next Config) (Config, error) {
	return Update(ctx, path, func(current *Config) error {
		if !current.UpdatedAt.Equal(expected) {
			return ErrChanged
		}
		*current = next
		return nil
	})
}

func Save(path string, cfg Config) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := Update(ctx, path, func(current *Config) error { *current = cfg; return nil })
	if err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	return nil
}
