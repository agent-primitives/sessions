// Command fakemodel serves the deterministic fake chat completions endpoint on :8080 (FAKEMODEL_ADDR).
package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"github.com/agent-primitives/sessions/examples/agentsuite-substrate/fakemodel"
)

func main() {
	addr := os.Getenv("FAKEMODEL_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	srv := &http.Server{Addr: addr, Handler: &fakemodel.Server{}, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("fakemodel: serving on %s", addr)
	log.Fatal(srv.ListenAndServe())
}
