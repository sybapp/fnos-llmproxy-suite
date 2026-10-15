package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStripGatewayPrefix(t *testing.T) {
	tests := map[string]string{
		"/app/llmproxy":           "/",
		"/app/llmproxy/":          "/",
		"/app/llmproxy/v1/models": "/v1/models",
		"/v1/models":              "/v1/models",
		"/app/llmproxy2/v1":       "/app/llmproxy2/v1",
	}
	for in, want := range tests {
		if got := stripGatewayPrefix(in, "/app/llmproxy"); got != want {
			t.Fatalf("stripGatewayPrefix(%q)=%q want %q", in, got, want)
		}
	}
}

func TestGatewayPrefixAndCookieStripping(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("upstream path=%q want /v1/models", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization=%q", got)
		}
		for _, secretCookie := range []string{"entry-token", "fnos-token", "fnos-long-token"} {
			if strings.Contains(r.Header.Get("Cookie"), secretCookie) {
				t.Errorf("%s leaked upstream: %q", secretCookie, r.Header.Get("Cookie"))
			}
		}
		if !strings.Contains(r.Header.Get("Cookie"), "keep=yes") {
			t.Errorf("non-sensitive cookie not preserved: %q", r.Header.Get("Cookie"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	proxy := httptest.NewServer(newReverseProxy(target, "/app/llmproxy"))
	defer proxy.Close()

	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/app/llmproxy/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Cookie", "entry-token=secret; fnos-token=session; fnos-long-token=never; keep=yes")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func TestUnixSocketHTTPAndSSE(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("path=%q", r.URL.Path)
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("response writer is not a flusher")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 3; i++ {
			_, _ = fmt.Fprintf(w, "data: %d\n\n", i)
			flusher.Flush()
			time.Sleep(40 * time.Millisecond)
		}
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	handler := newReverseProxy(target, "/app/llmproxy")

	sock := filepath.Join(t.TempDir(), "app.sock")
	ln, err := listenUnixSocket(sock)
	if err != nil {
		t.Fatal(err)
	}
	server := newHTTPServer("", handler)
	go func() { _ = server.Serve(ln) }()
	defer server.Close()

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = io.WriteString(conn, "GET /app/llmproxy/v1/responses HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "data: 0") || !strings.Contains(string(body), "data: 2") {
		t.Fatalf("unexpected SSE body: %q", string(body))
	}
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("socket missing: %v", err)
	}
}
