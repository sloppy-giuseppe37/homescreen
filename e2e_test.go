package main

// e2e_test.go — End-to-end tests that run the full server and verify
// behaviour through the HTTP API + SSE, simulating what a browser does.
//
// These tests use a real MQTT broker (localhost:1883) and a real HTTP
// server (httptest.Server). They verify the complete flow:
//
//   User action → POST to API → MQTT publish → MQTT message received
//   → SSE event broadcast → all clients updated
//
// Unlike the browser-based e2e tests, these don't need a real browser.
// They test the same flows using Go's HTTP client.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// e2eSetup starts a full server with MQTT and returns the server URL
// and a cleanup function.
func e2eSetup(t *testing.T) (string, *App, func()) {
	t.Helper()
	skipIfNoMQTT(t)

	app, mqttCleanup := integrationApp(t)

	mux := http.NewServeMux()
	app.SetupRoutes(mux)
	server := httptest.NewServer(mux)

	cleanup := func() {
		server.Close()
		mqttCleanup()
	}

	return server.URL, app, cleanup
}

// sseReader connects to the SSE endpoint and returns a channel of parsed events.
func sseReader(t *testing.T, baseURL string) (chan map[string]any, func()) {
	t.Helper()

	resp, err := http.Get(baseURL + "/api/events")
	if err != nil {
		t.Fatalf("SSE connect: %v", err)
	}

	ch := make(chan map[string]any, 64)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data: ") {
				var event map[string]any
				if err := json.Unmarshal([]byte(line[6:]), &event); err == nil {
					ch <- event
				}
			}
		}
	}()

	return ch, func() { resp.Body.Close() }
}

// waitForEvent reads events from the channel until one matches the predicate,
// or times out.
func waitForEvent(ch chan map[string]any, timeout time.Duration, match func(map[string]any) bool) (map[string]any, bool) {
	deadline := time.After(timeout)
	for {
		select {
		case event := <-ch:
			if match(event) {
				return event, true
			}
		case <-deadline:
			return nil, false
		}
	}
}

// postJSON sends a POST request with a JSON body.
func postJSON(baseURL, path string, value any) (*http.Response, error) {
	body, _ := json.Marshal(map[string]any{"value": value})
	return http.Post(baseURL+path, "application/json", bytes.NewReader(body))
}

// clearRetained publishes empty retained messages to clean up after tests.
func clearRetained(t *testing.T, app *App) {
	t.Helper()
	for _, zone := range app.Config.Zones {
		for _, room := range zone.Heating {
			power, temp, quiet := room.HeatingTopics()
			for _, topic := range []string{power, temp, quiet} {
				app.MQTT.client.Publish(topic, 1, true, []byte{}) // empty retained
			}
		}
		for _, light := range zone.Lights {
			app.MQTT.client.Publish(light.Topic, 1, true, []byte{})
		}
	}
	time.Sleep(100 * time.Millisecond)
}

// ---------- E2E Tests ----------

