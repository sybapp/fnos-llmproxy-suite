package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	appcfg "fnconnect-proxy/internal/config"
	"fnconnect-proxy/internal/daemon"
	"fnconnect-proxy/internal/fnos"
	pxy "fnconnect-proxy/internal/proxy"

	"golang.org/x/term"
)

const version = "1.0.9"

func usage() {
	fmt.Fprintf(os.Stderr, `fnconnect-proxy %s

Commands:
  login    Login to fnOS, save longToken/fnos-token/entry-token (password is not saved)
  serve    Run the proxy in the foreground
  start    Start the proxy in the background
  stop     Stop the background proxy
  restart  Restart the background proxy
  logs     Show recent background proxy logs
  refresh  Renew fnos-token and entry-token using the saved longToken
  status   Show process status, configuration and token expiry
  logout   Remove stored tokens
  version  Show version

Examples:
  fnconnect-proxy login --server myid.fnos.net --target https://myid.fnos.net/app/llmproxy --username alice
  fnconnect-proxy start
  fnconnect-proxy stop
`, version)
}

func readPassword() string {
	fmt.Fprint(os.Stderr, "fnOS password: ")
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err == nil {
			return strings.TrimSpace(string(b))
		}
		return ""
	}
	s, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	fmt.Fprintln(os.Stderr)
	return strings.TrimSpace(s)
}

func cfgPath() string {
	p, err := appcfg.DefaultPath()
	if err != nil {
		log.Fatal(err)
	}
	return p
}

