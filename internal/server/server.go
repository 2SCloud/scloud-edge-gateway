package server

import (
	"2scloud-edge-gateway/internal/runtime"
	"crypto/tls"
	"log"
	"net/http"
)

func Run() error {
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})

	wafHandler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/healthz" {
			mux.ServeHTTP(w, req)
			return
		}

		reqObj := runtime.BuildWafRequest(req)

		decision, err := runtime.CallModule(req.Context(), wafModule, wafCfg, reqObj)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if decision != 0 {
			http.Error(w, "Request blocked by WAF", http.StatusForbidden)
			return
		}

		mux.ServeHTTP(w, req)
	})

	srv := &http.Server{
		Addr:      ":443",
		Handler:   wafHandler,
		TLSConfig: tlsConfig,
	}

	log.Println("Listening on :443")
	return srv.ListenAndServeTLS("/certs/tls.crt", "/certs/tls.key")
}
