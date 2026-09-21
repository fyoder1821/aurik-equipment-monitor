// Command server starts the equipment monitoring HTTP API.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"aurik-equipment-monitor/internal/httpapi"
	"aurik-equipment-monitor/internal/refdata"
	"aurik-equipment-monitor/internal/store"
	"aurik-equipment-monitor/internal/worker"
)

const (
	workerCount   = 4
	queueSize     = 256
	dbConnTimeout = 10 * time.Second
)

func main() {
	ref, err := refdata.Load()
	if err != nil {
		log.Fatalf("failed to load reference data: %v", err)
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL is required, e.g. postgres://aurik:aurik@localhost:5432/aurik_equipment_monitor?sslmode=disable")
	}

	connectCtx, cancel := context.WithTimeout(context.Background(), dbConnTimeout)
	defer cancel()
	dbPool, err := store.Open(connectCtx, dsn)
	if err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}
	defer dbPool.Close()

	st := store.New(dbPool, ref)
	workerPool := worker.NewPool(workerCount, queueSize, st, st, ref)

	router := httpapi.NewRouter(st, workerPool)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("aurik equipment-monitor listening on :%s", port)
	if err := http.ListenAndServe(":"+port, router); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
