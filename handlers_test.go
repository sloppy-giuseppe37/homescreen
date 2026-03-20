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
// By default, the fake client reports as connected.
func fakeMQTTClient(cache map[string]string) *MQTTClient {
	return fakeMQTTClientWithConnected(cache, true)
}

// fakeMQTTClientWithConnected creates a fake MQTT client with explicit connected state.
func fakeMQTTClientWithConnected(cache map[string]string, connected bool) *MQTTClient {
	m := &MQTTClient{
		cache:     make(map[string]string),
		connected: connected,
	}
	for k, v := range cache {
		m.cache[k] = v
	}
	return m
}

// testApp creates an App wired up for testing (no real MQTT connection).
func testApp(cache map[string]string) *App {
	return testAppWithConfig(testConfig(), cache)
}

func testAppWithConfig(cfg *Config, cache map[string]string) *App {
	tmpl := template.Must(template.ParseFiles("templates/index.html"))
	return &App{
		Config:      cfg,
		MQTT:        fakeMQTTClient(cache),
		Broadcaster: NewSSEBroadcaster(),
		Template:    tmpl,
	}
}

// testAppDisconnected creates an App where MQTT reports as disconnected.
func testAppDisconnected() *App {
	tmpl := template.Must(template.ParseFiles("templates/index.html"))
	return &App{
		Config:      testConfig(),
		MQTT:        fakeMQTTClientWithConnected(nil, false),
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

	// We have 1 mode + 3 heating rooms + 2 lights = 6 events
	if len(events) != 6 {
		t.Errorf("snapshot has %d events, want 6", len(events))
	}

	// First event should be the mode event
	if events[0]["type"] != "mode" {
		t.Errorf("first event type = %v, want \"mode\"", events[0]["type"])
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

// TestBuildLightEvent_UnavailableEntity verifies that an unavailable entity
// is treated as OFF — it doesn't contribute to the group's on state.
func TestBuildLightEvent_UnavailableEntity(t *testing.T) {
	app := testApp(map[string]string{
		"zigbee2mqtt/bed":                  `{"state":"ON"}`,
		"zigbee2mqtt/bed/availability":      `{"state":"offline"}`,
		"zigbee2mqtt/ceiling":               `{"state":"OFF"}`,
		"zigbee2mqtt/ceiling/availability":   `{"state":"online"}`,
	})

	eventJSON := app.buildLightEvent("Upstairs", LightConfig{Name: "Bedroom", Entities: []string{"bed", "ceiling"}})

	var event map[string]any
	json.Unmarshal([]byte(eventJSON), &event)

	// bed is ON but unavailable → skipped; ceiling is OFF and available → group is OFF
	if event["on"] != false {
		t.Errorf("on = %v, want false (unavailable entity should be treated as off)", event["on"])
	}
}

// TestBuildLightEvent_UnavailablePlainString verifies that the plain string
// "offline" (not JSON) is also recognized as unavailable.
func TestBuildLightEvent_UnavailablePlainString(t *testing.T) {
	app := testApp(map[string]string{
		"zigbee2mqtt/bed":                  `{"state":"ON","brightness":200}`,
		"zigbee2mqtt/bed/availability":      "offline",
	})

	eventJSON := app.buildLightEvent("Upstairs", LightConfig{Name: "Bedroom", Entities: []string{"bed"}})

	var event map[string]any
	json.Unmarshal([]byte(eventJSON), &event)

	if event["on"] != false {
		t.Errorf("on = %v, want false", event["on"])
	}
	// Brightness should not be reported for unavailable entity
	if _, has := event["brightness"]; has {
		t.Errorf("brightness should be omitted for unavailable entity, got %v", event["brightness"])
	}
}

// TestBuildLightEvent_MixedAvailability verifies correct behavior when a group
// has one available and one unavailable entity.
func TestBuildLightEvent_MixedAvailability(t *testing.T) {
	app := testApp(map[string]string{
		"zigbee2mqtt/bed":                  `{"state":"ON","brightness":200}`,
		"zigbee2mqtt/bed/availability":      `{"state":"online"}`,
		"zigbee2mqtt/ceiling":               `{"state":"ON","brightness":150}`,
		"zigbee2mqtt/ceiling/availability":   `{"state":"offline"}`,
	})

	eventJSON := app.buildLightEvent("Upstairs", LightConfig{Name: "Bedroom", Entities: []string{"bed", "ceiling"}})

	var event map[string]any
	json.Unmarshal([]byte(eventJSON), &event)

	// bed is available and ON → group is ON
	if event["on"] != true {
		t.Errorf("on = %v, want true", event["on"])
	}
	// Only bed's brightness counts (ceiling is unavailable)
	if event["brightness"] != 200.0 {
		t.Errorf("brightness = %v, want 200 (only available entity)", event["brightness"])
	}
	if event["brightness_on"] != true {
		t.Errorf("brightness_on = %v, want true", event["brightness_on"])
	}
}

// TestBuildLightEvent_NoAvailabilityMessage verifies that entities with no
// availability message in the cache are assumed available (default behavior).
func TestBuildLightEvent_NoAvailabilityMessage(t *testing.T) {
	app := testApp(map[string]string{
		"zigbee2mqtt/bed": `{"state":"ON"}`,
		// No availability topic cached for bed
	})

	eventJSON := app.buildLightEvent("Upstairs", LightConfig{Name: "Bedroom", Entities: []string{"bed"}})

	var event map[string]any
	json.Unmarshal([]byte(eventJSON), &event)

	// No availability info → assumed available → ON
	if event["on"] != true {
		t.Errorf("on = %v, want true (no availability message should mean available)", event["on"])
	}
}

// TestTopicToEvent_Availability verifies that an availability topic change
// triggers a light event re-emission.
func TestTopicToEvent_Availability(t *testing.T) {
	app := testApp(map[string]string{
		"zigbee2mqtt/bed":                  `{"state":"ON"}`,
		"zigbee2mqtt/bed/availability":      `{"state":"offline"}`,
	})

	// An availability change for "bed" should produce a light event
	eventJSON := app.TopicToEvent("zigbee2mqtt/bed/availability", `{"state":"offline"}`)
	if eventJSON == "" {
		t.Fatal("TopicToEvent returned empty for availability topic")
	}

	var event map[string]any
	json.Unmarshal([]byte(eventJSON), &event)

	if event["type"] != "light" {
		t.Errorf("type = %v, want 'light'", event["type"])
	}
	if event["on"] != false {
		t.Errorf("on = %v, want false (entity is offline)", event["on"])
	}
}

// TestBuildLightEvent_AllUnavailable verifies that when all entities are
// unavailable, the light group reports as OFF.
func TestBuildLightEvent_AllUnavailable(t *testing.T) {
	app := testApp(map[string]string{
		"zigbee2mqtt/bed":                  `{"state":"ON","brightness":200}`,
		"zigbee2mqtt/bed/availability":      `{"state":"offline"}`,
		"zigbee2mqtt/ceiling":               `{"state":"ON","brightness":150}`,
		"zigbee2mqtt/ceiling/availability":   `{"state":"offline"}`,
	})

	eventJSON := app.buildLightEvent("Upstairs", LightConfig{Name: "Bedroom", Entities: []string{"bed", "ceiling"}})

	var event map[string]any
	json.Unmarshal([]byte(eventJSON), &event)

	if event["on"] != false {
		t.Errorf("on = %v, want false (all entities unavailable)", event["on"])
	}
	if _, has := event["brightness"]; has {
		t.Errorf("brightness should be omitted when all entities unavailable")
	}
}

// TestHandleIndex_SecretZone verifies that secret zones get the secret-zone
// CSS class in rendered HTML, while non-secret zones do not.
func TestHandleIndex_SecretZone(t *testing.T) {
	cfg := &Config{
		MQTT: MQTTConfig{Broker: "tcp://localhost:1883", TopicPrefix: "zigbee2mqtt"},
		Zones: []ZoneConfig{
			{
				Name:   "Public",
				Secret: false,
				Heating: []HeatingRoom{{Name: "Room1", UnitID: "Room1Faikin"}},
			},
			{
				Name:   "SecretLair",
				Secret: true,
				Heating: []HeatingRoom{{Name: "Bunker", UnitID: "BunkerFaikin"}},
				Lights:  []LightConfig{{Name: "HiddenLight", Entities: []string{"hidden1"}}},
			},
		},
	}
	app := testAppWithConfig(cfg, nil)

	mux := http.NewServeMux()
	app.SetupRoutes(mux)

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	body := w.Body.String()

	// Secret zone should have secret-zone class
	if !strings.Contains(body, `class="card secret-zone"`) && !strings.Contains(body, `class="card light-card secret-zone"`) {
		t.Error("secret zone missing 'secret-zone' CSS class")
	}

	// Public zone should NOT have secret-zone class (check it has data-heating-zone but no secret-zone)
	if strings.Contains(body, `data-heating-zone="Public"`) {
		// Find the card div for Public - it should not have secret-zone
		if strings.Contains(body, `class="card secret-zone" data-heating-zone="Public"`) {
			t.Error("non-secret zone has 'secret-zone' CSS class")
		}
	}

	// Zone names should be present in the HTML
	if !strings.Contains(body, "Public") {
		t.Error("Public zone name missing from rendered HTML")
	}
	if !strings.Contains(body, "SecretLair") {
		t.Error("SecretLair zone name missing from rendered HTML")
	}
}

// TestHandleIndex_SecretZoneLightsAndHeating verifies both light and heating
// cards get the secret-zone class for secret zones.
func TestHandleIndex_SecretZoneLightsAndHeating(t *testing.T) {
	cfg := &Config{
		MQTT: MQTTConfig{Broker: "tcp://localhost:1883", TopicPrefix: "zigbee2mqtt"},
		Zones: []ZoneConfig{
			{
				Name:    "HiddenZone",
				Secret:  true,
				Heating: []HeatingRoom{{Name: "SecretRoom", UnitID: "SecretFaikin"}},
				Lights:  []LightConfig{{Name: "SecretLight", Entities: []string{"secret_bulb"}}},
			},
		},
	}
	app := testAppWithConfig(cfg, nil)

	mux := http.NewServeMux()
	app.SetupRoutes(mux)

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()

	// Count occurrences of secret-zone class - should be 2 (one for heating, one for lights)
	count := strings.Count(body, "secret-zone")
	if count < 2 {
		t.Errorf("expected at least 2 occurrences of 'secret-zone' class (heating + lights), got %d", count)
	}
}

// ---------- MQTT disconnected (503) tests ----------

// TestHandleIndex_503WhenMQTTDisconnected verifies that GET / returns 503
// when MQTT is disconnected.
func TestHandleIndex_503WhenMQTTDisconnected(t *testing.T) {
	app := testAppDisconnected()

	mux := http.NewServeMux()
	app.SetupRoutes(mux)

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 503 {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

// TestHandleSSE_503WhenMQTTDisconnected verifies that GET /api/events returns 503
// when MQTT is disconnected.
func TestHandleSSE_503WhenMQTTDisconnected(t *testing.T) {
	app := testAppDisconnected()

	mux := http.NewServeMux()
	app.SetupRoutes(mux)

	req := httptest.NewRequest("GET", "/api/events", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 503 {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

// TestHandleZoneTemperature_503WhenMQTTDisconnected verifies POST returns 503.
func TestHandleZoneTemperature_503WhenMQTTDisconnected(t *testing.T) {
	app := testAppDisconnected()

	mux := http.NewServeMux()
	app.SetupRoutes(mux)

	body := `{"value": 21}`
	req := httptest.NewRequest("POST", "/api/heating/zone/Upstairs/temperature",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 503 {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

// TestHandleZoneQuiet_503WhenMQTTDisconnected verifies POST returns 503.
func TestHandleZoneQuiet_503WhenMQTTDisconnected(t *testing.T) {
	app := testAppDisconnected()

	mux := http.NewServeMux()
	app.SetupRoutes(mux)

	body := `{"value": true}`
	req := httptest.NewRequest("POST", "/api/heating/zone/Upstairs/quiet",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 503 {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

// TestHandleRoomPower_503WhenMQTTDisconnected verifies POST returns 503.
func TestHandleRoomPower_503WhenMQTTDisconnected(t *testing.T) {
	app := testAppDisconnected()

	mux := http.NewServeMux()
	app.SetupRoutes(mux)

	body := `{"value": true}`
	req := httptest.NewRequest("POST", "/api/heating/room/Upstairs/Bedroom/power",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 503 {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

// TestHandleLightPower_503WhenMQTTDisconnected verifies POST returns 503.
func TestHandleLightPower_503WhenMQTTDisconnected(t *testing.T) {
	app := testAppDisconnected()

	mux := http.NewServeMux()
	app.SetupRoutes(mux)

	body := `{"value": true}`
	req := httptest.NewRequest("POST", "/api/light/Upstairs/Bedroom/power",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 503 {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

// TestHandleLightBrightness_503WhenMQTTDisconnected verifies POST returns 503.
func TestHandleLightBrightness_503WhenMQTTDisconnected(t *testing.T) {
	app := testAppDisconnected()

	mux := http.NewServeMux()
	app.SetupRoutes(mux)

	body := `{"value": 128}`
	req := httptest.NewRequest("POST", "/api/light/Upstairs/Bedroom/brightness",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 503 {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

// TestHandleIndex_200WhenMQTTConnected verifies normal operation when connected.
func TestHandleIndex_200WhenMQTTConnected(t *testing.T) {
	app := testApp(nil) // default is connected

	mux := http.NewServeMux()
	app.SetupRoutes(mux)

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

// ---------- Cooling mode tests ----------

// TestGetMode_DefaultHeating verifies the default mode is "heating".
func TestGetMode_DefaultHeating(t *testing.T) {
	app := testApp(nil)

	mux := http.NewServeMux()
	app.SetupRoutes(mux)

	req := httptest.NewRequest("GET", "/api/mode", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["mode"] != "heating" {
		t.Errorf("mode = %q, want \"heating\"", resp["mode"])
	}
}

// TestSetMode_BadValue verifies invalid mode values are rejected.
func TestSetMode_BadValue(t *testing.T) {
	app := testApp(nil)

	mux := http.NewServeMux()
	app.SetupRoutes(mux)

	body := `{"mode": "turbo"}`
	req := httptest.NewRequest("POST", "/api/mode", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 400 {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// TestSetMode_NoChange verifies setting the same mode is a no-op 204.
func TestSetMode_NoChange(t *testing.T) {
	app := testApp(nil) // default is heating

	mux := http.NewServeMux()
	app.SetupRoutes(mux)

	body := `{"mode": "heating"}`
	req := httptest.NewRequest("POST", "/api/mode", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 204 {
		t.Errorf("status = %d, want 204 (no change)", w.Code)
	}
}

// TestSetMode_503WhenMQTTDisconnected verifies POST returns 503.
func TestSetMode_503WhenMQTTDisconnected(t *testing.T) {
	app := testAppDisconnected()

	mux := http.NewServeMux()
	app.SetupRoutes(mux)

	body := `{"mode": "cooling"}`
	req := httptest.NewRequest("POST", "/api/mode", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 503 {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

// TestBuildHeatingEvent_CoolPower verifies that power value "2" is
// recognised as powered on (cooling mode).
func TestBuildHeatingEvent_CoolPower(t *testing.T) {
	app := testApp(map[string]string{
		"HomeKit/BedroomFaikin_Thermostat/Thermostat/TargetHeatingCoolingState": "2",
		"HomeKit/BedroomFaikin_Thermostat/Thermostat/TargetTemperature":        "22.0",
		"HomeKit/BedroomFaikin_IndoorQuiet/Switch/On":                          "0",
	})

	eventJSON := app.buildHeatingEvent("Upstairs", app.Config.Zones[0].Heating[0])

	var event map[string]any
	json.Unmarshal([]byte(eventJSON), &event)

	if event["power"] != true {
		t.Errorf("power = %v, want true (cooling mode power=2 should be on)", event["power"])
	}
	if event["target_temp"] != 22.0 {
		t.Errorf("target_temp = %v, want 22.0", event["target_temp"])
	}
}

// TestTopicToEvent_ModeTopic verifies that the mode MQTT topic
// is converted to a mode SSE event and updates app state.
func TestTopicToEvent_ModeTopic(t *testing.T) {
	app := testApp(nil)

	if app.IsCoolingMode() {
		t.Fatal("expected heating mode initially")
	}

	eventJSON := app.TopicToEvent(ModeTopicName, "cooling")

	var event map[string]any
	json.Unmarshal([]byte(eventJSON), &event)

	if event["type"] != "mode" {
		t.Errorf("type = %v, want \"mode\"", event["type"])
	}
	if event["mode"] != "cooling" {
		t.Errorf("mode = %v, want \"cooling\"", event["mode"])
	}
	if !app.IsCoolingMode() {
		t.Error("expected app to be in cooling mode after TopicToEvent")
	}

	// Switch back to heating
	app.TopicToEvent(ModeTopicName, "heating")
	if app.IsCoolingMode() {
		t.Error("expected app to be in heating mode after TopicToEvent")
	}
}

// TestModeInSnapshot verifies that the snapshot includes a mode event.
func TestModeInSnapshot(t *testing.T) {
	app := testApp(nil)
	app.SetCoolingMode(true)

	snapshot := app.buildSnapshot()

	var events []map[string]any
	json.Unmarshal([]byte(snapshot), &events)

	if len(events) == 0 {
		t.Fatal("snapshot is empty")
	}

	// First event should be the mode event
	if events[0]["type"] != "mode" {
		t.Errorf("first event type = %v, want \"mode\"", events[0]["type"])
	}
	if events[0]["mode"] != "cooling" {
		t.Errorf("mode = %v, want \"cooling\"", events[0]["mode"])
	}
}

// TestHandleIndex_CoolingMode verifies the page renders with cooling-mode class.
func TestHandleIndex_CoolingMode(t *testing.T) {
	app := testApp(nil)
	app.SetCoolingMode(true)

	mux := http.NewServeMux()
	app.SetupRoutes(mux)

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, `class="cooling-mode"`) {
		t.Error("expected body to have cooling-mode class")
	}
	if !strings.Contains(body, "icon-snowflake") {
		t.Error("expected snowflake icon in cooling mode")
	}
	if !strings.Contains(body, ">Cooling</span>") {
		t.Error("expected 'Cooling' tab label in cooling mode")
	}
}

// TestHandleIndex_HeatingMode verifies the default page has no cooling-mode body class.
func TestHandleIndex_HeatingMode(t *testing.T) {
	app := testApp(nil)

	mux := http.NewServeMux()
	app.SetupRoutes(mux)

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	// The body tag should NOT have the cooling-mode class
	// (note: CSS rules reference "cooling-mode" as selectors, so check the body tag specifically)
	if strings.Contains(body, `class="cooling-mode"`) {
		t.Error("expected no cooling-mode class on body in default heating mode")
	}
	if !strings.Contains(body, "icon-flame") {
		t.Error("expected flame icon in heating mode")
	}
	if !strings.Contains(body, ">Heating</span>") {
		t.Error("expected 'Heating' tab label in heating mode")
	}
}
