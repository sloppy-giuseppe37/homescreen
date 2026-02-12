package main

// main.go — Entry point for the Homescreen web server.
//
// Starts the HTTP server on port 8000 and connects to the MQTT broker.
// The server is stateless — all device state comes from MQTT.

import (
	"embed"
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
