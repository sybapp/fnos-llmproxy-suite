package main

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

var (
	version = "dev"
	commit  = "unknown"
)

const (
	defaultListenPort    = "13430"
	defaultUpstreamURL   = "http://127.0.0.1:3430"
	defaultGatewayPrefix = "/app/llmproxy"
	defaultGatewaySocket = "/var/apps/llmproxy/target/app.sock"
)

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds | log.LUTC)

	listenPort := strings.TrimSpace(os.Getenv("TRIM_SERVICE_PORT"))
	if listenPort == "" {
		listenPort = defaultListenPort
	}

	rawUpstream := strings.TrimSpace(os.Getenv("UPSTREAM_URL"))
	if rawUpstream == "" {
		rawUpstream = defaultUpstreamURL
	}

	gatewayPrefix := normalizePrefix(os.Getenv("GATEWAY_PREFIX"))
	if gatewayPrefix == "" {
		gatewayPrefix = defaultGatewayPrefix
	}

	gatewaySocket := strings.TrimSpace(os.Getenv("GATEWAY_SOCKET"))
	if gatewaySocket == "" {
		gatewaySocket = defaultGatewaySocket
	}

	target, err := parseUpstreamURL(rawUpstream)
	if err != nil {
		log.Fatalf("invalid UPSTREAM_URL %q: %v", rawUpstream, err)
	}

	proxy := newReverseProxy(target, gatewayPrefix)

	tcpServer := newHTTPServer(":"+listenPort, proxy)
	unixServer := newHTTPServer("", proxy)

	unixListener, err := listenUnixSocket(gatewaySocket)
	if err != nil {
		log.Fatalf("gateway unix socket %q: %v", gatewaySocket, err)
	}
	defer func() {
		_ = unixListener.Close()
		_ = os.Remove(gatewaySocket)
	}()

	errCh := make(chan error, 2)

	go func() {
		log.Printf("fnOS LLM Proxy %s (%s) TCP :%s -> %s", version, commit, listenPort, target.Redacted())
		err := tcpServer.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("tcp http server: %w", err)
			return
		}
		errCh <- nil
	}()

	go func() {
		log.Printf("fnOS unified gateway socket %s prefix=%s", gatewaySocket, gatewayPrefix)
		err := unixServer.Serve(unixListener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("unix http server: %w", err)
			return
		}
		errCh <- nil
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	select {
	case sig := <-sigCh:
		log.Printf("received %s, shutting down", sig)
	case serveErr := <-errCh:
		if serveErr != nil {
			log.Printf("server failed: %v", serveErr)
		}
	}

	_ = tcpServer.Close()
	_ = unixServer.Close()
	_ = unixListener.Close()
	_ = os.Remove(gatewaySocket)
	log.Printf("shutdown complete")
}

func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       5 * time.Minute,
	}
}

func listenUnixSocket(path string) (net.Listener, error) {
	if path == "" {
		return nil, fmt.Errorf("socket path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if st, err := os.Lstat(path); err == nil {
		if st.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("refusing to replace non-socket path")
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o660); err != nil {
		_ = ln.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return ln, nil
}

func parseUpstreamURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("scheme must be http or https")
	}
	if u.Host == "" {
		return nil, fmt.Errorf("host is required")
	}
	if u.User != nil {
		return nil, fmt.Errorf("userinfo in upstream URL is not supported")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("query and fragment are not allowed in base upstream URL")
	}
	return u, nil
}

func normalizePrefix(raw string) string {
	p := strings.TrimSpace(raw)
	if p == "" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return strings.TrimRight(p, "/")
}

func stripGatewayPrefix(path, prefix string) string {
	prefix = normalizePrefix(prefix)
	if prefix == "" {
		return path
	}
	if path == prefix {
		return "/"
	}
	if strings.HasPrefix(path, prefix+"/") {
		stripped := strings.TrimPrefix(path, prefix)
		if stripped == "" {
			return "/"
		}
		return stripped
	}
	return path
}

func newReverseProxy(target *url.URL, gatewayPrefix string) *httputil.ReverseProxy {
	proxy := httputil.NewSingleHostReverseProxy(target)
	originalDirector := proxy.Director

	proxy.Director = func(req *http.Request) {
		req.URL.Path = stripGatewayPrefix(req.URL.Path, gatewayPrefix)
		if req.URL.RawPath != "" {
			req.URL.RawPath = stripGatewayPrefix(req.URL.RawPath, gatewayPrefix)
		}

		originalDirector(req)
		req.Host = target.Host

		stripCookie(req, "entry-token")
		stripCookie(req, "fnos-token")
		stripCookie(req, "fnos-long-token")
	}

	proxy.Transport = &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}

	proxy.FlushInterval = -1

	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("proxy error method=%s path=%s upstream=%s: %v", r.Method, r.URL.RequestURI(), target.Redacted(), err)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream unavailable","type":"proxy_error"}}`))
	}

	return proxy
}

func stripCookie(req *http.Request, name string) {
	cookies := req.Cookies()
	if len(cookies) == 0 {
		return
	}

	req.Header.Del("Cookie")
	for _, c := range cookies {
		if c.Name == name {
			continue
		}
		req.AddCookie(c)
	}
}
