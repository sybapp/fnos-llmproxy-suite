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
	mu        sync.Mutex
	path      string
	cfg       appcfg.Config
	timeout   time.Duration
	newClient func(string, time.Duration) sessionClient
}

type sessionClient interface {
	Connect(context.Context) error
	Close() error
	RefreshWithFNOSToken(context.Context, string, string, string) (fnos.Session, error)
	RefreshWithLongToken(context.Context, string, string, string, string, string) (fnos.Session, error)
}

const (
	entryRefreshWindow = 2 * time.Hour
	fnosRefreshWindow  = 30 * time.Minute
)

func NewTokenManager(path string, cfg appcfg.Config) *TokenManager {
	return &TokenManager{path: path, cfg: cfg, timeout: 30 * time.Second, newClient: func(server string, timeout time.Duration) sessionClient { return fnos.New(server, timeout) }}
}

func (m *TokenManager) Config() appcfg.Config { m.mu.Lock(); defer m.mu.Unlock(); return m.cfg }
func (m *TokenManager) EntryToken() string    { m.mu.Lock(); defer m.mu.Unlock(); return m.cfg.EntryToken }
func (m *TokenManager) FNOSToken() string     { m.mu.Lock(); defer m.mu.Unlock(); return m.cfg.FNOSToken }
func (m *TokenManager) LongToken() string     { m.mu.Lock(); defer m.mu.Unlock(); return m.cfg.LongToken }

// reloadConfigLocked adopts credentials written by a separate login or refresh
// process while serve is running.
func (m *TokenManager) reloadConfigLocked() (bool, error) {
	cfg, err := appcfg.Load(m.path)
	if err != nil {
		return false, err
	}
	if cfg.UpdatedAt.After(m.cfg.UpdatedAt) {
		m.cfg = cfg
		return true, nil
	}
	return false, nil
}

func isUnified(target string) bool {
	u, err := url.Parse(target)
	if err != nil {
		return false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i, part := range parts {
		if part == "app" && i+1 < len(parts) {
			return true
		}
	}
	return false
}

func (m *TokenManager) Refresh(ctx context.Context, force bool) error {
	_, err := m.snapshot(ctx, force, false)
	return err
}

// RenewSession explicitly rotates the short session, including when the gateway
// rejects a token that websocket authToken still accepts.
func (m *TokenManager) RenewSession(ctx context.Context) error {
	_, err := m.snapshot(ctx, true, true)
	return err
}

func (m *TokenManager) EnsureGatewaySession(ctx context.Context, force bool) error {
	return m.Refresh(ctx, force)
}

func (m *TokenManager) snapshot(ctx context.Context, forceEntry, forceSession bool) (appcfg.Config, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.reloadConfigLocked(); err != nil {
		return m.cfg, err
	}
	now := time.Now()
	entryDue := forceEntry || m.cfg.EntryToken == "" || !m.cfg.EntryTokenExpires.After(now.Add(entryRefreshWindow))
	sessionDue := forceSession || (isUnified(m.cfg.TargetURL) && (m.cfg.FNOSToken == "" || (!m.cfg.FNOSTokenExpires.IsZero() && !m.cfg.FNOSTokenExpires.After(now.Add(fnosRefreshWindow)))))
	if !entryDue && !sessionDue {
		return m.cfg, nil
	}
	if m.cfg.Server == "" {
		return m.cfg, errors.New("not configured: run login first")
	}
	if m.cfg.FNOSToken == "" && m.cfg.LongToken == "" {
		return m.cfg, errors.New("fnOS session unavailable; run login again")
	}
	base := m.cfg
	refresh := func(long bool) (fnos.Session, error) {
		// Every attempt owns a fresh socket, including fallback after transport errors.
		c := m.newClient(base.Server, m.timeout)
		if err := c.Connect(ctx); err != nil {
			return fnos.Session{}, err
		}
		defer c.Close()
		if long {
			return c.RefreshWithLongToken(ctx, base.LongToken, base.SignKey, base.BackID, base.DeviceType, base.DeviceName)
		}
		return c.RefreshWithFNOSToken(ctx, base.FNOSToken, base.SignKey, base.BackID)
	}
	usedLong := (sessionDue || base.FNOSToken == "") && base.LongToken != ""
	if forceSession && base.LongToken == "" {
		return m.cfg, errors.New("longToken unavailable for session renewal; run login again")
	}
	sess, err := refresh(usedLong)
	if err != nil && !usedLong && base.LongToken != "" && ctx.Err() == nil {
		usedLong = true
		sess, err = refresh(true)
	}
	if err != nil {
		return m.cfg, fmt.Errorf("FN Connect session refresh failed: %w; run login again if the session remains unavailable", err)
	}
	m.applySessionRefresh(sess, usedLong)
	next, err := appcfg.SaveIfUnchanged(ctx, m.path, base.UpdatedAt, m.cfg)
	if errors.Is(err, appcfg.ErrChanged) {
		// A login/logout/refresh committed while the network call was in flight.
		m.cfg = base
		_, loadErr := m.reloadConfigLocked()
		if loadErr != nil {
			return m.cfg, loadErr
		}
		if m.cfg.EntryToken == "" || (isUnified(m.cfg.TargetURL) && m.cfg.FNOSToken == "") {
			return m.cfg, errors.New("session cleared during refresh; run login again")
		}
		return m.cfg, nil
	}
	if err != nil {
		m.cfg = base
		return m.cfg, err
	}
	m.cfg = next
	return m.cfg, nil
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

func (m *TokenManager) applySessionRefresh(s fnos.Session, usedLongToken bool) {
	previousLongToken := m.cfg.LongToken
	m.applyEntryRefresh(s)
	if !usedLongToken {
		return
	}
	if s.FNOSToken != "" {
		m.cfg.FNOSToken = s.FNOSToken
		m.cfg.FNOSTokenExpires = time.Now().Add(8 * time.Hour)
	}
	if s.LongToken != "" {
		m.cfg.LongToken = s.LongToken
		if s.LongToken != previousLongToken {
			m.cfg.LongTokenExpires = time.Now().Add(25 * 24 * time.Hour)
		}
	}
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
			// Poll every 30 minutes, but only open a control connection when a
			// token is actually inside its refresh window.
			err := m.Refresh(cctx, false)
			cancel()
			if err != nil {
				log.Printf("automatic token refresh: %v", err)
			}
		}
	}
}

