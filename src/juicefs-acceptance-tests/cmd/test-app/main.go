package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"github.com/cloudfoundry/juicefs-release/src/juicefs-acceptance-tests/internal/testapp"
)

func main() {
	handler, err := testapp.New("/data", os.Getenv("TEST_TOKEN"))
	if err != nil {
		log.Fatal(err)
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	server := &http.Server{Addr: ":" + port, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(server.ListenAndServe())
}
