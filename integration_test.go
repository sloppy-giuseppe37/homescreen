package main

// integration_test.go — Tests that use the real local Mosquitto broker.
//
// These tests verify the full flow:
//   Browser POST → server → MQTT publish → MQTT message arrives → SSE event
//
// They require Mosquitto running on localhost:1883.
// To skip them (e.g. in CI without Mosquitto), set: SKIP_INTEGRATION=1

import (
	"bufio"
	"bytes"
	"encoding/json"
	"text/template"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// skipIfNoMQTT skips the test if SKIP_INTEGRATION is set or
// if we can't connect to the local MQTT broker.
func skipIfNoMQTT(t *testing.T) {
	t.Helper()
	if os.Getenv("SKIP_INTEGRATION") != "" {
		t.Skip("SKIP_INTEGRATION is set")
	}
}

// integrationApp creates a fully wired App with a real MQTT connection.
// It returns the app and a cleanup function.
func integrationApp(t *testing.T) (*App, func()) {
	t.Helper()

	cfg := testConfig()
	cfg.MQTT.Broker = "tcp://localhost:1883"

	broadcaster := NewSSEBroadcaster()

	var app *App
	mqttClient, err := NewMQTTClient(cfg, func(topic, value string) {
		if app != nil {
			if eventJSON := app.TopicToEvent(topic, value); eventJSON != "" {
				broadcaster.Broadcast(eventJSON)
			}
		}
	}, nil)
	if err != nil {
		t.Fatalf("MQTT connect failed (is mosquitto running?): %v", err)
	}

	tmpl := template.Must(template.ParseFiles("templates/index.html"))

	app = &App{
		Config:      cfg,
		MQTT:        mqttClient,
		Broadcaster: broadcaster,
		Template:    tmpl,
	}

	// Give MQTT subscriptions a moment to complete
	time.Sleep(200 * time.Millisecond)

	cleanup := func() {
		mqttClient.Disconnect()
	}
	return app, cleanup
}

// TestIntegration_RoomPower tests the full flow:
//   POST /api/heating/room/.../power → MQTT publish → cache updated
func TestIntegration_RoomPower(t *testing.T) {
	skipIfNoMQTT(t)

	app, cleanup := integrationApp(t)
	defer cleanup()

	mux := http.NewServeMux()
	app.SetupRoutes(mux)

	// Turn on the Bedroom heater
	req := httptest.NewRequest("POST", "/api/heating/room/Upstairs/Bedroom/power",
		bytes.NewBufferString(`{"value": true}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 204 {
		t.Fatalf("POST status = %d, want 204. Body: %s", w.Code, w.Body.String())
	}

	// Wait for the MQTT message to round-trip back
	time.Sleep(300 * time.Millisecond)

	// Verify the value is in the cache
	powerTopic := "HomeKit/BedroomFaikin_Thermostat/Thermostat/TargetHeatingCoolingState"
	val, ok := app.MQTT.GetValue(powerTopic)
	if !ok {
		t.Fatal("power topic not in cache after publish")
	}
	if val != "1" {
		t.Errorf("cached power = %q, want %q", val, "1")
	}
}

// TestIntegration_ZoneTemperature tests setting temperature for a whole zone.
func TestIntegration_ZoneTemperature(t *testing.T) {
	skipIfNoMQTT(t)

	app, cleanup := integrationApp(t)
	defer cleanup()

	mux := http.NewServeMux()
	app.SetupRoutes(mux)

	// Set Upstairs to 22°C (should publish to both Bedroom and Guest Room)
	req := httptest.NewRequest("POST", "/api/heating/zone/Upstairs/temperature",
		bytes.NewBufferString(`{"value": 22}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 204 {
		t.Fatalf("POST status = %d, want 204", w.Code)
	}

	time.Sleep(300 * time.Millisecond)

	// Both rooms should have the temperature set
	for _, unitID := range []string{"BedroomFaikin", "GuestFaikin"} {
		topic := "HomeKit/" + unitID + "_Thermostat/Thermostat/TargetTemperature"
		val, ok := app.MQTT.GetValue(topic)
		if !ok {
			t.Errorf("%s: temp topic not in cache", unitID)
			continue
		}
		if val != "22.0" {
			t.Errorf("%s: cached temp = %q, want %q", unitID, val, "22.0")
		}
	}
}

// TestIntegration_ZoneQuiet tests setting quiet mode for a whole zone.
func TestIntegration_ZoneQuiet(t *testing.T) {
	skipIfNoMQTT(t)

	app, cleanup := integrationApp(t)
	defer cleanup()

	mux := http.NewServeMux()
	app.SetupRoutes(mux)

	req := httptest.NewRequest("POST", "/api/heating/zone/Upstairs/quiet",
		bytes.NewBufferString(`{"value": true}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 204 {
		t.Fatalf("POST status = %d, want 204", w.Code)
	}

	time.Sleep(300 * time.Millisecond)

	for _, unitID := range []string{"BedroomFaikin", "GuestFaikin"} {
		topic := "HomeKit/" + unitID + "_IndoorQuiet/Switch/On"
		val, ok := app.MQTT.GetValue(topic)
		if !ok {
			t.Errorf("%s: quiet topic not in cache", unitID)
			continue
		}
		if val != "1" {
			t.Errorf("%s: cached quiet = %q, want %q", unitID, val, "1")
		}
	}
}

// TestIntegration_LightPower tests toggling a light group.
// The handler publishes to zigbee2mqtt/{entity}/set topics.
// We verify the commands are published correctly by checking the cache
// for the /set topics (the broker retains them).
func TestIntegration_LightPower(t *testing.T) {
	skipIfNoMQTT(t)

	app, cleanup := integrationApp(t)
	defer cleanup()

	// Subscribe to /set topics so we can verify publishes
	prefix := app.Config.MQTT.TopicPrefix
	setTopics := []string{
		prefix + "/bed/set",
		prefix + "/ceiling/set",
	}
	for _, topic := range setTopics {
		t := topic
		app.MQTT.client.Subscribe(t, 1, func(_ mqtt.Client, msg mqtt.Message) {
			app.MQTT.cacheMu.Lock()
			app.MQTT.cache[t] = string(msg.Payload())
			app.MQTT.cacheMu.Unlock()
		}).Wait()
	}

	mux := http.NewServeMux()
	app.SetupRoutes(mux)

	req := httptest.NewRequest("POST", "/api/light/Upstairs/Bedroom/power",
		bytes.NewBufferString(`{"value": true}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 204 {
		t.Fatalf("POST status = %d, want 204", w.Code)
	}

	time.Sleep(300 * time.Millisecond)

	// Verify commands were published to both entities
	for _, topic := range setTopics {
		val, ok := app.MQTT.GetValue(topic)
		if !ok {
			t.Errorf("%s: not in cache", topic)
			continue
		}
		want := `{"state":"ON"}`
		if val != want {
			t.Errorf("%s = %q, want %q", topic, val, want)
		}
	}
}

// TestIntegration_LightStateFromMQTT tests that receiving zigbee2mqtt
// state messages updates the light state correctly.
func TestIntegration_LightStateFromMQTT(t *testing.T) {
	skipIfNoMQTT(t)

	app, cleanup := integrationApp(t)
	defer cleanup()

	prefix := app.Config.MQTT.TopicPrefix

	// Simulate zigbee2mqtt publishing state for one entity
	err := app.MQTT.Publish(prefix+"/bed", `{"state":"ON"}`)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	time.Sleep(300 * time.Millisecond)

	// Verify the cached value
	val, ok := app.MQTT.GetValue(prefix + "/bed")
	if !ok {
		t.Fatal("entity state not in cache")
	}
	if !strings.Contains(val, `"ON"`) {
		t.Errorf("cached state = %q, want ON", val)
	}
}

// TestIntegration_SSESnapshot tests that a new SSE client receives
// the full current state as its initial snapshot.
func TestIntegration_SSESnapshot(t *testing.T) {
	skipIfNoMQTT(t)

	app, cleanup := integrationApp(t)
	defer cleanup()

	mux := http.NewServeMux()
	app.SetupRoutes(mux)

	// First, set some state via the API
	req := httptest.NewRequest("POST", "/api/heating/room/Upstairs/Bedroom/power",
		bytes.NewBufferString(`{"value": true}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	time.Sleep(300 * time.Millisecond)

	// Now connect an SSE client and read the snapshot
	server := httptest.NewServer(mux)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/events")
	if err != nil {
		t.Fatalf("SSE connect failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", resp.Header.Get("Content-Type"))
	}

	// Read SSE events (each is "data: {json}\n\n")
	scanner := bufio.NewScanner(resp.Body)
	var events []map[string]any

	// Read events with a timeout
	done := make(chan bool, 1)
	go func() {
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data: ") {
				var event map[string]any
				json.Unmarshal([]byte(line[6:]), &event)
				events = append(events, event)
			}
			// After reading a few events, we have the snapshot
			if len(events) >= 4 { // 2 heating rooms + 1 light + 1 downstairs
				done <- true
				return
			}
		}
	}()

	select {
	case <-done:
		// Good
	case <-time.After(3 * time.Second):
		// We may have received some events, check what we have
	}

	if len(events) == 0 {
		t.Fatal("received no SSE events")
	}

	// Verify we got a heating event for Bedroom with power=true
	found := false
	for _, e := range events {
		if e["type"] == "heating" && e["room"] == "Bedroom" && e["power"] == true {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("snapshot missing Bedroom heating event with power=true. Got: %v", events)
	}
}

// TestIntegration_SSELiveUpdate tests that after connecting SSE,
// a subsequent API action is pushed as a live update.
func TestIntegration_SSELiveUpdate(t *testing.T) {
	skipIfNoMQTT(t)

	app, cleanup := integrationApp(t)
	defer cleanup()

	mux := http.NewServeMux()
	app.SetupRoutes(mux)

	server := httptest.NewServer(mux)
	defer server.Close()

	// Connect SSE client
	resp, err := http.Get(server.URL + "/api/events")
	if err != nil {
		t.Fatalf("SSE connect failed: %v", err)
	}
	defer resp.Body.Close()

	// Read events in a goroutine
	eventCh := make(chan map[string]any, 32)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data: ") {
				var event map[string]any
				json.Unmarshal([]byte(line[6:]), &event)
				eventCh <- event
			}
		}
	}()

	// Wait for snapshot events to arrive
	time.Sleep(500 * time.Millisecond)

	// Drain snapshot events
	for len(eventCh) > 0 {
		<-eventCh
	}

	// Now trigger a change via the API
	req := httptest.NewRequest("POST", "/api/heating/zone/Downstairs/temperature",
		bytes.NewBufferString(`{"value": 23}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	// Wait for the live update to arrive via SSE
	var liveEvent map[string]any
	select {
	case liveEvent = <-eventCh:
		// Got an event
	case <-time.After(3 * time.Second):
		t.Fatal("no live SSE event received within 3s")
	}

	if liveEvent["type"] != "heating" {
		t.Errorf("live event type = %v, want 'heating'", liveEvent["type"])
	}
	if liveEvent["zone"] != "Downstairs" {
		t.Errorf("live event zone = %v, want 'Downstairs'", liveEvent["zone"])
	}
	if liveEvent["target_temp"] != 23.0 {
		t.Errorf("live event target_temp = %v, want 23.0", liveEvent["target_temp"])
	}
}

// TestIntegration_MultipleClients tests that two SSE clients both
// receive updates when state changes.
func TestIntegration_MultipleClients(t *testing.T) {
	skipIfNoMQTT(t)

	app, cleanup := integrationApp(t)
	defer cleanup()

	mux := http.NewServeMux()
	app.SetupRoutes(mux)

	server := httptest.NewServer(mux)
	defer server.Close()

	// Connect two SSE clients
	readEvents := func() (chan map[string]any, func()) {
		resp, err := http.Get(server.URL + "/api/events")
		if err != nil {
			t.Fatalf("SSE connect: %v", err)
		}
		ch := make(chan map[string]any, 32)
		go func() {
			scanner := bufio.NewScanner(resp.Body)
			for scanner.Scan() {
				line := scanner.Text()
				if strings.HasPrefix(line, "data: ") {
					var event map[string]any
					json.Unmarshal([]byte(line[6:]), &event)
					ch <- event
				}
			}
		}()
		return ch, func() { resp.Body.Close() }
	}

	ch1, close1 := readEvents()
	ch2, close2 := readEvents()
	defer close1()
	defer close2()

	// Wait for snapshots
	time.Sleep(500 * time.Millisecond)
	for len(ch1) > 0 {
		<-ch1
	}
	for len(ch2) > 0 {
		<-ch2
	}

	// Trigger a change (use heating — lights need a zigbee2mqtt bridge for round-trip)
	req := httptest.NewRequest("POST", "/api/heating/room/Upstairs/Bedroom/power",
		bytes.NewBufferString(`{"value": true}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	// Both clients should receive the update
	for i, ch := range []chan map[string]any{ch1, ch2} {
		select {
		case event := <-ch:
			if event["type"] != "heating" {
				t.Errorf("client %d: type = %v, want 'heating'", i+1, event["type"])
			}
			if event["power"] != true {
				t.Errorf("client %d: power = %v, want true", i+1, event["power"])
			}
		case <-time.After(3 * time.Second):
			t.Errorf("client %d: no event received", i+1)
		}
	}
}
