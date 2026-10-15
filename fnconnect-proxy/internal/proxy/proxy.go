package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	appcfg "fnconnect-proxy/internal/config"
	"fnconnect-proxy/internal/fnos"
)

type TokenManager struct {
	mu      sync.Mutex
	path    string
	cfg     appcfg.Config
	timeout time.Duration
}

func NewTokenManager(path string, cfg appcfg.Config) *TokenManager {
	return &TokenManager{path: path, cfg: cfg, timeout: 30 * time.Second}
}

func (m *TokenManager) Config() appcfg.Config { m.mu.Lock(); defer m.mu.Unlock(); return m.cfg }
func (m *TokenManager) EntryToken() string    { m.mu.Lock(); defer m.mu.Unlock(); return m.cfg.EntryToken }
func (m *TokenManager) FNOSToken() string     { m.mu.Lock(); defer m.mu.Unlock(); return m.cfg.FNOSToken }
func (m *TokenManager) LongToken() string     { m.mu.Lock(); defer m.mu.Unlock(); return m.cfg.LongToken }

func (m *TokenManager) Refresh(ctx context.Context, force bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if !force && m.cfg.EntryToken != "" && m.cfg.EntryTokenExpires.After(now.Add(2*time.Hour)) {
		return nil
	}
	if m.cfg.Server == "" {
		return errors.New("not configured: run login first")
	}
	if m.cfg.FNOSToken == "" || m.cfg.SignKey == "" {
		return errors.New("fnos-token session unavailable; run login again")
	}
	if !m.cfg.FNOSTokenExpires.After(now.Add(5 * time.Minute)) {
		return errors.New("fnos-token session expired or near expiry; run login again")
	}

	c := fnos.New(m.cfg.Server, m.timeout)
	if err := c.Connect(ctx); err != nil {
		return err
	}
	sess, err := c.RefreshWithFNOSToken(ctx, m.cfg.FNOSToken, m.cfg.SignKey, m.cfg.BackID)
	_ = c.Close()
	if err != nil {
		return fmt.Errorf("entry-token refresh with current fnos-token failed; keeping current fnos-token unchanged: %w; run login again if the gateway rejects the session", err)
	}
	m.applyEntryRefresh(sess)
	return appcfg.Save(m.path, m.cfg)
}

func (m *TokenManager) EnsureGatewaySession(ctx context.Context, forceEntryRefresh bool) error {
	m.mu.Lock()
	now := time.Now()
	if m.cfg.FNOSToken == "" || m.cfg.EntryToken == "" {
		m.mu.Unlock()
		return errors.New("fnOS gateway session unavailable; run login again")
	}
	if !m.cfg.FNOSTokenExpires.After(now.Add(5 * time.Minute)) {
		m.mu.Unlock()
		return errors.New("fnos-token session expired or near expiry; run login again")
	}
	needEntryRefresh := forceEntryRefresh || !m.cfg.EntryTokenExpires.After(now.Add(2*time.Hour))
	if !needEntryRefresh {
		m.mu.Unlock()
		return nil
	}
	if m.cfg.Server == "" {
		m.mu.Unlock()
		return errors.New("not configured: run login first")
	}
	m.mu.Unlock()
	return m.Refresh(ctx, true)
}

func (m *TokenManager) applyEntryRefresh(s fnos.Session) {
	now := time.Now()
	if s.EntryToken != "" {
		m.cfg.EntryToken = s.EntryToken
		m.cfg.EntryTokenExpires = now.Add(8 * time.Hour)
	}
	if s.SignKey != "" {
		m.cfg.SignKey = s.SignKey
	}
	if s.BackID != "" {
		m.cfg.BackID = s.BackID
	}
	if s.UID != 0 {
		m.cfg.UID = s.UID
	}
	m.cfg.Admin = s.Admin
}

func (m *TokenManager) AutoRefresh(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cctx, cancel := context.WithTimeout(ctx, 45*time.Second)
			err := m.Refresh(cctx, false)
			cancel()
			if err != nil {
				log.Printf("automatic entry-token refresh: %v", err)
			}
		}
	}
}

type Server struct {
	mgr            *TokenManager
	target         *url.URL
	client         *http.Client
	unifiedGateway bool
}

func NewServer(mgr *TokenManager, target string) (*Server, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("target must use http or https")
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ForceAttemptHTTP2 = true
	tr.MaxIdleConns = 100
	tr.MaxIdleConnsPerHost = 20
	tr.IdleConnTimeout = 90 * time.Second
	cl := &http.Client{Transport: tr, CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }, Timeout: 0}
	return &Server{mgr: mgr, target: u, client: cl, unifiedGateway: strings.HasPrefix(strings.TrimRight(u.Path, "/")+"/", "/app/")}, nil
}

func joinPath(a, b string) string {
	if a == "" || a == "/" {
		if b == "" {
			return "/"
		}
		return b
	}
	return strings.TrimRight(a, "/") + "/" + strings.TrimLeft(b, "/")
}

func copyHeader(dst, src http.Header) {
	for k, v := range src {
		for _, x := range v {
			dst.Add(k, x)
		}
	}
}
func removeHop(h http.Header) {
	for _, k := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		h.Del(k)
	}
}

