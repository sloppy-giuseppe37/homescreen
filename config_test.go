package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestHeatingTopics verifies that HeatingRoom.HeatingTopics()
// constructs the correct MQTT topic strings from a unit ID.
func TestHeatingTopics(t *testing.T) {
	room := HeatingRoom{Name: "Bedroom", UnitID: "BedroomFaikin"}
	power, temp, quiet := room.HeatingTopics()

	wantPower := "HomeKit/BedroomFaikin_Thermostat/Thermostat/TargetHeatingCoolingState"
	wantTemp := "HomeKit/BedroomFaikin_Thermostat/Thermostat/TargetTemperature"
	wantQuiet := "HomeKit/BedroomFaikin_IndoorQuiet/Switch/On"

	if power != wantPower {
		t.Errorf("power topic = %q, want %q", power, wantPower)
	}
	if temp != wantTemp {
		t.Errorf("temp topic = %q, want %q", temp, wantTemp)
	}
	if quiet != wantQuiet {
		t.Errorf("quiet topic = %q, want %q", quiet, wantQuiet)
	}
}

// TestLoadConfig_Valid writes a valid YAML config to a temp directory,
// overrides HOME, and verifies it parses correctly.
func TestLoadConfig_Valid(t *testing.T) {
	// Create a temp home directory with the config file
	tmpHome := t.TempDir()
	configDir := filepath.Join(tmpHome, ".config", "homescreen")
	os.MkdirAll(configDir, 0755)

	yamlContent := `
mqtt:
  broker: "tcp://testhost:1883"
  topic_prefix: "zigbee2mqtt"

zones:
  - name: Upstairs
    heating:
      - name: Bedroom
        unit_id: BedroomFaikin
    lights:
      - name: Bedroom
        entities: [bed, ceiling]
  - name: Downstairs
    heating:
      - name: Kitchen
        unit_id: KitchenFaikin
`
	os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(yamlContent), 0644)

	// Override HOME so LoadConfig finds our temp config
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error: %v", err)
	}

	// Verify broker
	if cfg.MQTT.Broker != "tcp://testhost:1883" {
		t.Errorf("broker = %q, want %q", cfg.MQTT.Broker, "tcp://testhost:1883")
	}

	// Verify topic prefix
	if cfg.MQTT.TopicPrefix != "zigbee2mqtt" {
		t.Errorf("topic_prefix = %q, want %q", cfg.MQTT.TopicPrefix, "zigbee2mqtt")
	}

	// Verify zones
	if len(cfg.Zones) != 2 {
		t.Fatalf("got %d zones, want 2", len(cfg.Zones))
	}
	if cfg.Zones[0].Name != "Upstairs" {
		t.Errorf("zone[0].Name = %q, want %q", cfg.Zones[0].Name, "Upstairs")
	}

	// Verify heating rooms
	if len(cfg.Zones[0].Heating) != 1 {
		t.Fatalf("Upstairs heating rooms = %d, want 1", len(cfg.Zones[0].Heating))
	}
	if cfg.Zones[0].Heating[0].UnitID != "BedroomFaikin" {
		t.Errorf("unit_id = %q, want %q", cfg.Zones[0].Heating[0].UnitID, "BedroomFaikin")
	}

	// Verify lights
	if len(cfg.Zones[0].Lights) != 1 {
		t.Fatalf("Upstairs lights = %d, want 1", len(cfg.Zones[0].Lights))
	}
	if len(cfg.Zones[0].Lights[0].Entities) != 2 {
		t.Fatalf("light entities = %d, want 2", len(cfg.Zones[0].Lights[0].Entities))
	}
	if cfg.Zones[0].Lights[0].Entities[0] != "bed" {
		t.Errorf("entity[0] = %q, want %q", cfg.Zones[0].Lights[0].Entities[0], "bed")
	}
}

// TestLightStateTopics verifies that LightConfig.StateTopics()
// constructs the correct MQTT state topic strings.
func TestLightStateTopics(t *testing.T) {
	light := LightConfig{Name: "Kitchen", Entities: []string{"fairy_lights", "kitchen_table_1"}}
	topics := light.StateTopics("zigbee2mqtt")

	if len(topics) != 2 {
		t.Fatalf("got %d topics, want 2", len(topics))
	}
	if topics[0] != "zigbee2mqtt/fairy_lights" {
		t.Errorf("topic[0] = %q, want %q", topics[0], "zigbee2mqtt/fairy_lights")
	}
	if topics[1] != "zigbee2mqtt/kitchen_table_1" {
		t.Errorf("topic[1] = %q, want %q", topics[1], "zigbee2mqtt/kitchen_table_1")
	}
}

// TestLightSetTopic verifies that LightConfig.SetTopic()
// constructs the correct MQTT command topic.
func TestLightSetTopic(t *testing.T) {
	light := LightConfig{Name: "Kitchen", Entities: []string{"fairy_lights"}}
	topic := light.SetTopic("zigbee2mqtt", "fairy_lights")

	want := "zigbee2mqtt/fairy_lights/set"
	if topic != want {
		t.Errorf("set topic = %q, want %q", topic, want)
	}
}

// TestLightGetTopic verifies that LightConfig.GetTopic()
// constructs the correct MQTT get topic for requesting state.
func TestLightGetTopic(t *testing.T) {
	light := LightConfig{Name: "Kitchen", Entities: []string{"fairy_lights"}}
	topic := light.GetTopic("zigbee2mqtt", "fairy_lights")

	want := "zigbee2mqtt/fairy_lights/get"
	if topic != want {
		t.Errorf("get topic = %q, want %q", topic, want)
	}
}

// TestLoadConfig_MissingFile verifies we get a clear error
// when the config file doesn't exist.
func TestLoadConfig_MissingFile(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for missing config, got nil")
	}
}

// TestLoadConfigFrom_Direct verifies LoadConfigFrom reads a specific path.
func TestLoadConfigFrom_Direct(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "config.yaml")
	yaml := `
mqtt:
  broker: "tcp://direct:1883"
zones:
  - name: TestZone
`
	os.WriteFile(tmpFile, []byte(yaml), 0644)

	cfg, err := LoadConfigFrom(tmpFile)
	if err != nil {
		t.Fatalf("LoadConfigFrom() error: %v", err)
	}
	if cfg.MQTT.Broker != "tcp://direct:1883" {
		t.Errorf("broker = %q, want %q", cfg.MQTT.Broker, "tcp://direct:1883")
	}
}

// TestLoadConfig_MissingBroker verifies we reject a config
// that has no broker specified.
func TestLoadConfig_MissingBroker(t *testing.T) {
	tmpHome := t.TempDir()
	configDir := filepath.Join(tmpHome, ".config", "homescreen")
	os.MkdirAll(configDir, 0755)

	yamlContent := `
zones:
  - name: Test
`
	os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(yamlContent), 0644)

	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for missing broker, got nil")
	}
}

func TestLightAvailabilityTopics(t *testing.T) {
	light := LightConfig{
		Name:     "Kitchen",
		Entities: []string{"fairy_lights", "kitchen_table_1"},
	}

	topics := light.AvailabilityTopics("zigbee2mqtt")

	expected := []string{
		"zigbee2mqtt/fairy_lights/availability",
		"zigbee2mqtt/kitchen_table_1/availability",
	}

	if len(topics) != len(expected) {
		t.Fatalf("got %d topics, want %d", len(topics), len(expected))
	}
	for i, topic := range topics {
		if topic != expected[i] {
			t.Errorf("topic[%d] = %q, want %q", i, topic, expected[i])
		}
	}
}
