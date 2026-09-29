package main

// api_test.go — GET /api and GET /api/state. The /api document is written for
// someone building a frontend without this repo in front of them, so the most
// important property is that it is true: every path it hands out must reach
// the handler and item it claims to.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// apiTestConfig has the awkward names real configs contain — spaces, an
// apostrophe, a non-ASCII dash, a slash — plus a secret zone, a zone with no
// heating, and scenes.
func apiTestConfig() *Config {
	cfg := testConfig()
	cfg.Zones = append(cfg.Zones,
		ZoneConfig{
			Name: "Garden",
			Lights: []LightConfig{
				{Name: "Kids' Room — Lamp", Entities: []string{"kids_lamp"}},
				{Name: "Path / Steps", Entities: []string{"path_lights"}},
			},
		},
		ZoneConfig{
			Name:    "Secret Lair",
			Secret:  true,
			Heating: []HeatingRoom{{Name: "Bunker", UnitID: "BunkerFaikin"}},
		},
	)
	cfg.Scenes = []SceneConfig{
		{Name: "Good Night", Description: "Everything off", Icon: "moon",
			Actions: []SceneAction{{Topic: "zigbee2mqtt/bed/set", Payload: `{"state":"OFF"}`}}},
	}
	return cfg
}

// getAPIDoc fetches /api through the real router and decodes it.
func getAPIDoc(t *testing.T, app *App) apiDoc {
	t.Helper()
	mux := http.NewServeMux()
	app.SetupRoutes(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var doc apiDoc
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("/api is not valid JSON: %v", err)
	}
	return doc
}

// allActions flattens every concrete request the document offers, labelled
// for error messages.
func allActions(doc apiDoc) map[string]apiAction {
	out := map[string]apiAction{}
	for name, a := range doc.Mode.Actions {
		out["mode."+name] = a
	}
	for _, z := range doc.Zones {
		if z.Heating != nil {
			for name, a := range z.Heating.Actions {
				out[z.Name+".heating."+name] = a
			}
			for _, r := range z.Heating.Rooms {
				for name, a := range r.Actions {
					out[z.Name+"/"+r.Name+"."+name] = a
				}
			}
		}
		for _, l := range z.Lights {
			for name, a := range l.Actions {
				out[z.Name+"/"+l.Name+"."+name] = a
			}
		}
	}
	for _, s := range doc.Scenes {
		for name, a := range s.Actions {
			out["scene "+s.Name+"."+name] = a
		}
	}
	return out
}

func TestAPIDoc_Inventory(t *testing.T) {
	app := testAppWithConfig(apiTestConfig(), map[string]string{
		"HomeKit/BedroomFaikin_Thermostat/Thermostat/TargetHeatingCoolingState": "1",
		"HomeKit/BedroomFaikin_Thermostat/Thermostat/TargetTemperature":         "21.5",
		"zigbee2mqtt/bed": `{"state":"ON","brightness":180}`,
	})
	doc := getAPIDoc(t, app)

	if !doc.BrokerConnected {
		t.Error("broker_connected should be true")
	}
	if doc.Mode.Current != "heating" {
		t.Errorf("mode.current = %q, want heating", doc.Mode.Current)
	}
	if len(doc.Zones) != 4 {
		t.Fatalf("got %d zones, want 4", len(doc.Zones))
	}

	up := doc.Zones[0]
	if up.Name != "Upstairs" || up.Secret {
		t.Errorf("zone 0 = %q secret=%v, want Upstairs, not secret", up.Name, up.Secret)
	}
	if up.Heating == nil || len(up.Heating.Rooms) != 2 {
		t.Fatalf("Upstairs should have 2 heating rooms, got %+v", up.Heating)
	}
	if got := up.Heating.Rooms[1].Actions["set_power"].Path; got != "/api/heating/room/Upstairs/Guest%20Room/power" {
		t.Errorf("Guest Room power path = %q", got)
	}

	// State on each item is exactly the SSE event for it.
	var room map[string]any
	json.Unmarshal(up.Heating.Rooms[0].State, &room)
	if room["type"] != "heating" || room["power"] != true || room["target_temp"] != 21.5 {
		t.Errorf("Bedroom state = %v", room)
	}
	var light map[string]any
	json.Unmarshal(up.Lights[0].State, &light)
	if light["type"] != "light" || light["on"] != true || light["brightness"] != 180.0 {
		t.Errorf("Bedroom light state = %v", light)
	}

	// Dimmability is learnt from the bulbs: only lights that have reported a
	// brightness get a brightness control.
	if d := up.Lights[0].Dimmable; d == nil || !*d {
		t.Error("Upstairs Bedroom light reports brightness, so should be dimmable")
	}
	if _, ok := up.Lights[0].Actions["set_brightness"]; !ok {
		t.Error("dimmable light should offer set_brightness")
	}
	if body, _ := up.Lights[0].Actions["set_power"].Body.(map[string]any); body["brightness"] != 180.0 {
		t.Errorf("set_power example should carry the current brightness, got %v", up.Lights[0].Actions["set_power"].Body)
	}

	// Bedroom (21.5, on) and Guest Room (default 20, off): only the on room counts.
	if s := up.Heating.Summary; s == nil || s.TargetTemp != 21.5 || !s.AnyOn || s.Quiet {
		t.Errorf("Upstairs summary = %+v, want 21.5, any_on, not quiet", s)
	}

	garden := doc.Zones[2]
	if garden.Heating != nil {
		t.Error("Garden has no heating rooms, so heating should be null")
	}
	if got := garden.Lights[0].Actions["set_power"].Path; got != "/api/light/Garden/Kids%27%20Room%20%E2%80%94%20Lamp/power" {
		t.Errorf("Kids' Room lamp path = %q", got)
	}

	if d := garden.Lights[0].Dimmable; d == nil || *d {
		t.Error("Garden lamp has reported no brightness, so should not be dimmable")
	}
	if _, ok := garden.Lights[0].Actions["set_brightness"]; ok {
		t.Error("non-dimmable light should not offer set_brightness")
	}

	if !doc.Zones[3].Secret {
		t.Error("Secret Lair should be marked secret")
	}

	if len(doc.Scenes) != 1 || doc.Scenes[0].Name != "Good Night" || doc.Scenes[0].Icon != "moon" {
		t.Fatalf("scenes = %+v", doc.Scenes)
	}
	if got := doc.Scenes[0].Actions["activate"].Path; got != "/api/scene/Good%20Night" {
		t.Errorf("scene path = %q", got)
	}
}