func replaceAuthCookies(h http.Header, entryToken, fnosToken, longToken string, unifiedGateway bool) {
	cookies := h.Values("Cookie")
	h.Del("Cookie")
	var kept []string
	for _, line := range cookies {
		for _, p := range strings.Split(line, ";") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			lower := strings.ToLower(p)
			if strings.HasPrefix(lower, "entry-token=") ||
				strings.HasPrefix(lower, "fnos-token=") ||
				strings.HasPrefix(lower, "fnos-long-token=") ||
				strings.HasPrefix(lower, "mode=") ||
				strings.HasPrefix(lower, "language=") {
				continue
			}
			kept = append(kept, p)
		}
	}

	kept = append(kept, "mode=relay", "language=zh-CN")
	if unifiedGateway && fnosToken != "" {
		kept = append(kept, "fnos-token="+fnosToken)
	}
	if entryToken != "" {
		kept = append(kept, "entry-token="+entryToken)
	}
	if len(kept) > 0 {
		h.Set("Cookie", strings.Join(kept, "; "))
	}

	_ = longToken
}

func (s *Server) do(req *http.Request, body []byte, entryToken, fnosToken, longToken string) (*http.Response, error) {
	out := req.Clone(req.Context())
	out.URL = &url.URL{}
	*out.URL = *s.target
	out.URL.Path = joinPath(s.target.Path, req.URL.Path)
	out.URL.RawQuery = req.URL.RawQuery
	out.Host = s.target.Host
	out.RequestURI = ""
	out.Header = req.Header.Clone()
	removeHop(out.Header)
	replaceAuthCookies(out.Header, entryToken, fnosToken, longToken, s.unifiedGateway)
	if body != nil {
		out.Body = io.NopCloser(bytes.NewReader(body))
		out.ContentLength = int64(len(body))
		out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	}
	out.Header.Set("X-Fnconnect-Proxy", "1")
	return s.client.Do(out)
}

func looksLikeFNAuth(resp *http.Response, unifiedGateway bool) (bool, []byte, error) {
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	loc := strings.ToLower(resp.Header.Get("Location"))
	if (resp.StatusCode == 302 || resp.StatusCode == 303) && (strings.Contains(loc, "fnos") || strings.Contains(loc, "login")) {
		return true, nil, nil
	}

	if unifiedGateway && strings.Contains(ct, "text/plain") && (resp.ContentLength < 0 || resp.ContentLength <= 512) {
		b, err := io.ReadAll(io.LimitReader(resp.Body, 513))
		if err != nil {
			return false, nil, err
		}
		if strings.EqualFold(strings.TrimSpace(string(b)), "invalid token") {
			return true, b, nil
		}
		return false, b, nil
	}

	if !strings.Contains(ct, "text/html") {
		return false, nil, nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return false, nil, err
	}
	text := string(b)
	bad := strings.Contains(text, "FN Connect") && (strings.Contains(text, "暂无权限访问该服务") || strings.Contains(text, "访问提示"))
	if (resp.StatusCode == 401 || resp.StatusCode == 403) && strings.Contains(text, "FN Connect") {
		bad = true
	}
	return bad, b, nil
}

func writeResponse(w http.ResponseWriter, resp *http.Response, buffered []byte) error {
	defer resp.Body.Close()
	removeHop(resp.Header)
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if buffered != nil {
		_, err := w.Write(buffered)
		return err
	}
	buf := make([]byte, 32*1024)
	fl, _ := w.(http.Flusher)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, e := w.Write(buf[:n]); e != nil {
				return e
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/__fnconnect_proxy/health" {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, "{\"ok\":true}\n")
		return
	}
	var authErr error
	if s.unifiedGateway {
		authErr = s.mgr.EnsureGatewaySession(r.Context(), false)
	} else {
		authErr = s.mgr.Refresh(r.Context(), false)
	}
	if authErr != nil {
		http.Error(w, "FN Connect auth unavailable: "+authErr.Error(), http.StatusBadGateway)
		return
	}
	var body []byte
	if r.Body != nil {
		var err error
		body, err = io.ReadAll(io.LimitReader(r.Body, 64<<20))
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		_ = r.Body.Close()
	}
	resp, err := s.do(r, body, s.mgr.EntryToken(), s.mgr.FNOSToken(), s.mgr.LongToken())
	if err != nil {
		http.Error(w, "proxy request failed: "+err.Error(), 502)
		return
	}
	bad, buf, err := looksLikeFNAuth(resp, s.unifiedGateway)
	if err != nil {
		_ = resp.Body.Close()
		http.Error(w, err.Error(), 502)
		return
	}
	if bad {
		_ = resp.Body.Close()
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		if s.unifiedGateway {
			err = s.mgr.EnsureGatewaySession(ctx, true)
		} else {
			err = s.mgr.Refresh(ctx, true)
		}
		cancel()
		if err != nil {
			http.Error(w, "FN Connect session refresh failed without rotating fnos-token: "+err.Error(), 502)
			return
		}
		resp, err = s.do(r, body, s.mgr.EntryToken(), s.mgr.FNOSToken(), s.mgr.LongToken())
		if err != nil {
			http.Error(w, "proxy retry failed: "+err.Error(), 502)
			return
		}
		bad, buf, err = looksLikeFNAuth(resp, s.unifiedGateway)
		if err != nil {
			_ = resp.Body.Close()
			http.Error(w, err.Error(), 502)
			return
		}
		if bad {
			_ = resp.Body.Close()
			http.Error(w, "FN Connect still rejected the session after refreshing entry-token; run fnconnect-proxy login again", 502)
			return
		}
	}
	if err := writeResponse(w, resp, buf); err != nil {
		log.Printf("response stream: %v", err)
	}
}
