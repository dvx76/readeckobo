package main

import (
	"log"

	"readeckobo/internal/app"
	"readeckobo/internal/config"
	"readeckobo/internal/logger"
	"readeckobo/internal/store"
	"readeckobo/internal/webserver"
)

func main() {
	cfg, err := config.Load("./config.yaml")
	if err != nil {
		log.Fatalf("Error loading configuration: %v", err)
	}

	logLevel, err := logger.ParseLevel(cfg.LogLevel)
	if err != nil {
		log.Fatalf("Error parsing log level: %v", err)
	}
	appLogger := logger.New(logLevel)

	// State store (SQLite, WAL) under server.data_dir.
	st, err := store.Open(cfg.Server.DataDir)
	if err != nil {
		log.Fatalf("Error opening state store: %v", err)
	}
	defer func() { _ = st.Close() }()

	// Initialize application
	application := app.NewApp(
		app.WithConfig(cfg),
		app.WithLogger(appLogger),
		app.WithStore(st),
	)

	// Initialize and start the web server
	webserver.ListenAndServe(cfg.Server.Port, application, appLogger)

	// Keep the main goroutine alive
	select {}
}