// TestE2E_FullHeatingFlow tests the complete flow of a user:
// 1. Connect SSE and get initial snapshot
// 2. Turn on a room
// 3. Set zone temperature
// 4. Enable quiet mode
// 5. Verify all SSE events arrive
func TestE2E_FullHeatingFlow(t *testing.T) {
	baseURL, app, cleanup := e2eSetup(t)
	defer cleanup()
	defer clearRetained(t, app)

	// Step 1: Connect SSE client
	events, closeSSE := sseReader(t, baseURL)
	defer closeSSE()

	// Wait for snapshot (should include heating events for all rooms)
	snapshotCount := 0
	for snapshotCount < 4 { // 2 heating + 1 light upstairs, 1 heating + 1 light downstairs = 5
		select {
		case <-events:
			snapshotCount++
		case <-time.After(3 * time.Second):
			t.Fatalf("snapshot incomplete: got %d events", snapshotCount)
		}
	}
	// Drain any remaining snapshot events
	time.Sleep(200 * time.Millisecond)
	for len(events) > 0 {
		<-events
	}

	// Step 2: Turn on Bedroom
	resp, err := postJSON(baseURL, "/api/heating/room/Upstairs/Bedroom/power", true)
	if err != nil {
		t.Fatalf("POST room power: %v", err)
	}
	if resp.StatusCode != 204 {
		t.Fatalf("POST room power status = %d", resp.StatusCode)
	}

	// Verify SSE event arrives
	event, ok := waitForEvent(events, 3*time.Second, func(e map[string]any) bool {
		return e["type"] == "heating" && e["room"] == "Bedroom" && e["power"] == true
	})
	if !ok {
		t.Fatal("did not receive Bedroom power=true event")
	}
	t.Logf("Bedroom power event: %v", event)

	// Step 3: Set zone temperature to 24
	resp, err = postJSON(baseURL, "/api/heating/zone/Upstairs/temperature", 24)
	if err != nil {
		t.Fatalf("POST zone temp: %v", err)
	}
	if resp.StatusCode != 204 {
		t.Fatalf("POST zone temp status = %d", resp.StatusCode)
	}

	// Should get events for BOTH Bedroom and Guest Room
	gotBedroom := false
	gotGuest := false
	for i := 0; i < 10 && !(gotBedroom && gotGuest); i++ {
		event, ok := waitForEvent(events, 2*time.Second, func(e map[string]any) bool {
			return e["type"] == "heating" && e["target_temp"] == 24.0
		})
		if !ok {
			break
		}
		if event["room"] == "Bedroom" {
			gotBedroom = true
		}
		if event["room"] == "Guest Room" {
			gotGuest = true
		}
	}
	if !gotBedroom || !gotGuest {
		t.Errorf("zone temp: bedroom=%v guest=%v, want both true", gotBedroom, gotGuest)
	}

	// Step 4: Enable quiet mode
	resp, err = postJSON(baseURL, "/api/heating/zone/Upstairs/quiet", true)
	if err != nil {
		t.Fatalf("POST zone quiet: %v", err)
	}
	if resp.StatusCode != 204 {
		t.Fatalf("POST zone quiet status = %d", resp.StatusCode)
	}

	// Verify quiet events for both rooms
	gotBedroom = false
	gotGuest = false
	for i := 0; i < 10 && !(gotBedroom && gotGuest); i++ {
		event, ok := waitForEvent(events, 2*time.Second, func(e map[string]any) bool {
			return e["type"] == "heating" && e["quiet"] == true
		})
		if !ok {
			break
		}
		if event["room"] == "Bedroom" {
			gotBedroom = true
		}
		if event["room"] == "Guest Room" {
			gotGuest = true
		}
	}
	if !gotBedroom || !gotGuest {
		t.Errorf("zone quiet: bedroom=%v guest=%v, want both true", gotBedroom, gotGuest)
	}
}

// TestE2E_LightToggle tests the full light on/off flow.
func TestE2E_LightToggle(t *testing.T) {
	baseURL, app, cleanup := e2eSetup(t)
	defer cleanup()
	defer clearRetained(t, app)

	// Connect SSE
	events, closeSSE := sseReader(t, baseURL)
	defer closeSSE()

	// Wait for snapshot
	time.Sleep(500 * time.Millisecond)
	for len(events) > 0 {
		<-events
	}

	// Turn on light
	resp, err := postJSON(baseURL, "/api/light/Upstairs/Bedroom/power", true)
	if err != nil {
		t.Fatalf("POST light: %v", err)
	}
	if resp.StatusCode != 204 {
		t.Fatalf("POST light status = %d", resp.StatusCode)
	}

	// Verify SSE event
	event, ok := waitForEvent(events, 3*time.Second, func(e map[string]any) bool {
		return e["type"] == "light" && e["name"] == "Bedroom" && e["on"] == true
	})
	if !ok {
		t.Fatal("did not receive light on event")
	}
	t.Logf("Light on event: %v", event)

	// Turn off
	resp, err = postJSON(baseURL, "/api/light/Upstairs/Bedroom/power", false)
	if err != nil {
		t.Fatalf("POST light off: %v", err)
	}
	if resp.StatusCode != 204 {
		t.Fatalf("POST light off status = %d", resp.StatusCode)
	}

	event, ok = waitForEvent(events, 3*time.Second, func(e map[string]any) bool {
		return e["type"] == "light" && e["name"] == "Bedroom" && e["on"] == false
	})
	if !ok {
		t.Fatal("did not receive light off event")
	}
	t.Logf("Light off event: %v", event)
}

