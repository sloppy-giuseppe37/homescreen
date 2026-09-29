package main

// api.go — the self-describing API for building other frontends.
//
//   GET /api        → a JSON document describing everything a client needs:
//                     conventions, every endpoint, the SSE event formats, the
//                     rules the web UI applies on top of the raw state, and the
//                     live inventory (zones, heating rooms, lights, scenes, mode)
//                     with the exact request each control sends.
//   GET /api/state  → the current state as a JSON array — the same events the
//                     SSE stream opens with — for clients that poll instead.
//
// The point of /api is that an agent (or a person) pointed at it cold can build
// a touchscreen panel or a hardware button box without reading this repo, so
// the prose here is part of the interface: when an endpoint or a UI rule
// changes, update the description below alongside it. api_test.go checks the
// document against the router so paths cannot silently drift.
//
// /api answers with the broker down (it is mostly config), reporting
// broker_connected=false and leaving out live state. /api/state 503s like the
// rest of the API, since it has nothing truthful to say without the broker.

import (
	"encoding/json"
	"log"
	"net/http"
	"net/url"
)

// apiDoc is the document served at GET /api. Field order is the reading order.
type apiDoc struct {
	Name            string        `json:"name"`
	Description     string        `json:"description"`
	Version         string        `json:"version"`
	BaseURL         string        `json:"base_url,omitempty"`
	BrokerConnected bool          `json:"broker_connected"`
	Conventions     []string      `json:"conventions"`
	Mode            apiMode       `json:"mode"`
	Zones           []apiZone     `json:"zones"`
	Scenes          []apiScene    `json:"scenes"`
	Endpoints       []apiEndpoint `json:"endpoints"`
	Events          apiEvents     `json:"events"`
	UIRules         []string      `json:"ui_rules"`
}

// apiAction is one concrete request a client can send, with the names already
// filled in and percent-encoded. Body is an example of a valid body; the
// matching entry in Endpoints describes it fully.
type apiAction struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Body   any    `json:"body,omitempty"`
}

type apiMode struct {
	Current string               `json:"current"` // "heating" or "cooling"
	Actions map[string]apiAction `json:"actions"`
}

type apiZone struct {
	Name    string      `json:"name"`
	Secret  bool        `json:"secret"`
	Heating *apiHeating `json:"heating"` // null when the zone has no heating rooms
	Lights  []apiLight  `json:"lights"`
}

// apiHeating is a zone's heating: one temperature and one quiet control shared
// by all its rooms, plus a power switch per room.
type apiHeating struct {
	Summary *apiHeatingSummary   `json:"summary,omitempty"` // omitted while the broker is down
	Actions map[string]apiAction `json:"actions"`
	Rooms   []apiRoom            `json:"rooms"`
}

// apiHeatingSummary is the zone-level reading the web UI displays, computed
// from the rooms with the rules in ui_rules.
type apiHeatingSummary struct {
	TargetTemp float64 `json:"target_temp"`
	Quiet      bool    `json:"quiet"`
	AnyOn      bool    `json:"any_on"`
}

type apiRoom struct {
	Name    string               `json:"name"`
	State   json.RawMessage      `json:"state,omitempty"` // the room's SSE "heating" event
	Actions map[string]apiAction `json:"actions"`
}

type apiLight struct {
	Name     string               `json:"name"`
	Dimmable *bool                `json:"dimmable,omitempty"` // omitted while the broker is down
	State    json.RawMessage      `json:"state,omitempty"`    // the light's SSE "light" event
	Actions  map[string]apiAction `json:"actions"`            // set_brightness only when dimmable
}

type apiScene struct {
	Name        string               `json:"name"`
	Description string               `json:"description,omitempty"`
	Icon        string               `json:"icon,omitempty"`
	Actions     map[string]apiAction `json:"actions"`
}

