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
		MQTT: MQTTConfig{Broker: "tcp://localhost:1883", TopicPrefix: "zigbee2mqtt"},
		Zones: []ZoneConfig{
			{
				Name: "Upstairs",
				Heating: []HeatingRoom{
					{Name: "Bedroom", UnitID: "BedroomFaikin"},
					{Name: "Guest Room", UnitID: "GuestFaikin"},
				},
				Lights: []LightConfig{
					{Name: "Bedroom", Entities: []string{"bed", "ceiling"}},
				},
			},
			{
				Name: "Downstairs",
				Heating: []HeatingRoom{
					{Name: "Kitchen", UnitID: "KitchenFaikin"},
				},
				Lights: []LightConfig{
					{Name: "Kitchen", Entities: []string{"fairy_lights", "kitchen_table_1"}},
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

// TestTopicToEvent_Light tests that a zigbee2mqtt entity state topic
// is correctly mapped to a light SSE event.
func TestTopicToEvent_Light(t *testing.T) {
	app := testApp(map[string]string{
		"zigbee2mqtt/bed": `{"state":"ON"}`,
	})

	eventJSON := app.TopicToEvent("zigbee2mqtt/bed", `{"state":"ON"}`)

	var event map[string]any
	json.Unmarshal([]byte(eventJSON), &event)

	if event["type"] != "light" {
		t.Errorf("type = %v, want 'light'", event["type"])
	}
	if event["zone"] != "Upstairs" {
		t.Errorf("zone = %v, want 'Upstairs'", event["zone"])
	}
	if event["name"] != "Bedroom" {
		t.Errorf("name = %v, want 'Bedroom'", event["name"])
	}
	if event["on"] != true {
		t.Errorf("on = %v, want true", event["on"])
	}
}

// TestTopicToEvent_Light_AnyOn tests the any-on aggregation logic:
// if any entity in a light group is ON, the group reports ON.
func TestTopicToEvent_Light_AnyOn(t *testing.T) {
	app := testApp(map[string]string{
		"zigbee2mqtt/bed":     `{"state":"OFF"}`,
		"zigbee2mqtt/ceiling": `{"state":"ON"}`,
	})

	eventJSON := app.TopicToEvent("zigbee2mqtt/ceiling", `{"state":"ON"}`)

	var event map[string]any
	json.Unmarshal([]byte(eventJSON), &event)

	if event["on"] != true {
		t.Errorf("on = %v, want true (any-on: ceiling is ON)", event["on"])
	}
}

// TestTopicToEvent_Light_AllOff tests that the group reports OFF
// only when all entities are OFF.
func TestTopicToEvent_Light_AllOff(t *testing.T) {
	app := testApp(map[string]string{
		"zigbee2mqtt/bed":     `{"state":"OFF"}`,
		"zigbee2mqtt/ceiling": `{"state":"OFF"}`,
	})

	eventJSON := app.TopicToEvent("zigbee2mqtt/bed", `{"state":"OFF"}`)

	var event map[string]any
	json.Unmarshal([]byte(eventJSON), &event)

	if event["on"] != false {
		t.Errorf("on = %v, want false (all entities OFF)", event["on"])
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

// TestHandleIndex_InitialState verifies that the page embeds a JSON
// snapshot of current MQTT state, so the browser doesn't flash defaults.
func TestHandleIndex_InitialState(t *testing.T) {
	// Pre-populate the MQTT cache with known values
	app := testApp(map[string]string{
		"HomeKit/BedroomFaikin_Thermostat/Thermostat/TargetHeatingCoolingState": "1",
		"HomeKit/BedroomFaikin_Thermostat/Thermostat/TargetTemperature":        "23.0",
		"HomeKit/BedroomFaikin_IndoorQuiet/Switch/On":                          "1",
		"zigbee2mqtt/bed": `{"state":"ON"}`,
	})

	mux := http.NewServeMux()
	app.SetupRoutes(mux)

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	body := w.Body.String()

	// The rendered HTML should contain the inline initial state
	if !strings.Contains(body, "const initialState = [") {
		t.Error("page missing initialState JSON")
	}

	// Verify specific values are embedded (not the defaults)
	if !strings.Contains(body, `"target_temp":23`) {
		t.Error("initial state missing target_temp 23")
	}
	if !strings.Contains(body, `"power":true`) {
		t.Error("initial state missing power:true")
	}
	if !strings.Contains(body, `"quiet":true`) {
		t.Error("initial state missing quiet:true")
	}
}

// TestBuildSnapshot verifies the snapshot contains events for all rooms and lights.
func TestBuildSnapshot(t *testing.T) {
	app := testApp(map[string]string{
		"HomeKit/BedroomFaikin_Thermostat/Thermostat/TargetTemperature": "22.0",
		"zigbee2mqtt/fairy_lights": `{"state":"ON"}`,
	})

	snapshot := app.buildSnapshot()

	// Parse the snapshot JSON
	var events []map[string]any
	if err := json.Unmarshal([]byte(snapshot), &events); err != nil {
		t.Fatalf("invalid snapshot JSON: %v", err)
	}

	// We have 3 heating rooms + 2 lights = 5 events
	if len(events) != 5 {
		t.Errorf("snapshot has %d events, want 5", len(events))
	}

	// Count types
	heating, lights := 0, 0
	for _, e := range events {
		switch e["type"] {
		case "heating":
			heating++
		case "light":
			lights++
		}
	}
	if heating != 3 {
		t.Errorf("snapshot has %d heating events, want 3", heating)
	}
	if lights != 2 {
		t.Errorf("snapshot has %d light events, want 2", lights)
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
		"zigbee2mqtt/bed": `{"state":"ON"}`,
	})

	eventJSON := app.buildLightEvent("Upstairs", LightConfig{Name: "Bedroom", Entities: []string{"bed", "ceiling"}})

	var event map[string]any
	json.Unmarshal([]byte(eventJSON), &event)

	if event["type"] != "light" {
		t.Errorf("type = %v, want 'light'", event["type"])
	}
	// any-on: bed is ON, ceiling has no cached state → group is ON
	if event["on"] != true {
		t.Errorf("on = %v, want true", event["on"])
	}
}

// TestBuildLightEvent_AllOff verifies light reports OFF when all entities are OFF.
func TestBuildLightEvent_AllOff(t *testing.T) {
	app := testApp(map[string]string{
		"zigbee2mqtt/bed":     `{"state":"OFF"}`,
		"zigbee2mqtt/ceiling": `{"state":"OFF"}`,
	})

	eventJSON := app.buildLightEvent("Upstairs", LightConfig{Name: "Bedroom", Entities: []string{"bed", "ceiling"}})

	var event map[string]any
	json.Unmarshal([]byte(eventJSON), &event)

	if event["on"] != false {
		t.Errorf("on = %v, want false", event["on"])
	}
}

// TestParseLightState verifies parsing of zigbee2mqtt JSON payloads.
func TestParseLightState(t *testing.T) {
	tests := []struct {
		payload string
		want    bool
	}{
		{`{"state":"ON"}`, true},
		{`{"state":"OFF"}`, false},
		{`{"state":"ON","brightness":254}`, true},
		{`{"state":"OFF","brightness":0}`, false},
		{`not json`, false},
		{"", false},
	}
	for _, tt := range tests {
		got := parseLightState(tt.payload)
		if got != tt.want {
			t.Errorf("parseLightState(%q) = %v, want %v", tt.payload, got, tt.want)
		}
	}
}

// TestParseLightPayload verifies full payload parsing including brightness.
func TestParseLightPayload(t *testing.T) {
	tests := []struct {
		name       string
		payload    string
		wantOn     bool
		wantBright int
	}{
		{"on no brightness", `{"state":"ON"}`, true, -1},
		{"off no brightness", `{"state":"OFF"}`, false, -1},
		{"on with brightness", `{"state":"ON","brightness":200}`, true, 200},
		{"off with brightness", `{"state":"OFF","brightness":0}`, false, 0},
		{"brightness max", `{"state":"ON","brightness":254}`, true, 254},
		{"invalid json", `not json`, false, -1},
		{"empty", "", false, -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseLightPayload(tt.payload)
			if got.On != tt.wantOn {
				t.Errorf("On = %v, want %v", got.On, tt.wantOn)
			}
			if got.Brightness != tt.wantBright {
				t.Errorf("Brightness = %d, want %d", got.Brightness, tt.wantBright)
			}
		})
	}
}

// TestBuildLightEvent_WithBrightness verifies brightness and brightness_on
// are included in event when entities report brightness.
func TestBuildLightEvent_WithBrightness(t *testing.T) {
	app := testApp(map[string]string{
		"zigbee2mqtt/bed":     `{"state":"ON","brightness":200}`,
		"zigbee2mqtt/ceiling": `{"state":"ON","brightness":150}`,
	})

	eventJSON := app.buildLightEvent("Upstairs", LightConfig{Name: "Bedroom", Entities: []string{"bed", "ceiling"}})

	var event map[string]any
	json.Unmarshal([]byte(eventJSON), &event)

	if event["on"] != true {
		t.Errorf("on = %v, want true", event["on"])
	}
	// Should report max brightness across entities
	if event["brightness"] != 200.0 {
		t.Errorf("brightness = %v, want 200", event["brightness"])
	}
	// At least one brightness-capable entity is ON
	if event["brightness_on"] != true {
		t.Errorf("brightness_on = %v, want true", event["brightness_on"])
	}
}

// TestBuildLightEvent_BrightnessOff verifies brightness_on is false when
// all brightness-capable entities are OFF (but brightness value is still reported).
func TestBuildLightEvent_BrightnessOff(t *testing.T) {
	app := testApp(map[string]string{
		"zigbee2mqtt/bed":     `{"state":"OFF","brightness":200}`,
		"zigbee2mqtt/ceiling": `{"state":"OFF","brightness":150}`,
	})

	eventJSON := app.buildLightEvent("Upstairs", LightConfig{Name: "Bedroom", Entities: []string{"bed", "ceiling"}})

	var event map[string]any
	json.Unmarshal([]byte(eventJSON), &event)

	if event["on"] != false {
		t.Errorf("on = %v, want false", event["on"])
	}
	if event["brightness"] != 200.0 {
		t.Errorf("brightness = %v, want 200", event["brightness"])
	}
	if event["brightness_on"] != false {
		t.Errorf("brightness_on = %v, want false", event["brightness_on"])
	}
}

// TestBuildLightEvent_MixedBrightness verifies that when a group has some
// entities with brightness and some without, brightness is only tracked for
// entities that report it, and brightness_on reflects only those entities.
func TestBuildLightEvent_MixedBrightness(t *testing.T) {
	app := testApp(map[string]string{
		"zigbee2mqtt/bed":     `{"state":"ON"}`,              // no brightness support
		"zigbee2mqtt/ceiling": `{"state":"OFF","brightness":180}`, // brightness, but OFF
	})

	eventJSON := app.buildLightEvent("Upstairs", LightConfig{Name: "Bedroom", Entities: []string{"bed", "ceiling"}})

	var event map[string]any
	json.Unmarshal([]byte(eventJSON), &event)

	// Group is ON because bed is on (even though it has no brightness)
	if event["on"] != true {
		t.Errorf("on = %v, want true", event["on"])
	}
	// brightness from ceiling entity
	if event["brightness"] != 180.0 {
		t.Errorf("brightness = %v, want 180", event["brightness"])
	}
	// brightness_on is false because the only brightness-capable entity (ceiling) is OFF
	if event["brightness_on"] != false {
		t.Errorf("brightness_on = %v, want false", event["brightness_on"])
	}
}

// TestBuildLightEvent_NoBrightness verifies brightness and brightness_on are
// omitted when entities don't report brightness.
func TestBuildLightEvent_NoBrightness(t *testing.T) {
	app := testApp(map[string]string{
		"zigbee2mqtt/bed": `{"state":"ON"}`,
	})

	eventJSON := app.buildLightEvent("Upstairs", LightConfig{Name: "Bedroom", Entities: []string{"bed"}})

	var event map[string]any
	json.Unmarshal([]byte(eventJSON), &event)

	if _, hasBrightness := event["brightness"]; hasBrightness {
		t.Errorf("brightness should be omitted for lights that don't support it, got %v", event["brightness"])
	}
	if _, hasBrightnessOn := event["brightness_on"]; hasBrightnessOn {
		t.Errorf("brightness_on should be omitted for lights that don't support brightness")
	}
}

// TestHandleLightBrightness_NotFound tests 404 for unknown light.
func TestHandleLightBrightness_NotFound(t *testing.T) {
	app := testApp(nil)

	mux := http.NewServeMux()
	app.SetupRoutes(mux)

	body := `{"value": 128}`
	req := httptest.NewRequest("POST", "/api/light/Upstairs/NoSuchLight/brightness",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 404 {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// TestHandleLightBrightness_BadValue tests 400 for non-numeric brightness.
func TestHandleLightBrightness_BadValue(t *testing.T) {
	app := testApp(nil)

	mux := http.NewServeMux()
	app.SetupRoutes(mux)

	body := `{"value": "bright"}`
	req := httptest.NewRequest("POST", "/api/light/Upstairs/Bedroom/brightness",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 400 {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// TestHandleLightBrightness_OutOfRange tests 400 for out-of-range brightness.
func TestHandleLightBrightness_OutOfRange(t *testing.T) {
	app := testApp(nil)

	mux := http.NewServeMux()
	app.SetupRoutes(mux)

	body := `{"value": 300}`
	req := httptest.NewRequest("POST", "/api/light/Upstairs/Bedroom/brightness",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 400 {
		t.Errorf("status = %d, want 400", w.Code)
	}
}
