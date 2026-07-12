package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"aiims-appointment/pkg/bot"
	"aiims-appointment/pkg/config"
)

func main() {
	logFile, err := os.OpenFile("bot.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		slog.Error("open log file", "error", err)
		os.Exit(1)
	}
	multiWriter := io.MultiWriter(os.Stdout, logFile)
	handler := slog.NewTextHandler(multiWriter, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(handler))
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

	botSvc, err := bot.NewBotService(cfg)
	if err != nil {
		slog.Error("bot init failed", "error", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

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
		botSvc.Stop()
		cancel()
	}()

	slog.Info("bot is running...")
	botSvc.Start(ctx, cfg.OwnerID)
	slog.Info("bot stopped.")
}