// TestE2E_MultiClientSync tests that two clients both see the same
// state changes, simulating two people using the app simultaneously.
func TestE2E_MultiClientSync(t *testing.T) {
	baseURL, app, cleanup := e2eSetup(t)
	defer cleanup()
	defer clearRetained(t, app)

	// Connect two SSE clients (like two browser tabs)
	ch1, close1 := sseReader(t, baseURL)
	ch2, close2 := sseReader(t, baseURL)
	defer close1()
	defer close2()

	// Wait for snapshots to complete
	time.Sleep(500 * time.Millisecond)
	for len(ch1) > 0 {
		<-ch1
	}
	for len(ch2) > 0 {
		<-ch2
	}

	// Client 1 triggers a change (in reality, this is a browser POST)
	resp, err := postJSON(baseURL, "/api/heating/zone/Downstairs/temperature", 18)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if resp.StatusCode != 204 {
		t.Fatalf("POST status = %d", resp.StatusCode)
	}

	// Both clients should receive the update
	for i, ch := range []chan map[string]any{ch1, ch2} {
		event, ok := waitForEvent(ch, 3*time.Second, func(e map[string]any) bool {
			return e["type"] == "heating" && e["zone"] == "Downstairs" && e["target_temp"] == 18.0
		})
		if !ok {
			t.Errorf("client %d: did not receive temp update", i+1)
		} else {
			t.Logf("client %d: got event %v", i+1, event)
		}
	}
}

// TestE2E_ExternalMQTTChange tests that a change made directly to MQTT
// (not through our API) is still pushed to SSE clients.
// This simulates another system (e.g. a physical thermostat) changing a value.
func TestE2E_ExternalMQTTChange(t *testing.T) {
	baseURL, app, cleanup := e2eSetup(t)
	defer cleanup()
	defer clearRetained(t, app)

	// Connect SSE client
	events, closeSSE := sseReader(t, baseURL)
	defer closeSSE()

	// Wait for snapshot
	time.Sleep(500 * time.Millisecond)
	for len(events) > 0 {
		<-events
	}

	// Publish directly to MQTT (simulating an external device)
	topic := "HomeKit/KitchenFaikin_Thermostat/Thermostat/TargetTemperature"
	err := app.MQTT.Publish(topic, "19.5")
	if err != nil {
		t.Fatalf("MQTT publish: %v", err)
	}

	// SSE client should receive the update
	event, ok := waitForEvent(events, 3*time.Second, func(e map[string]any) bool {
		return e["type"] == "heating" && e["room"] == "Kitchen" && e["target_temp"] == 19.5
	})
	if !ok {
		t.Fatal("did not receive external MQTT change via SSE")
	}
	t.Logf("External MQTT event: %v", event)
}