// apiEndpoint documents one route in general terms (with {placeholders}).
type apiEndpoint struct {
	Method    string            `json:"method"`
	Path      string            `json:"path"`
	Summary   string            `json:"summary"`
	Body      map[string]string `json:"body,omitempty"` // field → meaning
	Example   any               `json:"example,omitempty"`
	Responses map[string]string `json:"responses"`
}

type apiEvents struct {
	Path        string              `json:"path"`
	Description string              `json:"description"`
	Types       map[string]apiEvent `json:"types"`
}

type apiEvent struct {
	Description string            `json:"description"`
	Fields      map[string]string `json:"fields"`
	Example     json.RawMessage   `json:"example"`
}

// handleAPIDoc serves the self-describing document at GET /api.
func (app *App) handleAPIDoc(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false) // keep "<", ">" and "&" in the prose readable
	if err := enc.Encode(app.buildAPIDoc()); err != nil {
		log.Printf("API doc error: %v", err)
	}
}

// handleAPIState serves the current state of everything as one JSON array:
// the snapshot the SSE stream opens with, for clients that would rather poll.
func (app *App) handleAPIState(w http.ResponseWriter, r *http.Request) {
	if !app.checkMQTT(w) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(app.buildSnapshot()))
}

// buildAPIDoc assembles the /api document from the config and, when the broker
// is up, the current cached state.
func (app *App) buildAPIDoc() apiDoc {
	connected := app.MQTT.IsConnected()
	mode := "heating"
	if app.IsCoolingMode() {
		mode = "cooling"
	}

	doc := apiDoc{
		Name: "Home Control API",
		Description: "HTTP + Server-Sent Events API behind the Home Control panel. " +
			"It controls heating/cooling units and zigbee lights, grouped into zones, " +
			"and runs preset scenes. Everything the bundled web UI does goes through " +
			"these endpoints, so any other frontend can do the same. The MQTT broker " +
			"behind the server is the source of truth: the server keeps no state of " +
			"its own, and every change — from any client or from outside — is pushed " +
			"to every client over the event stream.",
		Version:         Version,
		BaseURL:         app.Config.BaseURL,
		BrokerConnected: connected,
		Conventions:     apiConventions,
		Mode: apiMode{
			Current: mode,
			Actions: map[string]apiAction{
				"get": {Method: "GET", Path: "/api/mode"},
				"set": {Method: "POST", Path: "/api/mode", Body: map[string]any{"mode": otherMode(mode)}},
			},
		},
		Zones:     []apiZone{},
		Scenes:    []apiScene{},
		Endpoints: apiEndpoints,
		Events:    apiEventDocs,
		UIRules:   apiUIRules,
	}

	for _, zone := range app.Config.Zones {
		z := apiZone{Name: zone.Name, Secret: zone.Secret, Lights: []apiLight{}}
		zp := url.PathEscape(zone.Name)

		if len(zone.Heating) > 0 {
			h := &apiHeating{
				Actions: map[string]apiAction{
					"set_temperature": {Method: "POST", Path: "/api/heating/zone/" + zp + "/temperature", Body: map[string]any{"value": 21}},
					"set_quiet":       {Method: "POST", Path: "/api/heating/zone/" + zp + "/quiet", Body: map[string]any{"value": true}},
				},
				Rooms: []apiRoom{},
			}
			var events []heatingState
			for _, room := range zone.Heating {
				rm := apiRoom{
					Name: room.Name,
					Actions: map[string]apiAction{
						"set_power": {Method: "POST", Path: "/api/heating/room/" + zp + "/" + url.PathEscape(room.Name) + "/power", Body: map[string]any{"value": true}},
					},
				}
				if connected {
					event := app.buildHeatingEvent(zone.Name, room)
					rm.State = json.RawMessage(event)
					var hs heatingState
					json.Unmarshal([]byte(event), &hs)
					events = append(events, hs)
				}
				h.Rooms = append(h.Rooms, rm)
			}
			if connected {
				h.Summary = summarizeHeating(events)
			}
			z.Heating = h
		}

		for _, light := range zone.Lights {
			base := "/api/light/" + zp + "/" + url.PathEscape(light.Name)
			l := apiLight{
				Name: light.Name,
				Actions: map[string]apiAction{
					"set_power": {Method: "POST", Path: base + "/power", Body: map[string]any{"value": true}},
				},
			}
			if connected {
				event := app.buildLightEvent(zone.Name, light)
				l.State = json.RawMessage(event)
				var ls struct {
					Brightness *int `json:"brightness"`
				}
				json.Unmarshal([]byte(event), &ls)
				dimmable := ls.Brightness != nil
				l.Dimmable = &dimmable
				if dimmable {
					l.Actions["set_power"] = apiAction{Method: "POST", Path: base + "/power", Body: map[string]any{"value": true, "brightness": *ls.Brightness}}
					l.Actions["set_brightness"] = apiAction{Method: "POST", Path: base + "/brightness", Body: map[string]any{"value": 127}}
				}
			}
			z.Lights = append(z.Lights, l)
		}

		doc.Zones = append(doc.Zones, z)
	}

	for _, scene := range app.Config.Scenes {
		doc.Scenes = append(doc.Scenes, apiScene{
			Name:        scene.Name,
			Description: scene.Description,
			Icon:        scene.Icon,
			Actions: map[string]apiAction{
				"activate": {Method: "POST", Path: "/api/scene/" + url.PathEscape(scene.Name)},
			},
		})
	}

	return doc
}