func cmdLogin(args []string) {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	server := fs.String("server", "", "FN Connect host, e.g. myid.fnos.net")
	target := fs.String("target", "", "App target, e.g. https://myid.fnos.net/app/llmproxy")
	user := fs.String("username", "", "fnOS username")
	listen := fs.String("listen", "127.0.0.1:13431", "local listen address")
	passwordStdin := fs.Bool("password-stdin", false, "read password from stdin")
	_ = fs.Parse(args)
	path := cfgPath()
	cfg, err := appcfg.Load(path)
	if err != nil {
		log.Fatal(err)
	}
	if *server == "" {
		*server = cfg.Server
	}
	if *target == "" {
		*target = cfg.TargetURL
	}
	if *user == "" {
		*user = cfg.Username
	}
	if *server == "" || *target == "" || *user == "" {
		log.Fatal("--server, --target and --username are required on first login")
	}
	if err := pxy.ValidateTarget(*target); err != nil {
		log.Fatal(err)
	}
	var pass string
	if *passwordStdin {
		b, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		pass = strings.TrimSpace(b)
	} else {
		pass = readPassword()
	}
	if pass == "" {
		log.Fatal("empty password")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	c := fnos.New(*server, 30*time.Second)
	if err := c.Connect(ctx); err != nil {
		log.Fatal(err)
	}
	defer c.Close()
	sess, err := c.Login(ctx, *user, pass, cfg.DeviceType, cfg.DeviceName)
	if err != nil {
		log.Fatal(err)
	}
	now := time.Now()
	cfg.Server = *server
	cfg.TargetURL = *target
	cfg.Username = *user
	cfg.Listen = *listen
	cfg.FNOSToken = sess.FNOSToken
	cfg.LongToken = sess.LongToken
	cfg.EntryToken = sess.EntryToken
	cfg.SignKey = sess.SignKey
	cfg.BackID = sess.BackID
	cfg.UID = sess.UID
	cfg.Admin = sess.Admin
	cfg.FNOSTokenExpires = now.Add(8 * time.Hour)
	cfg.EntryTokenExpires = now.Add(8 * time.Hour)
	if cfg.LongToken != "" {
		cfg.LongTokenExpires = now.Add(25 * 24 * time.Hour)
	}
	if err := appcfg.Save(path, cfg); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("login ok\nserver: %s\ntarget: %s\nentry-token expires (assumed): %s\nlongToken stored: %v\nconfig: %s\n", cfg.Server, cfg.TargetURL, cfg.EntryTokenExpires.Format(time.RFC3339), cfg.LongToken != "", path)
}

func cmdRefresh() {
	path := cfgPath()
	cfg, err := appcfg.Load(path)
	if err != nil {
		log.Fatal(err)
	}
	mgr := pxy.NewTokenManager(path, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := mgr.RenewSession(ctx); err != nil {
		log.Fatal(err)
	}
	c := mgr.Config()
	fmt.Printf("refreshed; entry-token expires (assumed): %s\n", c.EntryTokenExpires.Format(time.RFC3339))
}

func mask(s string) string {
	if s == "" {
		return "(none)"
	}
	if len(s) <= 8 {
		return "********"
	}
	return s[:4] + "…" + s[len(s)-4:]
}
func cmdStatus() {
	path := cfgPath()
	cfg, err := appcfg.Load(path)
	if err != nil {
		log.Fatal(err)
	}
	manager, err := daemon.New(path)
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	state, running, err := manager.Status(ctx)
	if err != nil {
		fmt.Printf("process: unavailable (%v)\n", err)
	} else if running {
		fmt.Printf("process: running (PID %d)\nprocess listen: %s\nstarted: %s\n", state.PID, state.Listen, state.Started.Format(time.RFC3339))
	} else {
		fmt.Println("process: stopped")
	}
	fmt.Printf("log: %s\n", manager.LogPath())
	fmt.Printf("config: %s\nserver: %s\ntarget: %s\nlisten: %s\nusername: %s\nfnos-token: %s  expires: %s\nentry-token: %s  expires: %s\nlongToken: %s  expires: %s\n", path, cfg.Server, cfg.TargetURL, cfg.Listen, cfg.Username, mask(cfg.FNOSToken), fmtTime(cfg.FNOSTokenExpires), mask(cfg.EntryToken), fmtTime(cfg.EntryTokenExpires), mask(cfg.LongToken), fmtTime(cfg.LongTokenExpires))
}
func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.Format(time.RFC3339) + " (" + time.Until(t).Round(time.Minute).String() + ")"
}

func cmdLogout() {
	path := cfgPath()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := appcfg.Update(ctx, path, func(cfg *appcfg.Config) error {
		cfg.FNOSToken = ""
		cfg.EntryToken = ""
		cfg.LongToken = ""
		cfg.SignKey = ""
		cfg.BackID = ""
		cfg.FNOSTokenExpires = time.Time{}
		cfg.EntryTokenExpires = time.Time{}
		cfg.LongTokenExpires = time.Time{}
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("tokens cleared")
}

func cmdProcess(action string, args []string) {
	fs := flag.NewFlagSet(action, flag.ExitOnError)
	var listen *string
	var lines *int
	if action == "start" || action == "restart" {
		listen = fs.String("listen", "", "override local listen address")
	}
	if action == "logs" {
		lines = fs.Int("lines", 100, "number of recent log lines (1-10000)")
	}
	_ = fs.Parse(args)
	if fs.NArg() != 0 {
		log.Fatal("unexpected arguments")
	}
	manager, err := daemon.New(cfgPath())
	if err != nil {
		log.Fatal(err)
	}
	if action == "logs" {
		if err := manager.Logs(os.Stdout, *lines); err != nil {
			log.Fatal(err)
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if action == "stop" {
		if err := manager.Stop(ctx); err != nil {
			log.Fatal(err)
		}
		fmt.Println("stopped")
		return
	}
	var state daemon.State
	if action == "restart" {
		state, err = manager.Restart(ctx, *listen)
	} else {
		state, err = manager.Start(ctx, *listen)
	}
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("running (PID %d)\nlisten: %s\nlog: %s\n", state.PID, state.Listen, manager.LogPath())
}

func cmdServe(args []string) {
	if err := runServe(args); err != nil {
		log.Fatal(err)
	}
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := fs.String("listen", "", "override local listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	path := cfgPath()
	cfg, err := appcfg.Load(path)
	if err != nil {
		return err
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:13431"
	}
	if cfg.TargetURL == "" {
		return fmt.Errorf("target missing; run login first")
	}
	mgr := pxy.NewTokenManager(path, cfg)
	handler, err := pxy.NewServer(mgr, cfg.TargetURL)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if token := os.Getenv(daemon.TokenEnv); token != "" {
		_ = os.Unsetenv(daemon.TokenEnv)
		manager, err := daemon.New(path)
		if err != nil {
			return err
		}
		control, err := manager.OpenControl(ctx, token, listener.Addr().String(), cancel)
		if err != nil {
			return err
		}
		defer control.Close()
	}
	go mgr.AutoRefresh(ctx)
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second}
	shutdownDone := make(chan error, 1)
	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		err := server.Shutdown(shutdownCtx)
		if err != nil {
			_ = server.Close()
		}
		shutdownDone <- err
	}()
	log.Printf("listening on http://%s -> %s", listener.Addr(), cfg.TargetURL)
	serveErr := server.Serve(listener)
	cancel()
	shutdownErr := <-shutdownDone
	if serveErr != nil && serveErr != http.ErrServerClosed {
		return serveErr
	}
	return shutdownErr
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "help", "--help", "-h":
		usage()
	case "login":
		cmdLogin(os.Args[2:])
	case "serve":
		cmdServe(os.Args[2:])
	case "start", "stop", "restart", "logs":
		cmdProcess(os.Args[1], os.Args[2:])
	case "refresh":
		cmdRefresh()
	case "status":
		cmdStatus()
	case "logout":
		cmdLogout()
	case "version", "--version", "-v":
		fmt.Println(version)
	default:
		usage()
		os.Exit(2)
	}
}
