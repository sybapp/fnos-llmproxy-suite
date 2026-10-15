package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	appcfg "fnconnect-proxy/internal/config"
	"fnconnect-proxy/internal/fnos"
)

func TestProxyInjectsEntryTokenAndStreams(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Cookie"), "entry-token=entry123") {
			t.Errorf("missing entry-token cookie: %q", r.Header.Get("Cookie"))
		}
		if r.Header.Get("Authorization") != "Bearer llm-key" {
			t.Errorf("authorization changed: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "data: %d\n\n", i)
			fl.Flush()
			time.Sleep(80 * time.Millisecond)
		}
	}))
	defer upstream.Close()

	cfg := appcfg.Default()
	cfg.TargetURL = upstream.URL
	cfg.EntryToken = "entry123"
	cfg.EntryTokenExpires = time.Now().Add(6 * time.Hour)
	mgr := NewTokenManager(t.TempDir()+"/config.json", cfg)
	h, err := NewServer(mgr, upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	local := httptest.NewServer(h)
	defer local.Close()

	req, _ := http.NewRequestWithContext(context.Background(), "GET", local.URL+"/v1/stream", nil)
	req.Header.Set("Authorization", "Bearer llm-key")
	started := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	var arrivals []time.Duration
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data:") {
			arrivals = append(arrivals, time.Since(started))
		}
	}
	if len(arrivals) != 3 {
		t.Fatalf("got %d events", len(arrivals))
	}
	if arrivals[1]-arrivals[0] < 50*time.Millisecond {
		t.Fatalf("response appears buffered: %v", arrivals)
	}
}

func TestUnifiedGatewayInjectsFNOSToken(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie := r.Header.Get("Cookie")
		if !strings.Contains(cookie, "fnos-token=fnos123") {
			t.Errorf("missing fnos-token cookie: %q", cookie)
		}
		if !strings.Contains(cookie, "entry-token=entry123") {
			t.Errorf("missing entry-token cookie: %q", cookie)
		}
		if strings.Contains(cookie, "fnos-long-token=") {
			t.Errorf("long-token must not be forwarded: %q", cookie)
		}
		if !strings.Contains(cookie, "mode=relay") {
			t.Errorf("missing mode=relay cookie: %q", cookie)
		}
		if !strings.Contains(cookie, "language=zh-CN") {
			t.Errorf("missing language cookie: %q", cookie)
		}
		if r.URL.Path != "/app/llmproxy/v1/models" {
			t.Errorf("unexpected path: %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	cfg := appcfg.Default()
	cfg.TargetURL = upstream.URL + "/app/llmproxy"
	cfg.EntryToken = "entry123"
	cfg.EntryTokenExpires = time.Now().Add(6 * time.Hour)
	cfg.FNOSToken = "fnos123"
	cfg.FNOSTokenExpires = time.Now().Add(6 * time.Hour)
	cfg.LongToken = "long123"
	cfg.LongTokenExpires = time.Now().Add(20 * 24 * time.Hour)
	mgr := NewTokenManager(t.TempDir()+"/config.json", cfg)
	h, err := NewServer(mgr, cfg.TargetURL)
	if err != nil {
		t.Fatal(err)
	}
	local := httptest.NewServer(h)
	defer local.Close()

	req, _ := http.NewRequest("GET", local.URL+"/v1/models", nil)
	req.Header.Set("Cookie", "fnos-token=old; fnos-long-token=NEVER; keep=yes")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func TestApplyEntryRefreshDoesNotRotateFNOSSession(t *testing.T) {
	cfg := appcfg.Default()
	cfg.FNOSToken = "known-good-fnos"
	cfg.LongToken = "known-good-long"
	cfg.EntryToken = "old-entry"
	cfg.FNOSTokenExpires = time.Now().Add(6 * time.Hour)
	mgr := NewTokenManager(t.TempDir()+"/config.json", cfg)

	mgr.mu.Lock()
	mgr.applyEntryRefresh(fnos.Session{
		FNOSToken:  "SHOULD-NOT-REPLACE",
		LongToken:  "SHOULD-NOT-REPLACE",
		EntryToken: "new-entry",
		SignKey:    "new-sign",
	})
	got := mgr.cfg
	mgr.mu.Unlock()

	if got.FNOSToken != "known-good-fnos" {
		t.Fatalf("fnos-token rotated: %q", got.FNOSToken)
	}
	if got.LongToken != "known-good-long" {
		t.Fatalf("long-token rotated: %q", got.LongToken)
	}
	if got.EntryToken != "new-entry" {
		t.Fatalf("entry-token not refreshed: %q", got.EntryToken)
	}
}

func TestUnifiedGatewayInvalidTokenDetected(t *testing.T) {
	resp := &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
		Body:          io.NopCloser(strings.NewReader("invalid token")),
		ContentLength: 13,
	}
	bad, body, err := looksLikeFNAuth(resp, true)
	if err != nil {
		t.Fatal(err)
	}
	if !bad {
		t.Fatal("expected unified-gateway invalid token to be detected")
	}
	if strings.TrimSpace(string(body)) != "invalid token" {
		t.Fatalf("unexpected buffered body: %q", string(body))
	}
}

func TestUnifiedGatewayPreservesLLMAuthorization(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer llm-key" {
			t.Errorf("authorization changed: %q", r.Header.Get("Authorization"))
		}
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	cfg := appcfg.Default()
	cfg.TargetURL = upstream.URL + "/app/llmproxy"
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
