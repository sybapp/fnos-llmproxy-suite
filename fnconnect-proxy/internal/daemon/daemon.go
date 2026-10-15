// Package daemon manages a detached proxy through a private authenticated
// loopback endpoint. It never sends signals to a PID read from disk.
package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	appcfg "fnconnect-proxy/internal/config"
	"fnconnect-proxy/internal/filelock"
)

const TokenEnv = "FNCONNECT_PROXY_MANAGED_TOKEN"

// State is private to the user; Token authorizes control of this exact instance.
type State struct {
	PID     int       `json:"pid"`
	Address string    `json:"control_address"`
	Token   string    `json:"control_token"`
	Listen  string    `json:"listen"`
	Started time.Time `json:"started_at"`
}

type Manager struct{ ConfigPath, Dir string }

func New(configPath string) (*Manager, error) {
	path, err := filepath.Abs(configPath)
	if err != nil {
		return nil, err
	}
	dir := strings.TrimSpace(os.Getenv("FNCONNECT_PROXY_RUN_DIR"))
	if dir == "" {
		dir = filepath.Join(filepath.Dir(path), "run")
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	return &Manager{ConfigPath: path, Dir: dir}, nil
}

func (m *Manager) LogPath() string   { return filepath.Join(m.Dir, "server.log") }
func (m *Manager) statePath() string { return filepath.Join(m.Dir, "server.json") }

func (m *Manager) readState() (State, error) {
	var state State
	b, err := os.ReadFile(m.statePath())
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(b, &state); err != nil {
		return state, fmt.Errorf("invalid process state: %w", err)
	}
	host, port, err := net.SplitHostPort(state.Address)
	if err != nil || host != "127.0.0.1" || port == "" || state.PID <= 0 || len(state.Token) != 64 {
		return state, errors.New("invalid process state")
	}
	return state, nil
}

func (m *Manager) writeState(state State) error {
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(m.Dir, ".server-state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(append(b, '\n')); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), m.statePath())
}

func (m *Manager) removeState(token string) {
	if state, err := m.readState(); err == nil && state.Token == token {
		_ = os.Remove(m.statePath())
	}
}

func controlRequest(ctx context.Context, state State, method, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://"+state.Address+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+state.Token)
	// Local control must not pass through a configured HTTP proxy or redirects.
	client := &http.Client{Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}, Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return client.Do(req)
}

// instanceRunning distinguishes stale state from an unresponsive live process.
func (m *Manager) instanceRunning(ctx context.Context) (bool, error) {
	if _, err := os.Stat(m.Dir); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	unlock, err := filelock.Acquire(probeCtx, filepath.Join(m.Dir, "instance.lock"))
	if err == nil {
		unlock()
		return false, nil
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true, nil
	}
	return false, err
}

func (m *Manager) Status(ctx context.Context) (State, bool, error) {
	state, err := m.readState()
	if err == nil {
		resp, requestErr := controlRequest(ctx, state, http.MethodGet, "/status")
		if requestErr == nil {
			defer resp.Body.Close()
			var live State
			if resp.StatusCode == 200 && json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&live) == nil && live.PID == state.PID && live.Listen == state.Listen {
				return state, true, nil
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return state, false, err
	}
	running, probeErr := m.instanceRunning(ctx)
	if probeErr != nil {
		return state, false, probeErr
	}
	if running {
		return state, false, errors.New("proxy is starting or its control endpoint is unavailable; try again shortly")
	}
	return State{}, false, nil
}

func (m *Manager) commandLock(ctx context.Context) (func(), error) {
	if err := os.MkdirAll(m.Dir, 0700); err != nil {
		return nil, err
	}
	return filelock.Acquire(ctx, filepath.Join(m.Dir, "command.lock"))
}

func (m *Manager) Start(ctx context.Context, listen string) (State, error) {
	unlock, err := m.commandLock(ctx)
	if err != nil {
		return State{}, err
	}
	defer unlock()
	return m.start(ctx, listen)
}

func (m *Manager) start(ctx context.Context, listen string) (State, error) {
	if state, running, err := m.Status(ctx); err != nil {
		return State{}, err
	} else if running {
		return state, nil
	}
	cfg, err := appcfg.Load(m.ConfigPath)
	if err != nil {
		return State{}, err
	}
	if cfg.TargetURL == "" {
		return State{}, errors.New("target missing; run login first")
	}
	executable, err := os.Executable()
	if err != nil {
		return State{}, err
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return State{}, err
	}
	token := hex.EncodeToString(tokenBytes)
	args := []string{"serve"}
	if listen != "" {
		args = append(args, "--listen", listen)
	}
	cmd := exec.Command(executable, args...)
	cmd.Env = managedEnv(m.ConfigPath, m.Dir, token)
	detach(cmd)
	logFile, err := os.OpenFile(m.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return State{}, err
	}
	defer logFile.Close()
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		return State{}, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	started, exited := false, false
	defer func() {
		if !started {
			if !exited {
				_ = cmd.Process.Kill()
				<-done
			}
			m.removeState(token)
		}
	}()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return State{}, fmt.Errorf("proxy startup did not complete: %w; see %s", ctx.Err(), m.LogPath())
		case err := <-done:
			exited = true
			return State{}, fmt.Errorf("proxy exited during startup (%v); see %s", err, m.LogPath())
		case <-ticker.C:
			state, err := m.readState()
			if err != nil || state.Token != token || state.PID != cmd.Process.Pid {
				continue
			}
			if _, running, err := m.Status(ctx); err == nil && running {
				started = true
				return state, nil
			}
		}
	}
}

func managedEnv(configPath, dir, token string) []string {
	var env []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.EqualFold(name, "FNCONNECT_PROXY_CONFIG") || strings.EqualFold(name, "FNCONNECT_PROXY_RUN_DIR") || strings.EqualFold(name, TokenEnv) {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "FNCONNECT_PROXY_CONFIG="+configPath, "FNCONNECT_PROXY_RUN_DIR="+dir, TokenEnv+"="+token)
}

func (m *Manager) Stop(ctx context.Context) error {
	unlock, err := m.commandLock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	return m.stop(ctx)
}

func (m *Manager) stop(ctx context.Context) error {
	state, running, err := m.Status(ctx)
	if err != nil {
		return err
	}
	if !running {
		return nil
	}
	resp, err := controlRequest(ctx, state, http.MethodPost, "/stop")
	if err != nil {
		return fmt.Errorf("stop request failed: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("stop rejected: HTTP %d", resp.StatusCode)
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		live, err := m.instanceRunning(ctx)
		if err != nil {
			return err
		}
		if !live {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("proxy is still stopping: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (m *Manager) Restart(ctx context.Context, listen string) (State, error) {
	unlock, err := m.commandLock(ctx)
	if err != nil {
		return State{}, err
	}
	defer unlock()
	if listen == "" {
		state, running, err := m.Status(ctx)
		if err != nil {
			return State{}, err
		}
		if running {
			listen = state.Listen
		}
	}
	if err := m.stop(ctx); err != nil {
		return State{}, err
	}
	return m.start(ctx, listen)
}

// Control owns the instance lock until the proxy has finished shutting down.
type Control struct {
	manager *Manager
	state   State
	server  *http.Server
	unlock  func()
}

func (m *Manager) OpenControl(ctx context.Context, token, listen string, stop func()) (*Control, error) {
	if len(token) != 64 {
		return nil, errors.New("managed serve must be launched with the start command")
	}
	if err := os.MkdirAll(m.Dir, 0700); err != nil {
		return nil, err
	}
	lockCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	unlock, err := filelock.Acquire(lockCtx, filepath.Join(m.Dir, "instance.lock"))
	if err != nil {
		return nil, fmt.Errorf("another proxy instance is still running: %w", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		unlock()
		return nil, err
	}
	state := State{PID: os.Getpid(), Address: listener.Addr().String(), Token: token, Listen: listen, Started: time.Now().UTC()}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			http.Error(w, "unauthorized", 401)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/status":
			public := state
			public.Token = ""
			public.Address = ""
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(public)
		case r.Method == http.MethodPost && r.URL.Path == "/stop":
			w.WriteHeader(http.StatusAccepted)
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			stop()
		default:
			http.NotFound(w, r)
		}
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second, IdleTimeout: 2 * time.Second, MaxHeaderBytes: 8192}
	go func() { _ = server.Serve(listener) }()
	if err := m.writeState(state); err != nil {
		_ = server.Close()
		_ = listener.Close()
		unlock()
		return nil, err
	}
	return &Control{manager: m, state: state, server: server, unlock: unlock}, nil
}

func (c *Control) Close() {
	_ = c.server.Close()
	c.manager.removeState(c.state.Token)
	c.unlock()
}

func (m *Manager) Logs(w io.Writer, lines int) error {
	if lines < 1 || lines > 10000 {
		return errors.New("lines must be between 1 and 10000")
	}
	f, err := os.Open(m.LogPath())
	if errors.Is(err, os.ErrNotExist) {
		_, err = fmt.Fprintln(w, "No log yet.")
		return err
	}
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	const limit int64 = 1 << 20
	offset := info.Size() - limit
	if offset < 0 {
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	b, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return err
	}
	if offset > 0 {
		if index := bytes.IndexByte(b, '\n'); index >= 0 {
			b = b[index+1:]
		} else {
			b = nil
		}
	}
	rows := bytes.Split(bytes.TrimSuffix(b, []byte("\n")), []byte("\n"))
	if len(rows) > lines {
		rows = rows[len(rows)-lines:]
	}
	if len(b) > 0 {
		_, err = w.Write(append(bytes.Join(rows, []byte("\n")), '\n'))
	}
	return err
}
