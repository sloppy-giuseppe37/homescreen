package main

// handlers.go — HTTP request handlers for the web API.
//
// Routes:
//   GET  /              → serves the HTML page (Go template, rendered from config)
//   GET  /api/events    → SSE stream of state updates
//   POST /api/heating/zone/{zone}/temperature  → set target temp for all rooms in zone
//   POST /api/heating/zone/{zone}/quiet        → set quiet mode for all rooms in zone
//   POST /api/heating/room/{zone}/{room}/power → toggle one room on/off
//   POST /api/light/{zone}/{name}/power        → toggle one light on/off

import (
	"encoding/json"
	"fmt"
	"text/template"
	"log"
	"net/http"
	"strconv"
	"strings"
)

// App holds all the shared dependencies for our HTTP handlers.
type App struct {
	Config      *Config
	MQTT        *MQTTClient
	Broadcaster *SSEBroadcaster
	Template    *template.Template
}

// PageData is the data passed to the HTML template.
// It includes the config (for rendering zones/rooms) plus a JSON snapshot
// of current state, so the page renders correctly without waiting for SSE.
type PageData struct {
	*Config
	InitialState string // JSON array of state events, safe to embed in <script>
}

// SetupRoutes registers all HTTP routes on the given ServeMux.
func (app *App) SetupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /", app.handleIndex)
	mux.HandleFunc("GET /api/events", app.handleSSE)
	mux.HandleFunc("POST /api/heating/zone/{zone}/temperature", app.handleZoneTemperature)
	mux.HandleFunc("POST /api/heating/zone/{zone}/quiet", app.handleZoneQuiet)
	mux.HandleFunc("POST /api/heating/room/{zone}/{room}/power", app.handleRoomPower)
	mux.HandleFunc("POST /api/light/{zone}/{name}/power", app.handleLightPower)
	mux.HandleFunc("POST /api/light/{zone}/{name}/brightness", app.handleLightBrightness)
}

// ---------- Page handler ----------