// heatingState is the part of a "heating" event the zone summary needs.
type heatingState struct {
	Power      bool    `json:"power"`
	TargetTemp float64 `json:"target_temp"`
	Quiet      bool    `json:"quiet"`
}

// summarizeHeating applies the web UI's zone rules (see apiUIRules) to a
// zone's rooms: the displayed temperature is the highest setpoint among rooms
// that are on, and quiet shows on only if every room that is on has it — both
// falling back to all rooms when none are on.
func summarizeHeating(rooms []heatingState) *apiHeatingSummary {
	if len(rooms) == 0 {
		return nil
	}
	var on []heatingState
	for _, r := range rooms {
		if r.Power {
			on = append(on, r)
		}
	}
	considered := rooms
	if len(on) > 0 {
		considered = on
	}
	s := &apiHeatingSummary{TargetTemp: considered[0].TargetTemp, Quiet: true, AnyOn: len(on) > 0}
	for _, r := range considered {
		s.TargetTemp = max(s.TargetTemp, r.TargetTemp)
		s.Quiet = s.Quiet && r.Quiet
	}
	return s
}

func otherMode(mode string) string {
	if mode == "cooling" {
		return "heating"
	}
	return "cooling"
}

// ---------- The static parts of the document ----------
//
// Keep these in step with handlers.go and templates/index.html.