// TestAPIDoc_ActionsReachTheirHandlers sends every request the document hands
// out through the real router. The fake MQTT client is "connected" but cannot
// publish, so a request that found its item fails with 500 at the publish
// step (or succeeds, when there is nothing to send) — never a 404, and never
// the HTML page that the catch-all route would serve.
func TestAPIDoc_ActionsReachTheirHandlers(t *testing.T) {
	restore := timeAfter
	timeAfter = func(int) <-chan struct{} { ch := make(chan struct{}); close(ch); return ch }
	defer func() { timeAfter = restore }()

	app := testAppWithConfig(apiTestConfig(), map[string]string{
		// dimmable, so the brightness requests are in the document too
		"zigbee2mqtt/kids_lamp":   `{"state":"ON","brightness":100}`,
		"zigbee2mqtt/path_lights": `{"state":"ON","brightness":100}`,
	})
	doc := getAPIDoc(t, app)
	mux := http.NewServeMux()
	app.SetupRoutes(mux)

	actions := allActions(doc)
	if _, ok := actions["Garden/Path / Steps.set_brightness"]; !ok {
		t.Fatal("expected a brightness action for the dimmable Path / Steps light")
	}
	if len(actions) < 15 {
		t.Fatalf("only %d actions in the document — inventory missing?", len(actions))
	}
	for label, a := range actions {
		var body string
		if a.Body != nil {
			b, _ := json.Marshal(a.Body)
			body = string(b)
		}
		req := httptest.NewRequest(a.Method, a.Path, strings.NewReader(body))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)

		if w.Code == http.StatusNotFound || w.Code == http.StatusBadRequest || w.Code == http.StatusMethodNotAllowed {
			t.Errorf("%s: %s %s body=%s → %d %q", label, a.Method, a.Path, body, w.Code, strings.TrimSpace(w.Body.String()))
		}
		if strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
			t.Errorf("%s: %s %s fell through to the HTML page", label, a.Method, a.Path)
		}
	}
}

// TestAPIDoc_EndpointsAreRoutes checks the endpoint catalogue against the
// router: each documented method+path must be exactly a registered pattern.
func TestAPIDoc_EndpointsAreRoutes(t *testing.T) {
	app := testApp(nil)
	mux := http.NewServeMux()
	app.SetupRoutes(mux)
	// /status.json is registered by SetupRoutes too; the rest of main.go's
	// routes (static files, /help) aren't API.

	fill := strings.NewReplacer("{zone}", "Upstairs", "{room}", "Bedroom", "{name}", "Bedroom")
	for _, ep := range apiEndpoints {
		req := httptest.NewRequest(ep.Method, fill.Replace(ep.Path), nil)
		_, pattern := mux.Handler(req)
		if want := ep.Method + " " + ep.Path; pattern != want {
			t.Errorf("documented %q, but the router matches it to %q", want, pattern)
		}
		if ep.Summary == "" || len(ep.Responses) == 0 {
			t.Errorf("%s %s: summary and responses must be filled in", ep.Method, ep.Path)
		}
	}
}

