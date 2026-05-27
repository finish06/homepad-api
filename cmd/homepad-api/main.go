package main

import (
	"log"
	"net/http"
	"os"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/api"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	h := api.New(api.Deps{
		Registration: os.Getenv("HOMEPAD_REGISTRATION"),
	})

	log.Printf("homepad-api listening on :%s (scaffold — no business logic yet)", port)
	if err := http.ListenAndServe(":"+port, h); err != nil {
		log.Fatal(err)
	}
}