var apiConventions = []string{
	"Zones, heating rooms, lights and scenes are identified by their display names, exactly as listed in this document (case-sensitive). Names go in URL paths percent-encoded (JavaScript's encodeURIComponent is fine; the server accepts characters such as ' either raw or encoded, and \"/\" inside a name must be sent as %2F). Every item below lists its ready-encoded request paths under \"actions\", so clients can use those directly instead of building URLs.",
	"POST bodies are JSON (Content-Type: application/json). Success is 204 No Content with an empty body. Errors are plain text: 400 for a malformed body or out-of-range value, 404 for an unknown zone/room/light/scene, 500 if publishing to a device failed, 503 while the MQTT broker is unreachable.",
	"A successful POST means the command was handed to the broker, not that the device has changed. The new state arrives on the event stream (GET /api/events), usually well under a second later, but heating and lights differ in where it comes from. Heating commands are written to the rooms' retained state topics, so a heating event follows every accepted POST. Light commands go to the zigbee2mqtt bridge, and a light event arrives only once the bridge reports the bulb's new state. With no bridge running (a development setup, or the bridge is down), light POSTs still return 204 but no light event follows and the light's state never changes. A light's brightness is likewise only known once its bulbs have reported one. Treat the event stream as the only source of truth; an optimistic UI is fine, but let incoming events correct it.",
	"While the broker is unreachable, the event stream, GET /api/state and every POST return 503, and open event streams are closed (GET /api still answers, with broker_connected=false). Show an offline state and retry every few seconds; nothing needs resetting when it comes back.",
	"The zone/room/light/scene inventory comes from the server's config file and only changes when the server restarts. Fetch GET /api at startup and again whenever the event stream reconnects.",
	"There is no authentication in the app itself; access control, if any, is done by whatever sits in front of it. No CORS headers are sent, so a browser-based frontend must be served from this same origin (or reach the API through its own proxy); devices and native apps are unaffected.",
	"There is no rate limiting, but every POST becomes MQTT traffic to real devices: send changes when the user commits them (see ui_rules on sliders), not continuously.",
	"This document includes live state (\"state\" on each room and light, \"summary\" on each zone's heating, \"dimmable\" on each light) when broker_connected is true, so it can double as a one-off snapshot. For live updates, use the event stream.",
}

