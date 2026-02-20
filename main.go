package main

// main.go — Entry point for the Homescreen web server.
//
// Starts the HTTP server on port 8000 and connects to the MQTT broker.
// The server is stateless — all device state comes from MQTT.

import (
	"embed"
	"io/fs"
	"text/template"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
)

// Embed the HTML template into the binary so we don't need
// to worry about file paths at runtime.
//
//go:embed templates/index.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

//go:embed docs
var docsFS embed.FS

func main() {
	// --- Load configuration ---
	cfg, err := LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	log.Printf("Config loaded: %d zones", len(cfg.Zones))

	// --- Parse the HTML template ---
	tmpl, err := template.ParseFS(templateFS, "templates/index.html")
	if err != nil {
		log.Fatalf("Failed to parse template: %v", err)
	}

	// --- Set up SSE broadcaster ---
	broadcaster := NewSSEBroadcaster()

	// --- Connect to MQTT ---
	// The onChange callback is called for every MQTT message.
	// It converts the raw topic+value into a JSON event and
	// broadcasts it to all connected browsers.
	// The onConnectionLost callback disconnects all SSE clients
	// so they trigger the offline UI flow.
	var app *App // forward declaration so the callback can use it

	mqttClient, err := NewMQTTClient(cfg, func(topic, value string) {
		if app == nil {
			return
		}
		// Convert the MQTT topic to a JSON SSE event
		eventJSON := app.TopicToEvent(topic, value)
		if eventJSON != "" {
			broadcaster.Broadcast(eventJSON)
		}
	}, func() {
		// MQTT connection lost — disconnect all SSE clients to trigger offline UI
		broadcaster.DisconnectAll()
	})
	if err != nil {
		log.Fatalf("Failed to connect to MQTT: %v", err)
	}
	defer mqttClient.Disconnect()

	// --- Create the App (wires everything together) ---
	app = &App{
		Config:      cfg,
		MQTT:        mqttClient,
		Broadcaster: broadcaster,
		Template:    tmpl,
	}

	// --- Set up HTTP routes ---
	mux := http.NewServeMux()
	app.SetupRoutes(mux)

	// Serve vendored static assets (fonts, CSS)
	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		log.Fatalf("Failed to create static sub-fs: %v", err)
	}
	staticHandler := http.FileServer(http.FS(staticSub))
	mux.Handle("GET /static/", http.StripPrefix("/static/", staticHandler))

	// Serve service worker from root scope so it can control all pages
	mux.HandleFunc("GET /sw.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("Cache-Control", "no-cache")
		data, _ := staticFS.ReadFile("static/sw.js")
		w.Write(data)
	})

	// Serve user documentation at /help
	docsSub, err := fs.Sub(docsFS, "docs")
	if err != nil {
		log.Fatalf("Failed to create docs sub-fs: %v", err)
	}
	docsHandler := http.FileServer(http.FS(docsSub))
	mux.Handle("GET /help/", http.StripPrefix("/help/", docsHandler))
	mux.HandleFunc("GET /help", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/help/", http.StatusMovedPermanently)
	})

	// --- Start the server ---
	addr := ":8000"
	log.Printf("Starting server on %s", addr)

	// Graceful shutdown on SIGINT/SIGTERM
	go func() {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		<-sigs
		log.Println("Shutting down...")
		mqttClient.Disconnect()
		os.Exit(0)
	}()

	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
