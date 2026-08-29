package main

// status_test.go — the /status page. Its whole reason to exist is answering
// when the rest of the app can't, so that is what these check hardest.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestHandleStatus_WorksWhileMQTTDown is the case the page exists for: every
// other route 503s when the broker is away, and this one still has to render.
func TestHandleStatus_WorksWhileMQTTDown(t *testing.T) {
	app := testAppDisconnected()

	req := httptest.NewRequest("GET", "/status", nil)
	w := httptest.NewRecorder()
	app.handleStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"disconnected", "MQTT", "Goroutines"} {
		if !strings.Contains(body, want) {
			t.Errorf("status page is missing %q", want)
		}
	}
	if strings.Contains(body, "connected</span>") && !strings.Contains(body, "disconnected</span>") {
		t.Error("status page reports a connection that isn't there")
	}
}

// TestHandleStatus_ReportsConnected checks the healthy rendering too.
func TestHandleStatus_ReportsConnected(t *testing.T) {
	app := testApp(map[string]string{"some/topic": "1"})

	req := httptest.NewRequest("GET", "/status", nil)
	w := httptest.NewRecorder()
	app.handleStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), ">connected<") {
		t.Error("status page does not report the connection as up")
	}
}

// TestHandleStatusJSON verifies the machine-readable form, which is what a
// monitoring check or a quick curl would use.
func TestHandleStatusJSON(t *testing.T) {
	app := testApp(map[string]string{"a/b": "1", "c/d": "2"})

	req := httptest.NewRequest("GET", "/status.json", nil)
	w := httptest.NewRecorder()
	app.handleStatusJSON(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var got StatusData
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("status.json is not valid JSON: %v", err)
	}
	if got.Version == "" {
		t.Error("version is empty")
	}
	if !got.MQTT.Connected {
		t.Error("mqtt.connected should be true for the test client")
	}
	if got.MQTT.CachedTopics != 2 {
		t.Errorf("cached_topics = %d, want 2", got.MQTT.CachedTopics)
	}
	if got.Zones != len(testConfig().Zones) {
		t.Errorf("zones = %d, want %d", got.Zones, len(testConfig().Zones))
	}
	if got.Rooms != 3 || got.Lights != 2 {
		t.Errorf("heating_rooms/lights = %d/%d, want 3/2", got.Rooms, got.Lights)
	}
	if got.StartedAt.IsZero() {
		t.Error("started_at is zero")
	}
}

// TestStatusRoutesRegistered guards the wiring: the page is useless if it is
// behind the MQTT check that the rest of the routes sit behind.
func TestStatusRoutesRegistered(t *testing.T) {
	app := testAppDisconnected()
	mux := http.NewServeMux()
	app.SetupRoutes(mux)

	for _, path := range []string{"/status", "/status.json"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != http.StatusOK {
			t.Errorf("GET %s with MQTT down: got %d, want 200", path, w.Code)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{5 * time.Second, "5s"},
		{90 * time.Second, "1m 30s"},
		{2*time.Hour + 5*time.Minute, "2h 5m"},
		{50 * time.Hour, "2d 2h"},
	}
	for _, c := range cases {
		if got := humanDuration(c.in); got != c.want {
			t.Errorf("humanDuration(%s) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestAgo covers the "nothing has happened yet" rendering, which is what a
// freshly started process with no broker shows.
func TestAgo(t *testing.T) {
	if got := ago(nil); got != "never" {
		t.Errorf("ago(nil) = %q, want %q", got, "never")
	}
	if got := ago(&time.Time{}); got != "never" {
		t.Errorf("ago(zero) = %q, want %q", got, "never")
	}
	past := time.Now().Add(-90 * time.Second)
	if got := ago(&past); got != "1m 30s ago" {
		t.Errorf("ago(90s back) = %q, want %q", got, "1m 30s ago")
	}
}