var apiEndpoints = []apiEndpoint{
	{
		Method:    "GET",
		Path:      "/api",
		Summary:   "This document: conventions, endpoints, event formats, UI rules, and the live inventory with ready-made requests for every control.",
		Responses: map[string]string{"200": "application/json"},
	},
	{
		Method:    "GET",
		Path:      "/api/events",
		Summary:   "Server-Sent Events stream: a full snapshot of every mode, heating room and light, then live changes. See \"events\".",
		Responses: map[string]string{"200": "text/event-stream", "503": "broker unreachable — retry"},
	},
	{
		Method:    "GET",
		Path:      "/api/state",
		Summary:   "The current state as a JSON array of events — the same objects the event stream opens with (one mode event, then one per heating room and per light). For clients that poll instead of holding a stream open.",
		Responses: map[string]string{"200": "application/json array of events", "503": "broker unreachable — retry"},
	},
	{
		Method:  "POST",
		Path:    "/api/heating/zone/{zone}/temperature",
		Summary: "Set the target temperature (°C) of every heating room in the zone, including rooms that are off, so they start at this setpoint when turned on.",
		Body: map[string]string{
			"value": "number, °C, rounded to one decimal place. Any number is accepted (400 only if it isn't a number), so enforce a sensible range in the client: the web UI offers whole degrees from 16 to 25, in both heating and cooling mode. There is one setpoint per room, shared by both modes.",
		},
		Example:   map[string]any{"value": 21},
		Responses: map[string]string{"204": "sent", "400": "value is not a number", "404": "unknown zone", "500": "publish failed", "503": "broker unreachable"},
	},
	{
		Method:  "POST",
		Path:    "/api/heating/zone/{zone}/quiet",
		Summary: "Turn quiet (low-noise) mode on or off for every heating room in the zone.",
		Body: map[string]string{
			"value": "boolean. Anything other than true turns quiet mode off.",
		},
		Example:   map[string]any{"value": true},
		Responses: map[string]string{"204": "sent", "400": "invalid JSON", "404": "unknown zone", "500": "publish failed", "503": "broker unreachable"},
	},
	{
		Method:  "POST",
		Path:    "/api/heating/room/{zone}/{room}/power",
		Summary: "Turn one heating room on or off. On means heat or cool depending on the global mode (see /api/mode); the room's current setpoint is re-sent first so it starts at the right temperature.",
		Body: map[string]string{
			"value": "boolean. Anything other than true turns the room off.",
		},
		Example:   map[string]any{"value": true},
		Responses: map[string]string{"204": "sent", "400": "invalid JSON", "404": "unknown zone or room", "500": "publish failed", "503": "broker unreachable"},
	},
	{
		Method:  "POST",
		Path:    "/api/light/{zone}/{name}/power",
		Summary: "Turn a light on or off. A light may be a group of several bulbs; this switches all of them.",
		Body: map[string]string{
			"value":      "boolean. Anything other than true turns the light off.",
			"brightness": "optional integer 0–254, only used when turning on: bulbs that support dimming come on at this level. Send the level your brightness control shows so the light doesn't come on at a stale level.",
		},
		Example:   map[string]any{"value": true, "brightness": 200},
		Responses: map[string]string{"204": "sent", "400": "invalid JSON", "404": "unknown zone or light", "500": "publish failed", "503": "broker unreachable"},
	},
	{
		Method:  "POST",
		Path:    "/api/light/{zone}/{name}/brightness",
		Summary: "Set the brightness of a light. Only reaches bulbs that are dimmable AND currently on — it never turns a light on (use power with a brightness for that). Only meaningful for lights whose state includes \"brightness\" (\"dimmable\": true in the inventory); for others it succeeds and does nothing.",
		Body: map[string]string{
			"value": "integer 0–254 (254 = full). The web UI's slider runs 1–254 and shows round(value / 254 × 100)%.",
		},
		Example:   map[string]any{"value": 128},
		Responses: map[string]string{"204": "sent (possibly to no bulbs, if none are on)", "400": "not a number, or outside 0–254", "404": "unknown zone or light", "500": "publish failed", "503": "broker unreachable"},
	},
	{
		Method:    "GET",
		Path:      "/api/mode",
		Summary:   "The global HVAC mode.",
		Example:   map[string]any{"mode": "heating"},
		Responses: map[string]string{"200": "application/json {\"mode\": \"heating\" | \"cooling\"}"},
	},
	{
		Method:  "POST",
		Path:    "/api/mode",
		Summary: "Switch the global HVAC mode between heating and cooling. Actually changing it TURNS OFF EVERY HEATING ROOM in the house, so ask the user to confirm first. Setting the current mode again does nothing.",
		Body: map[string]string{
			"mode": "\"heating\" or \"cooling\"",
		},
		Example:   map[string]any{"mode": "cooling"},
		Responses: map[string]string{"204": "changed, or already in that mode", "400": "invalid JSON or unknown mode", "500": "publish failed", "503": "broker unreachable"},
	},
	{
		Method:    "POST",
		Path:      "/api/scene/{name}",
		Summary:   "Run a scene: a preset batch of device commands. No body. The steps run one after another, 200 ms apart, and the response comes back when they have all been sent — that can take a few seconds, so show a busy state and allow a generous timeout. Scene effects arrive on the event stream like any other change.",
		Responses: map[string]string{"204": "all steps sent", "404": "unknown scene", "408": "client disconnected mid-scene (the remaining steps were not sent)", "500": "one or more steps failed to publish (the rest were still sent)"},
	},
	{
		Method:    "GET",
		Path:      "/status.json",
		Summary:   "Server health: version, uptime, MQTT connection state and history, connected clients. Answers even while the broker is down. /status is the same as a human-readable page.",
		Responses: map[string]string{"200": "application/json"},
	},
}

