package main

import (
	"log"
	"net/http"
	"os"

	"if3110.local/db-platform/internal/web"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, web.New()))
}
