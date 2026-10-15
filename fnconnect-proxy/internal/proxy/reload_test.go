package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	appcfg "fnconnect-proxy/internal/config"
	"fnconnect-proxy/internal/fnos"
)

func TestReloginMovesRouteAndCookiesTogether(t *testing.T) {
	var oldHits, newHits atomic.Int32
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		oldHits.Add(1)
		if strings.Contains(r.Header.Get("Cookie"), "new-") {
			t.Error("new NAS credentials sent to old target")
		}
		fmt.Fprint(w, "old")
	}))
	defer old.Close()
	next := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		newHits.Add(1)
		token, err := r.Cookie("entry-token")
		if err != nil || token.Value != "new-entry" {
			t.Error("new target missing new entry token")
		}
		if _, err := r.Cookie("fnos-token"); err == nil {
			t.Error("gateway mode did not change with target")
		}
		if r.URL.Path != "/v1/models" {
			t.Errorf("old route path retained: %s", r.URL.Path)
		}
		fmt.Fprint(w, "new")
	}))
	defer next.Close()
	cfg := appcfg.Default()
	cfg.TargetURL = old.URL + "/app/llmproxy"
	cfg.FNOSToken = "old-fnos"
	cfg.EntryToken = "old-entry"
	cfg.EntryTokenExpires = time.Now().Add(6 * time.Hour)
	cfg.FNOSTokenExpires = time.Now().Add(6 * time.Hour)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := appcfg.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	cfg, _ = appcfg.Load(path)
	mgr := NewTokenManager(path, cfg)
	handler, err := NewServer(mgr, cfg.TargetURL)
	if err != nil {
		t.Fatal(err)
	}
	request := func(expected string) {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", "http://local/v1/models", nil))
		if response.Code != 200 || response.Body.String() != expected {
			t.Fatalf("route mismatch: %d %s", response.Code, response.Body.String())
		}
	}
	request("old")
	cfg.TargetURL = next.URL
	cfg.Server = "new-nas"
	cfg.EntryToken = "new-entry"
	cfg.FNOSToken = ""
	if err := appcfg.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	request("new")
	if oldHits.Load() != 1 || newHits.Load() != 1 {
		t.Fatalf("wrong destinations: old=%d new=%d", oldHits.Load(), newHits.Load())
	}
}

type blockedRefreshClient struct {
	entered chan struct{}
	release chan struct{}
}

func (c *blockedRefreshClient) Connect(context.Context) error { return nil }
func (c *blockedRefreshClient) Close() error                  { return nil }
func (c *blockedRefreshClient) RefreshWithFNOSToken(context.Context, string, string, string) (fnos.Session, error) {
	return fnos.Session{}, errors.New("unexpected entry refresh")
}
func (c *blockedRefreshClient) RefreshWithLongToken(ctx context.Context, _ string, _ string, _ string, _ string, _ string) (fnos.Session, error) {
	close(c.entered)
	select {
	case <-c.release:
		return fnos.Session{FNOSToken: "stale-refresh-fnos", EntryToken: "stale-refresh-entry"}, nil
	case <-ctx.Done():
		return fnos.Session{}, ctx.Err()
	}
}
func TestConcurrentReloginAndLogoutWinOverRefresh(t *testing.T) {
	for _, logout := range []bool{false, true} {
		t.Run(map[bool]string{false: "relogin", true: "logout"}[logout], func(t *testing.T) {
			cfg := appcfg.Default()
			cfg.Server = "old-nas"
			cfg.TargetURL = "https://old-nas/app/llmproxy"
			cfg.LongToken = "old-long"
			cfg.FNOSToken = "old-fnos"
			cfg.EntryToken = "old-entry"
			path := filepath.Join(t.TempDir(), "config.json")
			if err := appcfg.Save(path, cfg); err != nil {
				t.Fatal(err)
			}
			cfg, _ = appcfg.Load(path)
			mgr := NewTokenManager(path, cfg)
			mock := &blockedRefreshClient{entered: make(chan struct{}), release: make(chan struct{})}
			mgr.newClient = func(string, time.Duration) sessionClient { return mock }
			done := make(chan error, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			go func() { done <- mgr.RenewSession(ctx) }()
			select {
			case <-mock.entered:
			case <-ctx.Done():
				t.Fatal("refresh never started")
			}
			next := cfg
			if logout {
				next.FNOSToken = ""
				next.EntryToken = ""
				next.LongToken = ""
				next.SignKey = ""
			} else {
				next.TargetURL = "https://new-nas/app/llmproxy"
				next.Server = "new-nas"
				next.FNOSToken = "new-login-fnos"
				next.EntryToken = "new-login-entry"
				next.LongToken = "new-login-long"
			}
			if err := appcfg.Save(path, next); err != nil {
				t.Fatal(err)
			}
			close(mock.release)
			err := <-done
			if !logout && err != nil {
				t.Fatal(err)
			}
			if logout && err == nil {
				t.Fatal("refresh claimed usable session after logout")
			}
			saved, err := appcfg.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			memory := mgr.Config()
			if saved.FNOSToken != next.FNOSToken || memory.FNOSToken != next.FNOSToken || saved.TargetURL != next.TargetURL || memory.TargetURL != next.TargetURL {
				t.Fatal("stale refresh overwrote newer credentials or route")
			}
		})
	}
}

func TestLLMAuthFailureDoesNotRenewNASSession(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		fmt.Fprint(w, `{"error":"invalid api key"}`)
	}))
	defer upstream.Close()
	cfg := appcfg.Default()
	cfg.TargetURL = upstream.URL + "/app/llmproxy"
	cfg.EntryToken = "entry"
	cfg.FNOSToken = "fnos"
	cfg.EntryTokenExpires = time.Now().Add(6 * time.Hour)
	cfg.FNOSTokenExpires = time.Now().Add(6 * time.Hour)
	mgr := NewTokenManager(filepath.Join(t.TempDir(), "config.json"), cfg)
	mgr.newClient = func(string, time.Duration) sessionClient { t.Fatal("LLM failure triggered NAS refresh"); return nil }
	h, err := NewServer(mgr, cfg.TargetURL)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	h.ServeHTTP(response, httptest.NewRequest("GET", "http://local/v1/models", nil))
	if response.Code != 401 || response.Body.String() != `{"error":"invalid api key"}` {
		t.Fatalf("changed upstream error: %d %s", response.Code, response.Body.String())
	}
}