// TestAPIDoc_DocumentsEveryAPIRoute is the other direction: an /api route
// added to SetupRoutes must be added to apiEndpoints too.
func TestAPIDoc_DocumentsEveryAPIRoute(t *testing.T) {
	documented := map[string]bool{}
	for _, ep := range apiEndpoints {
		documented[ep.Method+" "+ep.Path] = true
	}
	// Record every pattern SetupRoutes registers.
	rec := &recordingMux{ServeMux: http.NewServeMux()}
	app := testApp(nil)
	app.SetupRoutes(rec)
	for _, p := range rec.patterns {
		if p == "GET /" || p == "GET /api/{$}" || p == "GET /status" {
			continue // the page itself, /api's trailing-slash alias, the HTML status page
		}
		if !documented[p] {
			t.Errorf("route %q is not described in apiEndpoints (api.go)", p)
		}
	}
}

type recordingMux struct {
	*http.ServeMux
	patterns []string
}

func (m *recordingMux) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	m.patterns = append(m.patterns, pattern)
	m.ServeMux.HandleFunc(pattern, h)
}

func TestAPIDoc_EventExamplesAreValid(t *testing.T) {
	for name, ev := range apiEventDocs.Types {
		var m map[string]any
		if err := json.Unmarshal(ev.Example, &m); err != nil {
			t.Errorf("%s example is not JSON: %v", name, err)
			continue
		}
		if m["type"] != name {
			t.Errorf("%s example has type %v", name, m["type"])
		}
		for field := range m {
			if _, ok := ev.Fields[field]; !ok && field != "type" {
				t.Errorf("%s example field %q is not documented", name, field)
			}
		}
	}
}

// TestAPIDoc_ExamplesMatchRealEvents compares the documented example events
// with what the server really produces, so the field lists can't drift.
func TestAPIDoc_ExamplesMatchRealEvents(t *testing.T) {
	app := testApp(map[string]string{"zigbee2mqtt/bed": `{"state":"ON","brightness":200}`})
	real := map[string]string{
		"mode":    app.buildModeEvent(),
		"heating": app.buildHeatingEvent("Upstairs", app.Config.Zones[0].Heating[0]),
		"light":   app.buildLightEvent("Upstairs", app.Config.Zones[0].Lights[0]),
	}
	for name, event := range real {
		var got, doc map[string]any
		json.Unmarshal([]byte(event), &got)
		json.Unmarshal(apiEventDocs.Types[name].Example, &doc)
		for k := range got {
			if _, ok := doc[k]; !ok {
				t.Errorf("%s events carry %q, which the documented example lacks", name, k)
			}
		}
		for k := range doc {
			if _, ok := got[k]; !ok {
				t.Errorf("documented %s example has %q, which real events lack", name, k)
			}
		}
	}
}

// TestAPIDoc_WorksWhileMQTTDown: the document is mostly config, so it should
// still answer — but must not present stale cache as live state.
func TestAPIDoc_WorksWhileMQTTDown(t *testing.T) {
	app := testAppDisconnected()
	app.MQTT.cache["zigbee2mqtt/bed"] = `{"state":"ON"}` // stale leftovers
	doc := getAPIDoc(t, app)

	if doc.BrokerConnected {
		t.Error("broker_connected should be false")
	}
	if len(doc.Zones) != 2 {
		t.Fatalf("inventory should still be listed, got %d zones", len(doc.Zones))
	}
	for _, z := range doc.Zones {
		if z.Heating.Summary != nil {
			t.Errorf("%s: summary present while disconnected", z.Name)
		}
		for _, r := range z.Heating.Rooms {
			if r.State != nil {
				t.Errorf("%s/%s: state present while disconnected", z.Name, r.Name)
			}
		}
		for _, l := range z.Lights {
			if l.State != nil || l.Dimmable != nil {
				t.Errorf("%s/%s: state present while disconnected", z.Name, l.Name)
			}
		}
	}
}

// TestAPIDoc_RawApostropheAccepted backs the claim in apiConventions that
// clients using encodeURIComponent (which leaves ' alone) reach the right item.
func TestAPIDoc_RawApostropheAccepted(t *testing.T) {
	app := testAppWithConfig(apiTestConfig(), nil)
	mux := http.NewServeMux()
	app.SetupRoutes(mux)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/light/Garden/Kids'%20Room%20%E2%80%94%20Lamp/power", strings.NewReader(`{"value":true}`))
	mux.ServeHTTP(w, req)
	if w.Code == http.StatusNotFound {
		t.Errorf("raw apostrophe path → 404 %q", w.Body.String())
	}
}

func TestAPIDoc_TrailingSlash(t *testing.T) {
	app := testApp(nil)
	mux := http.NewServeMux()
	app.SetupRoutes(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/", nil))
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/json" {
		t.Errorf("GET /api/ = %d %s, want the JSON document", w.Code, w.Header().Get("Content-Type"))
	}
}