// handleIndex renders the main HTML page.
// The page is a Go template that receives the zone/room config
// plus a JSON snapshot of current device state. This means the page
// renders with correct values immediately — no flash of defaults
// while waiting for the SSE connection to deliver state.
func (app *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	if !app.MQTT.IsConnected() {
		http.Error(w, "MQTT broker unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := PageData{
		Config:       app.Config,
		InitialState: app.buildSnapshot(),
	}
	if err := app.Template.Execute(w, data); err != nil {
		log.Printf("template error: %v", err)
		http.Error(w, "Internal Server Error", 500)
	}
}

// buildSnapshot returns a JSON array string containing one event object
// per heating room and per light — the same format as SSE events.
// This is embedded in the HTML so the browser has state on first paint.
func (app *App) buildSnapshot() string {
	var events []json.RawMessage
	for _, zone := range app.Config.Zones {
		for _, room := range zone.Heating {
			events = append(events, json.RawMessage(app.buildHeatingEvent(zone.Name, room)))
		}
		for _, light := range zone.Lights {
			events = append(events, json.RawMessage(app.buildLightEvent(zone.Name, light)))
		}
	}
	data, _ := json.Marshal(events)
	return string(data)
}

// ---------- SSE handler ----------

// handleSSE opens an SSE stream to the browser.
// First it sends a snapshot of all current state, then streams changes.
func (app *App) handleSSE(w http.ResponseWriter, r *http.Request) {
	if !app.MQTT.IsConnected() {
		http.Error(w, "MQTT broker unavailable", http.StatusServiceUnavailable)
		return
	}
	app.Broadcaster.ServeHTTP(w, r, func(ch chan string) {
		// Send current state for every heating room and every light
		for _, zone := range app.Config.Zones {
			for _, room := range zone.Heating {
				event := app.buildHeatingEvent(zone.Name, room)
				ch <- event
			}
			for _, light := range zone.Lights {
				event := app.buildLightEvent(zone.Name, light)
				ch <- event
			}
		}
	})
}

// buildHeatingEvent creates a JSON event string for one heating room
// by reading its 3 topics from the MQTT cache.
func (app *App) buildHeatingEvent(zoneName string, room HeatingRoom) string {
	powerTopic, tempTopic, quietTopic := room.HeatingTopics()

	// Read cached values (default to sensible fallbacks if not yet received)
	powerVal, _ := app.MQTT.GetValue(powerTopic)
	tempVal, _ := app.MQTT.GetValue(tempTopic)
	quietVal, _ := app.MQTT.GetValue(quietTopic)

	// Parse the raw MQTT strings into typed values
	power := powerVal == "1"
	temp := 20.0 // default
	if t, err := strconv.ParseFloat(tempVal, 64); err == nil {
		temp = t
	}
	quiet := quietVal == "1"

	// Build the JSON event
	event := map[string]any{
		"type":        "heating",
		"zone":        zoneName,
		"room":        room.Name,
		"power":       power,
		"target_temp": temp,
		"quiet":       quiet,
	}
	data, _ := json.Marshal(event)
	return string(data)
}

// buildLightEvent creates a JSON event string for one light group.
// A light group may contain multiple zigbee2mqtt entities.
// The "on" state uses any-on logic: if any entity is ON, the group is ON.
// Only when all entities are OFF is the group OFF.
// Brightness is included only if any entity reports it (max across entities).
// "brightness_on" indicates whether any brightness-capable entity is ON
// (used by the frontend to grey out the slider when all dimmable lights are off).
func (app *App) buildLightEvent(zoneName string, light LightConfig) string {
	on := false
	brightness := -1
	brightnessOn := false
	prefix := app.Config.MQTT.TopicPrefix
	for _, entity := range light.Entities {
		// Skip unavailable entities — treat them as OFF
		if !app.isEntityAvailable(prefix, entity) {
			continue
		}
		stateTopic := prefix + "/" + entity
		if val, ok := app.MQTT.GetValue(stateTopic); ok {
			ls := parseLightPayload(val)
			on = on || ls.On
			if ls.Brightness >= 0 {
				if ls.Brightness > brightness {
					brightness = ls.Brightness
				}
				if ls.On {
					brightnessOn = true
				}
			}
		}
	}

	event := map[string]any{
		"type": "light",
		"zone": zoneName,
		"name": light.Name,
		"on":   on,
	}
	if brightness >= 0 {
		event["brightness"] = brightness
		event["brightness_on"] = brightnessOn
	}
	data, _ := json.Marshal(event)
	return string(data)
}

// isEntityAvailable checks whether a zigbee2mqtt entity is available.
// It reads the cached availability topic. Returns false if the cached value
// indicates offline/unavailable, true otherwise (including when no availability
// message has been received — we assume available by default).
func (app *App) isEntityAvailable(prefix, entity string) bool {
	availTopic := prefix + "/" + entity + "/availability"
	if val, ok := app.MQTT.GetValue(availTopic); ok {
		// zigbee2mqtt publishes either JSON {"state":"offline"} or plain "offline"
		val = strings.TrimSpace(strings.ToLower(val))
		if val == "offline" || strings.Contains(val, `"offline"`) {
			return false
		}
	}
	return true
}

// lightEntityState holds the parsed state of a single zigbee2mqtt entity.
type lightEntityState struct {
	On         bool
	Brightness int // 0-254 if reported, -1 if not supported
}

// parseLightPayload extracts on/off state and optional brightness from
// a zigbee2mqtt state payload. zigbee2mqtt publishes JSON like
// {"state":"ON","brightness":254} on entity topics.
func parseLightPayload(payload string) lightEntityState {
	var msg struct {
		State      string `json:"state"`
		Brightness *int   `json:"brightness"`
	}
	if err := json.Unmarshal([]byte(payload), &msg); err != nil {
		return lightEntityState{Brightness: -1}
	}
	ls := lightEntityState{
		On:         msg.State == "ON",
		Brightness: -1,
	}
	if msg.Brightness != nil {
		ls.Brightness = *msg.Brightness
	}
	return ls
}

// parseLightState is a convenience wrapper that returns just the on/off bool.
func parseLightState(payload string) bool {
	return parseLightPayload(payload).On
}

// TopicToEvent converts an MQTT topic + value into a JSON SSE event string.
// It looks up which room/light the topic belongs to by checking the config.
// Returns "" if the topic doesn't match anything we know about.
func (app *App) TopicToEvent(topic, value string) string {
	for _, zone := range app.Config.Zones {
		// Check heating topics
		for _, room := range zone.Heating {
			power, temp, quiet := room.HeatingTopics()
			if topic == power || topic == temp || topic == quiet {
				return app.buildHeatingEvent(zone.Name, room)
			}
		}
		// Check light entity state topics
		for _, light := range zone.Lights {
			for _, stateTopic := range light.StateTopics(app.Config.MQTT.TopicPrefix) {
				if topic == stateTopic {
					return app.buildLightEvent(zone.Name, light)
				}
			}
			// Check light entity availability topics
			for _, availTopic := range light.AvailabilityTopics(app.Config.MQTT.TopicPrefix) {
				if topic == availTopic {
					return app.buildLightEvent(zone.Name, light)
				}
			}
		}
	}
	return ""
}

// ---------- API handlers ----------

// apiRequest is the JSON body for all POST endpoints.
// "value" can be a bool (for power/quiet) or a number (for temperature).
type apiRequest struct {
	Value      any  `json:"value"`
	Brightness *int `json:"brightness,omitempty"` // optional, used when turning a light on
}

// readJSON reads and parses the JSON request body.
func readJSON(r *http.Request) (apiRequest, error) {
	var req apiRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return req, fmt.Errorf("invalid JSON: %w", err)
	}
	return req, nil
}

