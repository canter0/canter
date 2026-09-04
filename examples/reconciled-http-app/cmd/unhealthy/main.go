// Command unhealthy is an acceptance fixture proving that a candidate which
// never becomes ready cannot replace the currently healthy release.
package main

import (
	"net/http"
	"os"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	http.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	})
	http.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"version":"unhealthy"}`)) })
	_ = http.ListenAndServe("127.0.0.1:"+port, nil)
}
