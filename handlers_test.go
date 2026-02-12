package main

import (
	"bytes"
	"encoding/json"
	"text/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testConfig returns a small config for testing.
func testConfig() *Config {
	return &Config{
		MQTT: MQTTConfig{Broker: "tcp://localhost:1883"},
		Zones: []ZoneConfig{
			{
				Name: "Upstairs",
				Heating: []HeatingRoom{
					{Name: "Bedroom", UnitID: "BedroomFaikin"},
					{Name: "Guest Room", UnitID: "GuestFaikin"},
				},
				Lights: []LightConfig{
					{Name: "Bedroom", Topic: "lights/bedroom"},
				},
			},
			{
				Name: "Downstairs",
				Heating: []HeatingRoom{
					{Name: "Kitchen", UnitID: "KitchenFaikin"},
				},
				Lights: []LightConfig{
					{Name: "Kitchen", Topic: "lights/kitchen"},
				},
			},
		},
	}
}

// fakeMQTTClient creates a minimal MQTTClient with a pre-populated cache
// and no actual MQTT connection. Good for testing handlers in isolation.
func fakeMQTTClient(cache map[string]string) *MQTTClient {
	m := &MQTTClient{
		cache: make(map[string]string),
	}
	for k, v := range cache {
		m.cache[k] = v
	}
	return m
}

// testApp creates an App wired up for testing (no real MQTT connection).
func testApp(cache map[string]string) *App {
	cfg := testConfig()
	tmpl := template.Must(template.ParseFiles("templates/index.html"))
	return &App{
		Config:      cfg,
		MQTT:        fakeMQTTClient(cache),
		Broadcaster: NewSSEBroadcaster(),
		Template:    tmpl,
	}
}

// ---------- Topic-to-Event mapping ----------

// TestTopicToEvent_HeatingPower tests that an MQTT power topic
// is correctly mapped to a heating SSE event.
func TestTopicToEvent_HeatingPower(t *testing.T) {
	app := testApp(map[string]string{
		"HomeKit/BedroomFaikin_Thermostat/Thermostat/TargetHeatingCoolingState": "1",
		"HomeKit/BedroomFaikin_Thermostat/Thermostat/TargetTemperature":        "21.0",
		"HomeKit/BedroomFaikin_IndoorQuiet/Switch/On":                          "0",
	})

	eventJSON := app.TopicToEvent(
		"HomeKit/BedroomFaikin_Thermostat/Thermostat/TargetHeatingCoolingState", "1",
	)

	var event map[string]any
	if err := json.Unmarshal([]byte(eventJSON), &event); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	if event["type"] != "heating" {
		t.Errorf("type = %v, want 'heating'", event["type"])
	}
	if event["zone"] != "Upstairs" {
		t.Errorf("zone = %v, want 'Upstairs'", event["zone"])
	}
	if event["room"] != "Bedroom" {
		t.Errorf("room = %v, want 'Bedroom'", event["room"])
	}
	if event["power"] != true {
		t.Errorf("power = %v, want true", event["power"])
	}
	if event["target_temp"] != 21.0 {
		t.Errorf("target_temp = %v, want 21.0", event["target_temp"])
	}
}

// TestTopicToEvent_Light tests that a light topic is correctly mapped.
func TestTopicToEvent_Light(t *testing.T) {
	app := testApp(map[string]string{
		"lights/bedroom": "1",
	})

	eventJSON := app.TopicToEvent("lights/bedroom", "1")

	var event map[string]any
	json.Unmarshal([]byte(eventJSON), &event)

	if event["type"] != "light" {
		t.Errorf("type = %v, want 'light'", event["type"])
	}
	if event["zone"] != "Upstairs" {
		t.Errorf("zone = %v, want 'Upstairs'", event["zone"])
	}
	if event["on"] != true {
		t.Errorf("on = %v, want true", event["on"])
	}
}

// TestTopicToEvent_Unknown tests that an unknown topic returns empty string.
func TestTopicToEvent_Unknown(t *testing.T) {
	app := testApp(nil)
	result := app.TopicToEvent("unknown/topic", "42")
	if result != "" {
		t.Errorf("expected empty string for unknown topic, got %q", result)
	}
}

// ---------- HTTP handler tests ----------

// TestHandleIndex verifies the index page renders with zone data.
func TestHandleIndex(t *testing.T) {
	app := testApp(nil)

	mux := http.NewServeMux()
	app.SetupRoutes(mux)

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	body := w.Body.String()

	// The rendered HTML should contain our zone and room names
	for _, want := range []string{"Upstairs", "Downstairs", "Bedroom", "Kitchen"} {
		if !strings.Contains(body, want) {
			t.Errorf("response body missing %q", want)
		}
	}
}

// TestHandleRoomPower_NotFound tests that a request for a non-existent
// zone returns 404.
func TestHandleRoomPower_NotFound(t *testing.T) {
	app := testApp(nil)

	mux := http.NewServeMux()
	app.SetupRoutes(mux)

	body := `{"value": true}`
	req := httptest.NewRequest("POST", "/api/heating/room/NoSuchZone/Bedroom/power",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 404 {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// TestHandleRoomPower_RoomNotFound tests that a request for a
// non-existent room in a valid zone returns 404.
func TestHandleRoomPower_RoomNotFound(t *testing.T) {
	app := testApp(nil)

	mux := http.NewServeMux()
	app.SetupRoutes(mux)

	body := `{"value": true}`
	req := httptest.NewRequest("POST", "/api/heating/room/Upstairs/NoSuchRoom/power",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 404 {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// TestHandleLightPower_NotFound tests 404 for unknown light.
func TestHandleLightPower_NotFound(t *testing.T) {
	app := testApp(nil)

	mux := http.NewServeMux()
	app.SetupRoutes(mux)

	body := `{"value": true}`
	req := httptest.NewRequest("POST", "/api/light/Upstairs/NoSuchLight/power",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 404 {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// TestHandleZoneTemperature_BadJSON tests that malformed JSON returns 400.
func TestHandleZoneTemperature_BadJSON(t *testing.T) {
	app := testApp(nil)

	mux := http.NewServeMux()
	app.SetupRoutes(mux)

	req := httptest.NewRequest("POST", "/api/heating/zone/Upstairs/temperature",
		bytes.NewBufferString("not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 400 {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// TestHandleZoneTemperature_BadValue tests that a non-numeric value returns 400.
func TestHandleZoneTemperature_BadValue(t *testing.T) {
	app := testApp(nil)

	mux := http.NewServeMux()
	app.SetupRoutes(mux)

	req := httptest.NewRequest("POST", "/api/heating/zone/Upstairs/temperature",
		bytes.NewBufferString(`{"value": "hot"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 400 {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// ---------- buildHeatingEvent / buildLightEvent ----------

// TestBuildHeatingEvent_Defaults verifies sensible defaults when
// no MQTT data has been received yet.
func TestBuildHeatingEvent_Defaults(t *testing.T) {
	app := testApp(nil) // empty cache

	eventJSON := app.buildHeatingEvent("Upstairs", HeatingRoom{Name: "Bedroom", UnitID: "BedroomFaikin"})

	var event map[string]any
	json.Unmarshal([]byte(eventJSON), &event)

	// Default: power off, temp 20, quiet off
	if event["power"] != false {
		t.Errorf("default power = %v, want false", event["power"])
	}
	if event["target_temp"] != 20.0 {
		t.Errorf("default temp = %v, want 20.0", event["target_temp"])
	}
	if event["quiet"] != false {
		t.Errorf("default quiet = %v, want false", event["quiet"])
	}
}

// TestBuildLightEvent verifies light event JSON structure.
func TestBuildLightEvent(t *testing.T) {
	app := testApp(map[string]string{
		"lights/bedroom": "1",
	})

	eventJSON := app.buildLightEvent("Upstairs", LightConfig{Name: "Bedroom", Topic: "lights/bedroom"})

	var event map[string]any
	json.Unmarshal([]byte(eventJSON), &event)

	if event["type"] != "light" {
		t.Errorf("type = %v, want 'light'", event["type"])
	}
	if event["on"] != true {
		t.Errorf("on = %v, want true", event["on"])
	}
}
