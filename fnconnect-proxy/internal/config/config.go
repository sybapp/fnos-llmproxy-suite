package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
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

func Save(path string, cfg Config) error {
	cfg.UpdatedAt = time.Now()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0600); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(tmp, 0600)
	}
	return os.Rename(tmp, path)
}
