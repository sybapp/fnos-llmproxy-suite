package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	appcfg "fnconnect-proxy/internal/config"
	"fnconnect-proxy/internal/fnos"
	pxy "fnconnect-proxy/internal/proxy"
)

const version = "1.0.5"

func usage() {
	fmt.Fprintf(os.Stderr, `fnconnect-proxy %s

Commands:
  login    Login to fnOS, save longToken/fnos-token/entry-token (password is not saved)
  serve    Listen locally and proxy requests through FN Connect
  refresh  Refresh entry-token using the current fnos-token (never rotates fnos-token)
  status   Show configuration and token expiry
  logout   Remove stored tokens
  version  Show version

Examples:
  fnconnect-proxy login --server myid.fnos.net --target https://myid.fnos.net/app/llmproxy --username alice
  fnconnect-proxy serve
`, version)
}

func readPassword() string {
	fmt.Fprint(os.Stderr, "fnOS password: ")
	if _, err := exec.LookPath("stty"); err == nil {
		_ = exec.Command("stty", "-echo").Run()
		defer exec.Command("stty", "echo").Run()
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
	cfg, _ := appcfg.Load(path)
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
	if err := mgr.Refresh(ctx, true); err != nil {
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
	cfg, err := appcfg.Load(path)
	if err != nil {
		log.Fatal(err)
	}
	cfg.FNOSToken = ""
	cfg.EntryToken = ""
	cfg.LongToken = ""
	cfg.SignKey = ""
	cfg.BackID = ""
	cfg.FNOSTokenExpires = time.Time{}
	cfg.EntryTokenExpires = time.Time{}
	cfg.LongTokenExpires = time.Time{}
	if err := appcfg.Save(path, cfg); err != nil {
		log.Fatal(err)
	}
	fmt.Println("tokens cleared")
}

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", "", "override local listen address")
	_ = fs.Parse(args)
	path := cfgPath()
	cfg, err := appcfg.Load(path)
	if err != nil {
		log.Fatal(err)
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:13431"
	}
	if cfg.TargetURL == "" {
		log.Fatal("target missing; run login first")
	}
	mgr := pxy.NewTokenManager(path, cfg)
	srvHandler, err := pxy.NewServer(mgr, cfg.TargetURL)
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.AutoRefresh(ctx)
	server := &http.Server{Addr: cfg.Listen, Handler: srvHandler, ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		cancel()
		c, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelShutdown()
		_ = server.Shutdown(c)
	}()
	log.Printf("listening on http://%s -> %s", cfg.Listen, cfg.TargetURL)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "login":
		cmdLogin(os.Args[2:])
	case "serve":
		cmdServe(os.Args[2:])
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