// checkMQTT returns true if MQTT is connected, false otherwise.
// If disconnected, it writes a 503 response.
func (app *App) checkMQTT(w http.ResponseWriter) bool {
	if !app.MQTT.IsConnected() {
		http.Error(w, "MQTT broker unavailable", http.StatusServiceUnavailable)
		return false
	}
	return true
}

// findZone looks up a zone by name in the config.
func (app *App) findZone(name string) *ZoneConfig {
	for i := range app.Config.Zones {
		if app.Config.Zones[i].Name == name {
			return &app.Config.Zones[i]
		}
	}
	return nil
}

// handleZoneTemperature sets the target temperature for ALL rooms in a zone.
// This is because the UI has one slider per zone, not per room.
func (app *App) handleZoneTemperature(w http.ResponseWriter, r *http.Request) {
	if !app.checkMQTT(w) {
		return
	}
	zoneName := r.PathValue("zone")
	zone := app.findZone(zoneName)
	if zone == nil {
		http.Error(w, "zone not found", http.StatusNotFound)
		return
	}

	req, err := readJSON(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Convert the value to a float, then format as "21.0" for MQTT
	tempFloat, ok := req.Value.(float64)
	if !ok {
		http.Error(w, "value must be a number", http.StatusBadRequest)
		return
	}
	tempStr := strconv.FormatFloat(tempFloat, 'f', 1, 64)

	// Publish to every room in this zone
	var errors []string
	for _, room := range zone.Heating {
		_, tempTopic, _ := room.HeatingTopics()
		if err := app.MQTT.Publish(tempTopic, tempStr); err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", room.Name, err))
		}
	}

	if len(errors) > 0 {
		http.Error(w, strings.Join(errors, "; "), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleZoneQuiet sets quiet mode for ALL rooms in a zone.
func (app *App) handleZoneQuiet(w http.ResponseWriter, r *http.Request) {
	if !app.checkMQTT(w) {
		return
	}
	zoneName := r.PathValue("zone")
	zone := app.findZone(zoneName)
	if zone == nil {
		http.Error(w, "zone not found", http.StatusNotFound)
		return
	}

	req, err := readJSON(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Convert bool to "0" or "1"
	val := "0"
	if b, ok := req.Value.(bool); ok && b {
		val = "1"
	}

	// Publish to every room in this zone
	var errors []string
	for _, room := range zone.Heating {
		_, _, quietTopic := room.HeatingTopics()
		if err := app.MQTT.Publish(quietTopic, val); err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", room.Name, err))
		}
	}

	if len(errors) > 0 {
		http.Error(w, strings.Join(errors, "; "), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRoomPower turns a single room's heating on or off.
func (app *App) handleRoomPower(w http.ResponseWriter, r *http.Request) {
	if !app.checkMQTT(w) {
		return
	}
	zoneName := r.PathValue("zone")
	roomName := r.PathValue("room")

	zone := app.findZone(zoneName)
	if zone == nil {
		http.Error(w, "zone not found", http.StatusNotFound)
		return
	}

	// Find the room within the zone
	var room *HeatingRoom
	for i := range zone.Heating {
		if zone.Heating[i].Name == roomName {
			room = &zone.Heating[i]
			break
		}
	}
	if room == nil {
		http.Error(w, "room not found", http.StatusNotFound)
		return
	}

	req, err := readJSON(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	val := "0"
	if b, ok := req.Value.(bool); ok && b {
		val = "1"
	}

	powerTopic, tempTopic, _ := room.HeatingTopics()

	// When turning ON, publish the target temperature first so the unit
	// starts at the correct setpoint rather than whatever stale value
	// it had from before.
	if val == "1" {
		if cached, ok := app.MQTT.GetValue(tempTopic); ok && cached != "" {
			if err := app.MQTT.Publish(tempTopic, cached); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
	}

	if err := app.MQTT.Publish(powerTopic, val); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleLightPower turns a light group on or off.
// Publishes to all entities in the group via zigbee2mqtt /set topics.
func (app *App) handleLightPower(w http.ResponseWriter, r *http.Request) {
	if !app.checkMQTT(w) {
		return
	}
	zoneName := r.PathValue("zone")
	lightName := r.PathValue("name")

	zone := app.findZone(zoneName)
	if zone == nil {
		http.Error(w, "zone not found", http.StatusNotFound)
		return
	}

	// Find the light within the zone
	var light *LightConfig
	for i := range zone.Lights {
		if zone.Lights[i].Name == lightName {
			light = &zone.Lights[i]
			break
		}
	}
	if light == nil {
		http.Error(w, "light not found", http.StatusNotFound)
		return
	}

	req, err := readJSON(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	turningOn := false
	if b, ok := req.Value.(bool); ok && b {
		turningOn = true
	}

	// Publish to every entity in this light group.
	// When turning ON with a brightness value, brightness-capable entities
	// get {"state":"ON","brightness":N} so they start at the right level.
	prefix := app.Config.MQTT.TopicPrefix
	var errors []string
	for _, entity := range light.Entities {
		var payload string
		if turningOn && req.Brightness != nil {
			// Check if this entity supports brightness (has reported it before)
			stateTopic := prefix + "/" + entity
			supportsBrightness := false
			if val, ok := app.MQTT.GetValue(stateTopic); ok {
				ls := parseLightPayload(val)
				supportsBrightness = ls.Brightness >= 0
			}
			if supportsBrightness {
				payload = fmt.Sprintf(`{"state":"ON","brightness":%d}`, *req.Brightness)
			} else {
				payload = `{"state":"ON"}`
			}
		} else if turningOn {
			payload = `{"state":"ON"}`
		} else {
			payload = `{"state":"OFF"}`
		}
		topic := light.SetTopic(prefix, entity)
		if err := app.MQTT.Publish(topic, payload); err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", entity, err))
		}
	}

	if len(errors) > 0 {
		http.Error(w, strings.Join(errors, "; "), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleLightBrightness sets the brightness for a light group.
// Only publishes to entities that support brightness (i.e., have previously
// reported a brightness value in their state payload).
func (app *App) handleLightBrightness(w http.ResponseWriter, r *http.Request) {
	if !app.checkMQTT(w) {
		return
	}
	zoneName := r.PathValue("zone")
	lightName := r.PathValue("name")

	zone := app.findZone(zoneName)
	if zone == nil {
		http.Error(w, "zone not found", http.StatusNotFound)
		return
	}

	var light *LightConfig
	for i := range zone.Lights {
		if zone.Lights[i].Name == lightName {
			light = &zone.Lights[i]
			break
		}
	}
	if light == nil {
		http.Error(w, "light not found", http.StatusNotFound)
		return
	}

	req, err := readJSON(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	brightnessFloat, ok := req.Value.(float64)
	if !ok {
		http.Error(w, "value must be a number", http.StatusBadRequest)
		return
	}
	brightness := int(brightnessFloat)
	if brightness < 0 || brightness > 254 {
		http.Error(w, "brightness must be 0-254", http.StatusBadRequest)
		return
	}

	payload := fmt.Sprintf(`{"brightness":%d}`, brightness)

	// Only publish to entities that support brightness AND are currently ON.
	// Dragging the slider must not turn on a light that is off.
	prefix := app.Config.MQTT.TopicPrefix
	var errors []string
	for _, entity := range light.Entities {
		stateTopic := prefix + "/" + entity
		if val, ok := app.MQTT.GetValue(stateTopic); ok {
			ls := parseLightPayload(val)
			if ls.Brightness < 0 || !ls.On {
				continue // entity doesn't support brightness or is off
			}
		} else {
			continue // no cached state, skip
		}
		topic := light.SetTopic(prefix, entity)
		if err := app.MQTT.Publish(topic, payload); err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", entity, err))
		}
	}

	if len(errors) > 0 {
		http.Error(w, strings.Join(errors, "; "), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
