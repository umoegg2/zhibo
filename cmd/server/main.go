package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"zhibo/internal/server"
)

func main() {
	addr := flag.String("addr", ":8080", "HTTP listen address")
	flag.Parse()

	srv := server.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
		<-c
		cancel()
	}()

	log.Printf("control server listening on %s", *addr)
	if err := srv.Run(ctx, *addr); err != nil && err != context.Canceled {
		log.Fatalf("server error: %v", err)
	}
}
