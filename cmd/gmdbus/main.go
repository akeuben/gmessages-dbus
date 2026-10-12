package main

import (
	"os"
	"os/signal"
	"syscall"
	"time"

	"akeuben.ca/gmessages-dbus/internal/service"
	"akeuben.ca/gmessages-dbus/internal/store"
	"github.com/rs/zerolog"
)

func main() {
	log := zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.TimeOnly}).
		With().Timestamp().Logger()

	path, err := store.DefaultPath()
	if err != nil {
		log.Fatal().Err(err).Msg("cannot determine data path")
	}
	st := store.New(path, true, log) // true = prefer the Secret Service keyring

	svc := service.New(log, st)
	if err := svc.Start(); err != nil {
		log.Fatal().Err(err).Msg("failed to start")
	}
	log.Info().Str("name", service.BusName).Msg("service running")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Info().Msg("shutting down")
	svc.Shutdown()
}
