package webserver

import (
	"fmt"
	"net/http"

	"readeckobo/internal/app"
	"readeckobo/internal/logger"
)

// ListenAndServe starts the HTTP server on the specified port.
func ListenAndServe(port int, application *app.App, logger *logger.Logger) {
	addr := fmt.Sprintf(":%d", port)
	logger.Infof("Web server starting on port %s", addr)

	mux := http.NewServeMux()

	// Register handlers
	mux.HandleFunc("/api/kobo/get", application.HandleKoboGet)
	mux.HandleFunc("/api/kobo/download", application.HandleKoboDownload)
	mux.HandleFunc("/api/kobo/send", application.HandleKoboSend)
	mux.HandleFunc("/api/convert-image", application.HandleConvertImage)

	// On-device agent routes (stream E wire contract): kepub serving, the
	// agent state feed and annotation ingress. Go 1.22+ path wildcards.
	mux.HandleFunc("/api/kepub/{id}", application.HandleKepubDownload)
	mux.HandleFunc("/api/agent/state", application.HandleAgentState)
	mux.HandleFunc("/api/agent/annotations", application.HandleAgentAnnotations)

	mux.HandleFunc("/instapaper-proxy/storeapi/v1/initialization", application.HandleDumpAndForward)
	mux.HandleFunc("/instapaper-proxy/storeapi/", application.HandleDumpAndForward)
	mux.HandleFunc("/instapaper-proxy/instapaper/api/kobo/get", application.HandleKoboGet)
	mux.HandleFunc("/instapaper-proxy/instapaper/api/kobo/download", application.HandleKoboDownload)
	mux.HandleFunc("/instapaper-proxy/instapaper/api/kobo/send", application.HandleKoboSend)

	// The Kobo client resolves these endpoints relative to its configured
	// instapaper_env_url base, which may be the host root or the proxy
	// prefix. Register the bare (Pocket-style) aliases and their prefixed
	// equivalents so the device's calls match regardless of the base.
	mux.HandleFunc("/get", application.HandleKoboGet)
	mux.HandleFunc("/send", application.HandleKoboSend)
	mux.HandleFunc("/text", application.HandleKoboDownload)
	mux.HandleFunc("/download", application.HandleKoboDownload)
	mux.HandleFunc("/instapaper-proxy/instapaper/get", application.HandleKoboGet)
	mux.HandleFunc("/instapaper-proxy/instapaper/send", application.HandleKoboSend)
	mux.HandleFunc("/instapaper-proxy/instapaper/text", application.HandleKoboDownload)
	mux.HandleFunc("/instapaper-proxy/instapaper/download", application.HandleKoboDownload)

	// Catch-all for unimplemented routes
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		logger.Warnf("404 Not Found: URL=%s, Method=%s, Params=%v", r.URL.Path, r.Method, r.URL.Query())
		http.Error(w, "404 Not Found", http.StatusNotFound)
	})

	// Apply logging middleware
	loggedMux := LoggingMiddleware(mux)

	if err := http.ListenAndServe(addr, loggedMux); err != nil {
		logger.Errorf("Web server failed to start: %v", err)
	}
}