type Server struct {
	mgr    *TokenManager
	client *http.Client
}

func parseTarget(target string) (*url.URL, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil, errors.New("target must use http or https with a host and no credentials")
	}
	// The FN Connect portal serves its SPA for every fnos.net path, so
	// https://fnos.net/<FN_ID>/app/... returns HTML instead of reaching
	// the NAS gateway. Only <FN_ID>.fnos.net routes to an app.
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "fnos.net" {
		return nil, errors.New("fnos.net is the FN Connect portal, not an app gateway; use https://<FN_ID>.fnos.net/app/<app>")
	}
	return u, nil
}

// ValidateTarget checks a configured app target without keeping the parsed URL.
func ValidateTarget(target string) error {
	_, err := parseTarget(target)
	return err
}

func NewServer(mgr *TokenManager, target string) (*Server, error) {
	if err := ValidateTarget(target); err != nil {
		return nil, err
	}
	mgr.mu.Lock()
	if mgr.cfg.TargetURL == "" {
		mgr.cfg.TargetURL = target
	}
	mgr.mu.Unlock()
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ForceAttemptHTTP2 = true
	tr.MaxIdleConns = 100
	tr.MaxIdleConnsPerHost = 20
	tr.IdleConnTimeout = 90 * time.Second
	cl := &http.Client{Transport: tr, CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
	return &Server{mgr: mgr, client: cl}, nil
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

func (s *Server) do(req *http.Request, body []byte, cfg appcfg.Config) (*http.Response, error) {
	target, err := parseTarget(cfg.TargetURL)
	if err != nil {
		return nil, err
	}
	out := req.Clone(req.Context())
	out.URL = &url.URL{}
	*out.URL = *target
	out.URL.Path = joinPath(target.Path, req.URL.Path)
	out.URL.RawQuery = req.URL.RawQuery
	out.Host = target.Host
	out.RequestURI = ""
	out.Header = req.Header.Clone()
	removeHop(out.Header)
	replaceAuthCookies(out.Header, cfg.EntryToken, cfg.FNOSToken, cfg.LongToken, isUnified(cfg.TargetURL))
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
		if _, err := w.Write(buffered); err != nil {
			return err
		}
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
	// Route and credentials always come from the same locked snapshot.
	// Bound authentication recovery to an entry refresh and a session renewal.
	for attempt := 0; attempt < 3; attempt++ {
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		cfg, err := s.mgr.snapshot(ctx, attempt > 0, attempt == 2)
		cancel()
		if err != nil {
			http.Error(w, "FN Connect auth unavailable: "+err.Error(), 502)
			return
		}
		resp, err := s.do(r, body, cfg)
		if err != nil {
			http.Error(w, "proxy request failed: "+err.Error(), 502)
			return
		}
		bad, buf, err := looksLikeFNAuth(resp, isUnified(cfg.TargetURL))
		if err != nil {
			_ = resp.Body.Close()
			http.Error(w, err.Error(), 502)
			return
		}
		if bad {
			_ = resp.Body.Close()
			if attempt < 2 {
				continue
			}
			http.Error(w, "FN Connect rejected the renewed session; run fnconnect-proxy login again", 502)
			return
		}
		if err := writeResponse(w, resp, buf); err != nil {
			log.Printf("response stream: %v", err)
		}
		return
	}
}
