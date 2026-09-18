// canter-node-proxy exposes only the node gateway when tunnelling a local demo.
package main

import (
	"flag"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

func nodeGatewayHandler(upstream *url.URL) http.Handler {
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "node gateway unavailable", http.StatusBadGateway)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/node/") && !(r.Method == http.MethodGet && r.URL.Path == "/readyz") {
			http.NotFound(w, r)
			return
		}
		proxy.ServeHTTP(w, r)
	})
}

func main() {
	listen := flag.String("listen", "127.0.0.1:8091", "local gateway listener")
	target := flag.String("upstream", "http://127.0.0.1:8081", "control plane origin")
	flag.Parse()
	upstream, err := url.Parse(*target)
	if err != nil || upstream.Scheme != "http" || upstream.Hostname() != "127.0.0.1" || upstream.User != nil || upstream.RawQuery != "" || upstream.Path != "" {
		log.Fatal("upstream must be an HTTP origin on 127.0.0.1")
	}
	server := &http.Server{Addr: *listen, Handler: nodeGatewayHandler(upstream), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second}
	log.Printf("Node-only gateway listening on %s", *listen)
	log.Fatal(server.ListenAndServe())
}
