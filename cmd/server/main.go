package main

import (
	"log"
	"net/http"
)

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("Halo dari DB Platform\n"))
	})

	log.Println("listening on :8080")
	// ListenAndServe
	log.Fatal(http.ListenAndServe(":8080", mux))
}
