package main

import (
	"log"
	"net/http"
	"os"
)

func main() {
	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "./data"
	}
	store := NewStore(dataDir)
	state, err := store.Load()
	if err != nil {
		log.Fatalf("load state: %v", err)
	}
	svc := NewService(state, store)
	srv := NewServer(svc)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("多币种出行资金管家 listening on :%s (data: %s)", port, dataDir)
	if err := http.ListenAndServe(":"+port, srv.Routes()); err != nil {
		log.Fatal(err)
	}
}
