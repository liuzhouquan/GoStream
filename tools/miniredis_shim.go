package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/alicebob/miniredis/v2"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "address to listen on")
	flag.Parse()

	mr := miniredis.NewMiniRedis()
	if *addr == "127.0.0.1:0" || *addr == "" {
		if err := mr.Start(); err != nil {
			log.Fatalf("failed to start miniredis: %v", err)
		}
	} else {
		if err := mr.StartAddr(*addr); err != nil {
			log.Fatalf("failed to start miniredis on %s: %v", *addr, err)
		}
	}
	defer mr.Close()

	fmt.Printf("MINIREDIS_READY %s\n", mr.Addr())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
}

