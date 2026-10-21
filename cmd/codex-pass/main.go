package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"codex-pass/internal/auth"
	debuglog "codex-pass/internal/debug"
	"codex-pass/internal/proxy"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(arguments []string) int {
	flags := flag.NewFlagSet("codex-pass", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: codex-pass --key API_KEY [--listen ADDRESS] [--debug FILE] AUTH_FILE [AUTH_FILE ...]")
		fmt.Fprintln(flags.Output(), "\nTransparent Codex Responses API proxy listening on 127.0.0.1:8317 by default.")
		flags.PrintDefaults()
	}
	apiKey := flags.String("key", "", "required client Bearer API key")
	debugPath := flags.String("debug", "", "optional JSON Lines debug log path")
	listen := flags.String("listen", "", "listen address (overrides CODEX_PROXY_LISTEN)")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if strings.TrimSpace(*apiKey) == "" {
		fmt.Fprintln(os.Stderr, "error: --key is required")
		flags.Usage()
		return 2
	}
	authFiles := flags.Args()
	if len(authFiles) == 0 {
		fmt.Fprintln(os.Stderr, "error: at least one Codex auth file is required")
		flags.Usage()
		return 2
	}
	credentials, err := auth.LoadAll(authFiles)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	httpClient := proxy.NewHTTPClient()
	pool, err := auth.NewPool(credentials, auth.RefreshConfig{Client: httpClient})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	var logger *debuglog.Logger
	if *debugPath != "" {
		logger, err = debuglog.New(*debugPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 2
		}
		defer func() {
			if closeErr := logger.Close(); closeErr != nil {
				fmt.Fprintln(os.Stderr, "debug log error:", closeErr)
			}
		}()
	}
	handler, err := proxy.New(proxy.Config{
		APIKey: *apiKey, AuthPool: pool, HTTPClient: httpClient, Logger: logger,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	listenAddress := resolveListenAddress(*listen, os.Getenv("CODEX_PROXY_LISTEN"))
	if !isLoopbackAddress(listenAddress) {
		fmt.Fprintln(os.Stderr, "warning: listen address is not loopback; put TLS, access control, and protected debug logs in front of this proxy")
	}
	server := &http.Server{
		Addr:              listenAddress,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		WriteTimeout:      0,
		MaxHeaderBytes:    1 << 20,
	}
	log.Printf("codex-pass listening on http://%s with %d auth file(s)", listenAddress, len(credentials))
	if logger != nil {
		log.Printf("detailed debug logging enabled at %s", *debugPath)
	}
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-interrupts
		shutdownContext, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			log.Printf("shutdown error: %v", err)
		}
	}()
	err = server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "server error:", err)
		return 1
	}
	return 0
}

func resolveListenAddress(cliAddress, environmentAddress string) string {
	if strings.TrimSpace(cliAddress) != "" {
		return cliAddress
	}
	if strings.TrimSpace(environmentAddress) != "" {
		return environmentAddress
	}
	return "127.0.0.1:8317"
}

func isLoopbackAddress(address string) bool {
	return strings.HasPrefix(address, "127.") || strings.HasPrefix(address, "[::1]") || strings.HasPrefix(address, "localhost:")
}
