package main

// status.go — the /status page: a once-in-a-while "is everything working?" view.
//
// Its one design rule is that it must render when nothing else does. No MQTT
// guard, no SSE, no JavaScript: if the broker is unreachable, saying so is the
// whole point of the page. /status.json serves the same data for scripting.
//
// This file uses html/template rather than the text/template the main page
// needs — there is no inline JS here for the contextual escaper to mangle, so
// the escaping is worth having.

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"math"
	"net/http"
	"os"
	"runtime"
	"time"
)

// Version is the release this binary was built from. The Makefile and the pkg
// workflow set it at link time:  -ldflags "-X main.Version=..."
var Version = "dev"

// startedAt is when this process came up, for the uptime reading.
var startedAt = time.Now()

var statusTmpl = template.Must(template.ParseFS(templateFS, "templates/status.html"))

// StatusData is everything /status and /status.json report.
type StatusData struct {
	Version    string    `json:"version"`
	Go         string    `json:"go"`
	Host       string    `json:"host"`
	PID        int       `json:"pid"`
	StartedAt  time.Time `json:"started_at"`
	Uptime     string    `json:"uptime"`
	Goroutines int       `json:"goroutines"`
	HeapMB     float64   `json:"heap_mb"`
	MQTT       MQTTStats `json:"mqtt"`
	SSEClients int       `json:"sse_clients"`
	Mode       string    `json:"mode"`
	Zones      int       `json:"zones"`
	Rooms      int       `json:"heating_rooms"`
	Lights     int       `json:"lights"`
	Scenes     int       `json:"scenes"`
}

// statusView adds the human-readable forms the HTML page shows. The JSON
// endpoint keeps the raw timestamps instead, which are easier to compute with.
type statusView struct {
	StatusData
	StartedAtText   string
	ConnectedFor    string
	LastLostText    string
	LastMessageText string
}

// buildStatus gathers the current state of the process.
func (app *App) buildStatus() StatusData {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	host, _ := os.Hostname()

	s := StatusData{
		Version:    Version,
		Go:         runtime.Version(),
		Host:       host,
		PID:        os.Getpid(),
		StartedAt:  startedAt,
		Uptime:     humanDuration(time.Since(startedAt)),
		Goroutines: runtime.NumGoroutine(),
		HeapMB:     math.Round(float64(mem.HeapAlloc)/(1024*1024)*10) / 10,
		MQTT:       app.MQTT.Stats(),
		SSEClients: app.Broadcaster.ClientCount(),
		Mode:       "heating",
		Zones:      len(app.Config.Zones),
		Scenes:     len(app.Config.Scenes),
	}
	if app.IsCoolingMode() {
		s.Mode = "cooling"
	}
	for _, zone := range app.Config.Zones {
		s.Rooms += len(zone.Heating)
		s.Lights += len(zone.Lights)
	}
	return s
}

// handleStatus renders the status page.
func (app *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	data := app.buildStatus()
	view := statusView{
		StatusData:      data,
		StartedAtText:   data.StartedAt.Format("2006-01-02 15:04:05 MST"),
		ConnectedFor:    since(data.MQTT.ConnectedAt),
		LastLostText:    ago(data.MQTT.LastLostAt),
		LastMessageText: ago(data.MQTT.LastMessageAt),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := statusTmpl.Execute(w, view); err != nil {
		log.Printf("status template error: %v", err)
	}
}

// handleStatusJSON serves the same data for scripts and monitoring checks.
func (app *App) handleStatusJSON(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(app.buildStatus()); err != nil {
		log.Printf("status JSON error: %v", err)
	}
}

// since renders how long something has been true — an uptime, not a timestamp.
func since(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "not connected"
	}
	return humanDuration(time.Since(*t))
}

// ago describes when something last happened, or "never" if it hasn't.
func ago(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "never"
	}
	return humanDuration(time.Since(*t)) + " ago"
}

// humanDuration renders a duration the way you read it on a status page:
// coarse, two units at most, no wall of decimals.
func humanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}
