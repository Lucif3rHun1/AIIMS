package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"aiims-appointment/pkg/bot"
	"aiims-appointment/pkg/config"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("🚀 AIIMS Bot Starting...")

	cfg, err := config.LoadConfig("config.json")
	if err != nil {
		log.Fatalf("Config load failed: %v", err)
	}

	if err := cfg.Validate(); err != nil {
		log.Fatalf("Invalid config: %v", err)
	}
	log.Println(cfg.Redact())

	botSvc, err := bot.NewBotService(cfg)
	if err != nil {
		log.Fatalf("Bot init failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigChan
		log.Println("⏳ Shutdown signal received, stopping gracefully...")
		botSvc.Stop()
		cancel()

		go func() {
			time.Sleep(15 * time.Second)
			log.Println("⚠️ Forced shutdown after timeout")
			os.Exit(1)
		}()
	}()

	log.Println("🟢 Bot is running...")
	botSvc.Start(ctx)
	log.Println("👋 Bot stopped.")
}
