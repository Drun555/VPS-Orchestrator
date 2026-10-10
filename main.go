package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) != 1 {
		log.Fatal("vps-reality-master does not accept arguments; configure using environment variables or the adjacent .env file")
	}
	config, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	store, err := NewStore(filepath.Join(config.DataDir, "master.json"))
	if err != nil {
		log.Fatal(err)
	}
	app := NewApp(config, store)
	server := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", config.ListenAddress, config.Port),
		Handler:           app.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		log.Fatal(err)
	}
	pidPath := filepath.Join(config.DataDir, "master.pid")
	if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
		_ = listener.Close()
		log.Fatal(err)
	}
	defer os.Remove(pidPath)

	errorsChannel := make(chan error, 1)
	go func() {
		log.Printf("VPS Reality Master listening on http://%s", server.Addr)
		errorsChannel <- server.Serve(listener)
	}()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	select {
	case received := <-signals:
		log.Printf("received %s, shutting down", received)
	case serveError := <-errorsChannel:
		if !errors.Is(serveError, http.ErrServerClosed) {
			log.Fatal(serveError)
		}
		return
	}

	shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownContext); err != nil {
		log.Printf("graceful shutdown failed: %v", err)
	}
}
