package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	// Embeds the tz database. Without it LoadLocation("Asia/Kolkata") fails on
	// a distroless image and every booking time computation is wrong or panics.
	_ "time/tzdata"

	"aiims-appointment/pkg/appointments"
	"aiims-appointment/pkg/bot"
	"aiims-appointment/pkg/config"
)

const healthAddr = "127.0.0.1:8080"

// runHealthcheck is the container probe: `/app/bot -healthcheck`. The image is
// distroless, so there is no shell to run a test command in.
func runHealthcheck() {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + healthAddr + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: status", resp.StatusCode)
		os.Exit(1)
	}
	os.Exit(0)
}

func main() {
	healthcheck := flag.Bool("healthcheck", false, "probe a running instance and exit 0 if healthy")
	flag.Parse()
	if *healthcheck {
		runHealthcheck()
	}

	if os.Getenv("AIIMS_MASTER_KEY") == "" && os.Getenv("AIIMS_ALLOW_PLAINTEXT") != "1" {
		fmt.Fprintln(os.Stderr, "WARNING: AIIMS_MASTER_KEY is unset; sensitive config may be stored as plaintext. Set AIIMS_MASTER_KEY or explicitly set AIIMS_ALLOW_PLAINTEXT=1.")
	}

	logFile, err := os.OpenFile("bot.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		slog.Error("open log file", "error", err)
		os.Exit(1)
	}
	multiWriter := io.MultiWriter(os.Stdout, logFile)
	handler := slog.NewTextHandler(multiWriter, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(handler))
	// The telegram library reports getUpdates failures through the stdlib
	// logger and retries forever. Unredirected, a revoked token makes the bot
	// permanently deaf with the evidence going to a stream nobody captures.
	log.SetOutput(multiWriter)
	defer logFile.Close()

	slog.Info("AIIMS Bot Starting...")

	cfg, err := config.LoadConfig("config.json")
	if err != nil {
		slog.Error("config load failed", "error", err)
		os.Exit(1)
	}

	if err := cfg.Validate(); err != nil {
		slog.Error("invalid config", "error", err)
		os.Exit(1)
	}
	slog.Info("config loaded", "config", cfg.Redact())

	apptStore, err := appointments.Open("appointments.db")
	if err != nil {
		slog.Error("open appointments store", "error", err)
		os.Exit(1)
	}
	defer apptStore.Close()

	botSvc, err := bot.NewBotService(cfg, apptStore)
	if err != nil {
		slog.Error("bot init failed", "error", err)
		os.Exit(1)
	}

	kolTZ, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		slog.Error("load kolkata tz", "error", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	apptSrv := appointments.NewServer(apptStore, kolTZ)
	go func() {
		if err := apptSrv.ListenAndServe(ctx, healthAddr); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("appointments http server failed", "error", err)
		}
	}()
	slog.Info("appointments http server listening", "addr", healthAddr)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigChan
		go func() {
			time.Sleep(15 * time.Second)
			slog.Warn("forced shutdown after timeout")
			os.Exit(1)
		}()
		slog.Info("shutdown signal received, saving state and stopping gracefully...")
		// cancel() first: stopping the update producer last left a window where
		// fresh updates were dispatched into an already torn-down service.
		cancel()
		botSvc.Stop()
	}()

	slog.Info("bot is running...")
	botSvc.Start(ctx, cfg.OwnerID)
	slog.Info("bot stopped.")
}