func TestAPIDoc_CoolingMode(t *testing.T) {
	app := testApp(nil)
	app.SetCoolingMode(true)
	doc := getAPIDoc(t, app)
	if doc.Mode.Current != "cooling" {
		t.Errorf("mode.current = %q, want cooling", doc.Mode.Current)
	}
	if body, _ := doc.Mode.Actions["set"].Body.(map[string]any); body["mode"] != "heating" {
		t.Errorf("mode.set example should switch to heating, got %v", doc.Mode.Actions["set"].Body)
	}
}

func TestAPIState(t *testing.T) {
	app := testApp(map[string]string{"zigbee2mqtt/bed": `{"state":"ON"}`})
	mux := http.NewServeMux()
	app.SetupRoutes(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/state", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/state = %d", w.Code)
	}
	if w.Body.String() != app.buildSnapshot() {
		t.Error("/api/state should be the SSE snapshot")
	}
	var events []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &events); err != nil {
		t.Fatalf("not a JSON array: %v", err)
	}
	if len(events) != 6 || events[0]["type"] != "mode" { // mode + 3 rooms + 2 lights
		t.Errorf("got %d events, first %v", len(events), events[0])
	}
}

func TestAPIState_503WhenMQTTDisconnected(t *testing.T) {
	app := testAppDisconnected()
	w := httptest.NewRecorder()
	app.handleAPIState(w, httptest.NewRequest("GET", "/api/state", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("got %d, want 503", w.Code)
	}
}

func TestHandleScene_503WhenMQTTDisconnected(t *testing.T) {
	app := testAppDisconnected()
	app.Config.Scenes = []SceneConfig{{Name: "Night", Actions: []SceneAction{{Topic: "a", Payload: "b"}}}}
	req := httptest.NewRequest("POST", "/api/scene/Night", nil)
	req.SetPathValue("name", "Night")
	w := httptest.NewRecorder()
	app.handleScene(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("got %d, want 503", w.Code)
	}
}

func TestSummarizeHeating(t *testing.T) {
	cases := []struct {
		name  string
		rooms []heatingState
		want  apiHeatingSummary
	}{
		{"only rooms that are on count",
			[]heatingState{{Power: true, TargetTemp: 19, Quiet: true}, {Power: false, TargetTemp: 24, Quiet: false}},
			apiHeatingSummary{TargetTemp: 19, Quiet: true, AnyOn: true}},
		{"highest setpoint among on rooms",
			[]heatingState{{Power: true, TargetTemp: 19}, {Power: true, TargetTemp: 22}},
			apiHeatingSummary{TargetTemp: 22, AnyOn: true}},
		{"quiet needs every on room",
			[]heatingState{{Power: true, Quiet: true, TargetTemp: 20}, {Power: true, Quiet: false, TargetTemp: 20}},
			apiHeatingSummary{TargetTemp: 20, Quiet: false, AnyOn: true}},
		{"all off falls back to all rooms",
			[]heatingState{{TargetTemp: 18, Quiet: true}, {TargetTemp: 23, Quiet: true}},
			apiHeatingSummary{TargetTemp: 23, Quiet: true}},
	}
	for _, c := range cases {
		if got := summarizeHeating(c.rooms); got == nil || *got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

// TestE2E_DriveFromAPIDoc does what a new frontend would: read /api, pick a
// control from it, send the request it describes, and see the change come
// back on the event stream.
func TestE2E_DriveFromAPIDoc(t *testing.T) {
	baseURL, app, cleanup := e2eSetup(t)
	defer cleanup()
	defer clearRetained(t, app)

	resp, err := http.Get(baseURL + "/api")
	if err != nil {
		t.Fatalf("GET /api: %v", err)
	}
	var doc apiDoc
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("decode /api: %v", err)
	}
	resp.Body.Close()
	if !doc.BrokerConnected {
		t.Fatal("broker_connected should be true")
	}

	events, closeSSE := sseReader(t, baseURL)
	defer closeSSE()
	time.Sleep(300 * time.Millisecond)
	for len(events) > 0 {
		<-events
	}

	set := doc.Zones[0].Heating.Actions["set_temperature"]
	resp, err = postJSON(baseURL, set.Path, 23.5)
	if err != nil {
		t.Fatalf("%s %s: %v", set.Method, set.Path, err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("%s %s = %d", set.Method, set.Path, resp.StatusCode)
	}
	if _, ok := waitForEvent(events, 3*time.Second, func(e map[string]any) bool {
		return e["type"] == "heating" && e["room"] == "Guest Room" && e["target_temp"] == 23.5
	}); !ok {
		t.Fatal("did not receive the temperature change on the event stream")
	}
}