var apiEventDocs = apiEvents{
	Path: "/api/events",
	Description: "Standard Server-Sent Events (EventSource in browsers; any HTTP client that reads a streaming response works too). " +
		"Each message is a single \"data: <json>\" line followed by a blank line; there are no event names or ids — dispatch on the JSON \"type\" field. " +
		"On connect the server first sends the complete current state: one \"mode\" event, then one event per heating room and per light (the same array GET /api/state returns). " +
		"After that it sends an event whenever something changes; each event carries the FULL current state of that one item, so just replace what you had. " +
		"A \": heartbeat\" comment line arrives every 15 seconds — if nothing (not even a heartbeat) arrives for ~40 seconds, assume the connection is dead and reconnect. " +
		"The server closes all streams when it loses the MQTT broker; reconnect with a few seconds' delay (expect 503 until the broker is back), and treat the snapshot on reconnect as the new truth. " +
		"Ignore event types you don't recognise.",
	Types: map[string]apiEvent{
		"mode": {
			Description: "The global HVAC mode. Sent first in every snapshot, and whenever the mode changes. After a mode change, expect this event and a \"heating\" event with power=false for each room that was on, in no guaranteed order.",
			Fields: map[string]string{
				"mode": "\"heating\" or \"cooling\"",
			},
			Example: json.RawMessage(`{"type":"mode","mode":"heating"}`),
		},
		"heating": {
			Description: "One heating room's state. Zone-level readings (the temperature and quiet controls) are derived from all of a zone's rooms — see ui_rules.",
			Fields: map[string]string{
				"zone":        "zone name",
				"room":        "room name",
				"power":       "boolean — the unit is running (heating or cooling, per the mode)",
				"target_temp": "number, °C — the setpoint (20 if the unit has never reported one)",
				"quiet":       "boolean — quiet mode",
			},
			Example: json.RawMessage(`{"type":"heating","zone":"Upstairs","room":"Bedroom","power":true,"target_temp":21,"quiet":false}`),
		},
		"light": {
			Description: "One light's state. A light can be a group of bulbs: it is on if ANY of them is on. Bulbs that are unreachable count as off.",
			Fields: map[string]string{
				"zone":          "zone name",
				"name":          "light name",
				"on":            "boolean",
				"brightness":    "integer 0–254, the highest level among the group's dimmable bulbs. ABSENT when no reachable bulb in the group has reported a brightness level — it isn't dimmable, or is offline or not heard from yet. Hide the brightness control while it's absent, but show it as soon as an event includes it: dimmability is learnt from the devices, not configured.",
				"brightness_on": "boolean, present with brightness: whether any dimmable bulb is on. When false, show the brightness control disabled/greyed (adjusting it would have no effect until the light is on).",
			},
			Example: json.RawMessage(`{"type":"light","zone":"Upstairs","name":"Bedroom","on":true,"brightness":200,"brightness_on":true}`),
		},
	},
}

var apiUIRules = []string{
	"Layout: the web UI has a Lights view and a Heating view, each listing zones in config order with their lights / heating rooms in config order, plus a Scenes view when scenes exist.",
	"Heating is controlled per ZONE for temperature and quiet mode (one slider and one quiet toggle per zone, applied to every room in it) and per ROOM for power (one on/off switch per room).",
	"Zone temperature shown = the highest target_temp among the zone's rooms that are on; if none are on, the highest across all its rooms. The web UI rounds it to a whole degree. (\"summary.target_temp\" in this document is this value.)",
	"Zone quiet shown = true only if every room that is on has quiet=true; if none are on, only if every room has quiet=true. (\"summary.quiet\")",
	"When every room in a zone is off (\"summary.any_on\" false), the web UI greys out that zone's temperature and quiet controls but leaves them usable — changes still apply and take effect when a room is turned on.",
	"In cooling mode the web UI relabels Heating as Cooling, swaps the flame icon for a snowflake and the orange accent for icy blue (#38bdf8). The API calls are identical in both modes.",
	"Zones with secret=true are hidden by default. The web UI shows them only after a deliberate unlock gesture (13 rapid taps on the Lights/Heating heading); another frontend can offer its own unlock or simply leave them out. They work exactly like other zones.",
	"Scene icons are Lucide icon names (https://lucide.dev/icons/), e.g. \"sun\", \"moon\". A scene's description is free text written for people; the API does not say which devices a scene touches — watch the event stream to see its effects.",
	"Sliders: send at most one request per control every ~300 ms while the user drags (the web UI waits for the drag to pause), and don't let incoming events move a control the user is touching or has changed in the last ~1.5–2 s, so it doesn't jump back while the device catches up.",
	"Turning a dimmable light on: include the brightness your control currently shows in the power request, so it comes on at that level rather than its old one.",
}
