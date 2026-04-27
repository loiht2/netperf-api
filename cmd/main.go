package main

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/netperf/netperf-api/internal/api"
	"github.com/netperf/netperf-api/internal/executor"
	"github.com/netperf/netperf-api/internal/k8sclient"
	"github.com/netperf/netperf-api/internal/store"
)

func main() {
	client, err := k8sclient.New()
	if err != nil {
		log.Fatalf("failed to initialise Kubernetes client: %v", err)
	}

	s := store.New()
	// Sweep terminal tasks older than 1 hour every 5 minutes.
	s.StartSweeper(context.Background(), 5*time.Minute, time.Hour)

	exec := executor.New(client)
	h := api.New(s, exec)

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	addr := ":8080"
	log.Printf("netperf API server listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
