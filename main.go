package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bong", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		configPath  = fs.String("c", "", "config file path (env: BONG_CONFIG, default: config.yaml)")
		listenAddr  = fs.String("l", "", "listen address (env: BONG_LISTEN)")
		baseURL     = fs.String("b", "", "base URL for opensearch.xml (env: BONG_BASE_URL)")
		prefix      = fs.String("p", "", "shortcut prefix (env: BONG_PREFIX)")
		defaultBong = fs.String("d", "", "default shortcut key (env: BONG_DEFAULT)")
		checkOnly   = fs.Bool("check", false, "validate config and exit")
		showVersion = fs.Bool("version", false, "print version and exit")
	)

	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: bong [flags] [config.yaml]\n\nFlags:\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if *showVersion {
		fmt.Fprintln(stdout, "bong", version)
		return 0
	}

	if fs.NArg() > 1 || (fs.NArg() == 1 && strings.TrimSpace(*configPath) != "") {
		fmt.Fprintln(stderr, "bong: give one config path, via -c or as an argument")
		fs.Usage()
		return 2
	}

	cfgPath := resolveOption(*configPath, getenv("BONG_CONFIG"), "config.yaml")
	if fs.NArg() == 1 {
		cfgPath = fs.Arg(0)
	}

	cfg, err := loadRawConfigFile(cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "bong: %v\n", err)
		return 1
	}

	cfg.Listen = resolveOption(*listenAddr, getenv("BONG_LISTEN"), cfg.Listen)
	cfg.BaseURL = resolveOption(*baseURL, getenv("BONG_BASE_URL"), cfg.BaseURL)
	cfg.Prefix = resolveOption(*prefix, getenv("BONG_PREFIX"), cfg.Prefix)
	cfg.Default = resolveOption(*defaultBong, getenv("BONG_DEFAULT"), cfg.Default)

	if err := cfg.NormalizeAndValidate(); err != nil {
		fmt.Fprintf(stderr, "bong: %v\n", err)
		return 1
	}

	if *checkOnly {
		fmt.Fprintf(stdout, "bong: %s ok, %d shortcuts, prefix %q, default %q\n",
			cfgPath, len(cfg.Bongs), cfg.Prefix, cfg.Default)
		return 0
	}

	logger := slog.New(slog.NewTextHandler(stderr, nil))
	srvHandler := NewServer(cfg, logger)

	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srvHandler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("starting bong server",
			"listen", cfg.Listen,
			"prefix", cfg.Prefix,
			"default", cfg.Default,
			"shortcuts", len(cfg.Bongs),
		)
		errCh <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		logger.Error("serve failed", "err", err)
		return 1
	case <-ctx.Done():
	}

	logger.Info("shutting down server")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), httpServer.WriteTimeout+5*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown failed", "err", err)
		return 1
	}
	return 0
}

func resolveOption(flagVal, envVal, fallback string) string {
	for _, v := range []string{flagVal, envVal} {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return fallback
}
