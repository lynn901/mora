// Command healthcheck is a tiny static HTTP probe used as the docker compose
// HEALTHCHECK for the distroless application images (wiki-api, rag-worker,
// mcp-server). distroless has no shell/curl/wget, so a static binary is the
// only way to run an HTTP healthcheck inside those images.
//
// Usage: healthcheck <url>   (default http://localhost:8080/healthz)
// Exits 0 when the URL returns a 2xx status, 1 otherwise.
package main

import (
	"net/http"
	"os"
	"time"
)

func main() {
	url := "http://localhost:8080/healthz"
	if len(os.Args) > 1 {
		url = os.Args[1]
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		os.Exit(0)
	}
	os.Exit(1)
}
