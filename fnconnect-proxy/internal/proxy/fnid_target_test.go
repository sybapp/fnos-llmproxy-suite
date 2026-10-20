package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	appcfg "fnconnect-proxy/internal/config"
)

func TestIsUnifiedSupportsGatewayPathPrefix(t *testing.T) {
	tests := []struct {
		target string
		want   bool
	}{
		{"https://vas4er.fnos.net/app/llmproxy", true},
		{"https://gateway.example.com/vas4er/app/llmproxy", true},
		{"https://gateway.example.com/vas4er/app/llmproxy/", true},
		{"https://example.com/prefix/app/llmproxy", true},
		{"https://gateway.example.com/vas4er/myapp/llmproxy", false},
		{"https://gateway.example.com/vas4er/app", false},
		{"https://example.com/", false},
	}

	for _, tt := range tests {
		t.Run(tt.target, func(t *testing.T) {
			if got := isUnified(tt.target); got != tt.want {
				t.Fatalf("isUnified(%q) = %v, want %v", tt.target, got, tt.want)
			}
		})
	}
}

func TestValidateTargetRejectsFNConnectPortal(t *testing.T) {
	for _, target := range []string{
		"https://fnos.net/vas4er/app/llmproxy",
		"https://fnos.net/app/llmproxy",
		"https://FNOS.NET/vas4er/app/llmproxy",
	} {
		if err := ValidateTarget(target); err == nil || !strings.Contains(err.Error(), "FN Connect portal") {
			t.Errorf("ValidateTarget(%q) = %v, want portal rejection", target, err)
		}
	}
	if err := ValidateTarget("https://vas4er.fnos.net/app/llmproxy"); err != nil {
		t.Fatalf("valid gateway target rejected: %v", err)
	}
}

func TestUnifiedPathGatewayInjectsTokensAndJoinsPath(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/vas4er/app/llmproxy/v1/models" {
			t.Errorf("unexpected path: %q", r.URL.Path)
		}
		cookie := r.Header.Get("Cookie")
		if !strings.Contains(cookie, "fnos-token=fnos123") {
			t.Errorf("missing fnos-token cookie: %q", cookie)
		}
		if !strings.Contains(cookie, "entry-token=entry123") {
			t.Errorf("missing entry-token cookie: %q", cookie)
		}
		if !strings.Contains(cookie, "mode=relay") {
			t.Errorf("missing mode=relay cookie: %q", cookie)
		}
		if !strings.Contains(cookie, "language=zh-CN") {
			t.Errorf("missing language=zh-CN cookie: %q", cookie)
		}
		if r.Header.Get("Authorization") != "Bearer llm-key" {
			t.Errorf("authorization changed: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	cfg := appcfg.Default()
	cfg.TargetURL = upstream.URL + "/vas4er/app/llmproxy"
	cfg.EntryToken = "entry123"
	cfg.EntryTokenExpires = time.Now().Add(6 * time.Hour)
	cfg.FNOSToken = "fnos123"
	cfg.FNOSTokenExpires = time.Now().Add(6 * time.Hour)

	mgr := NewTokenManager(t.TempDir()+"/config.json", cfg)
	h, err := NewServer(mgr, cfg.TargetURL)
	if err != nil {
		t.Fatal(err)
	}
	local := httptest.NewServer(h)
	defer local.Close()

	req, _ := http.NewRequest("GET", local.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer llm-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}
