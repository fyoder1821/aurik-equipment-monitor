// Command server starts the equipment monitoring HTTP API.
package main

import (
	"log"
	"net/http"
	"os"

	"aurik-equipment-monitor/internal/httpapi"
	"aurik-equipment-monitor/internal/refdata"
	"aurik-equipment-monitor/internal/store"
	"aurik-equipment-monitor/internal/worker"
)

const (
	workerCount = 4
	queueSize   = 256
)

func main() {
	ref, err := refdata.Load()
	if err != nil {
		log.Fatalf("failed to load reference data: %v", err)
	}

	st := store.New(ref)
	pool := worker.NewPool(workerCount, queueSize, st, st, ref)

	router := httpapi.NewRouter(st, pool)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("aurik equipment-monitor listening on :%s", port)
	if err := http.ListenAndServe(":"+port, router); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