// TestE2E_IndexPage tests that the HTML page is served correctly
// with all expected elements.
func TestE2E_IndexPage(t *testing.T) {
	baseURL, _, cleanup := e2eSetup(t)
	defer cleanup()

	resp, err := http.Get(baseURL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	// Read the full body
	buf := new(bytes.Buffer)
	buf.ReadFrom(resp.Body)
	body := buf.String()

	// Check for key UI elements
	checks := []string{
		"Home Control",       // page title
		"data-tab=\"lights\"",  // lights tab
		"data-tab=\"heating\"", // heating tab
		"data-temp-zone",      // temperature slider
		"data-quiet-zone",     // quiet toggle
		"data-room",           // room power toggle
		"data-light",          // light toggle
		"connectSSE",          // SSE connection function
		"EventSource",         // SSE API usage
		"status-dot",          // connection indicator
		"Upstairs",            // zone from config
		"Downstairs",          // zone from config
		"Bedroom",             // room from config
		"Kitchen",             // room from config
	}

	for _, check := range checks {
		if !strings.Contains(body, check) {
			t.Errorf("page missing: %q", check)
		}
	}
}

// TestE2E_APIErrorHandling tests various error cases.
func TestE2E_APIErrorHandling(t *testing.T) {
	baseURL, _, cleanup := e2eSetup(t)
	defer cleanup()

	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		wantStatus int
	}{
		{"unknown zone", "POST", "/api/heating/zone/Mars/temperature", `{"value":20}`, 404},
		{"unknown room", "POST", "/api/heating/room/Upstairs/Garage/power", `{"value":true}`, 404},
		{"unknown light", "POST", "/api/light/Upstairs/Disco/power", `{"value":true}`, 404},
		{"bad json", "POST", "/api/heating/zone/Upstairs/temperature", "not json", 400},
		{"non-numeric temp", "POST", "/api/heating/zone/Upstairs/temperature", `{"value":"hot"}`, 400},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(tc.method, baseURL+tc.path,
				bytes.NewBufferString(tc.body))
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
		})
	}
}

// TestE2E_TemperatureFormatting tests that temperatures are correctly
// formatted with one decimal place in MQTT.
func TestE2E_TemperatureFormatting(t *testing.T) {
	baseURL, app, cleanup := e2eSetup(t)
	defer cleanup()
	defer clearRetained(t, app)

	// Set temperature to integer value
	resp, err := postJSON(baseURL, "/api/heating/zone/Upstairs/temperature", 21)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if resp.StatusCode != 204 {
		t.Fatalf("POST status = %d", resp.StatusCode)
	}

	time.Sleep(300 * time.Millisecond)

	// Check MQTT cache — should be formatted as "21.0"
	topic := "HomeKit/BedroomFaikin_Thermostat/Thermostat/TargetTemperature"
	val, ok := app.MQTT.GetValue(topic)
	if !ok {
		t.Fatal("temp not in cache")
	}
	if val != "21.0" {
		t.Errorf("cached temp = %q, want %q", val, "21.0")
	}

	// Also test with a float
	resp, err = postJSON(baseURL, "/api/heating/zone/Upstairs/temperature", 21.5)
	if err != nil {
		t.Fatalf("POST float: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	val, _ = app.MQTT.GetValue(topic)
	if val != "21.5" {
		t.Errorf("cached float temp = %q, want %q", val, "21.5")
	}
}

// TestE2E_SSEReconnectGetsSnapshot tests that when a client disconnects
// and reconnects, it gets the current state (not just future changes).
func TestE2E_SSEReconnectGetsSnapshot(t *testing.T) {
	baseURL, app, cleanup := e2eSetup(t)
	defer cleanup()
	defer clearRetained(t, app)

	// Set some state first
	resp, _ := postJSON(baseURL, "/api/heating/room/Upstairs/Bedroom/power", true)
	if resp.StatusCode != 204 {
		t.Fatalf("POST status = %d", resp.StatusCode)
	}
	resp, _ = postJSON(baseURL, "/api/heating/zone/Upstairs/temperature", 23)
	if resp.StatusCode != 204 {
		t.Fatalf("POST status = %d", resp.StatusCode)
	}
	time.Sleep(300 * time.Millisecond)

	// Now connect a NEW client (simulating reconnect)
	events, closeSSE := sseReader(t, baseURL)
	defer closeSSE()

	// The snapshot should contain the current state
	event, ok := waitForEvent(events, 3*time.Second, func(e map[string]any) bool {
		return e["type"] == "heating" && e["room"] == "Bedroom" &&
			e["power"] == true && e["target_temp"] == 23.0
	})
	if !ok {
		t.Fatal("reconnected client did not receive current state in snapshot")
	}
	fmt.Sprintf("%v", event) // prevent unused warning
}
