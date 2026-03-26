package main

// config.go — Loads the YAML configuration file that tells us:
//   - Where the MQTT broker is
//   - What zones/rooms/lights exist and their MQTT topic mappings
//
// Config is searched in order:
//   1. ~/.config/homescreen/config.yaml       (user-level, for dev/desktop)
//   2. /usr/local/etc/homescreen.yaml  (FreeBSD pkg convention)
//   3. /etc/homescreen.yaml                   (system-level, for Docker/servers)

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration structure.
type Config struct {
	MQTT    MQTTConfig    `yaml:"mqtt"`
	Zones   []ZoneConfig  `yaml:"zones"`
	Scenes  []SceneConfig `yaml:"scenes"`
	BaseURL string        `yaml:"base_url"` // Full URL of the app (e.g. "https://example.com:8000")
}

// MQTTConfig holds the broker connection details.
type MQTTConfig struct {
	Broker      string `yaml:"broker"`       // e.g. "tcp://localhost:1883"
	TopicPrefix string `yaml:"topic_prefix"` // zigbee2mqtt topic prefix, e.g. "zigbee2mqtt"
}

// ZoneConfig represents a physical zone in the house (e.g. "Upstairs").
// Each zone can have heating units and/or lights.
type ZoneConfig struct {
	Name    string         `yaml:"name"`
	Secret  bool           `yaml:"secret"`  // if true, hidden unless user enables secret mode
	Heating []HeatingRoom  `yaml:"heating"` // may be empty
	Lights  []LightConfig  `yaml:"lights"`  // may be empty
}

// SceneConfig defines a preset scene that publishes a batch of MQTT messages.
type SceneConfig struct {
	Name        string        `yaml:"name"`        // display name, e.g. "Good Morning"
	Description string        `yaml:"description"` // short description
	Icon        string        `yaml:"icon"`        // lucide icon name without "icon-" prefix
	Actions     []SceneAction `yaml:"actions"`     // MQTT messages to publish
}

// SceneAction is a single MQTT publish within a scene.
type SceneAction struct {
	Topic    string `yaml:"topic"`
	Payload  string `yaml:"payload"`
	Retained bool   `yaml:"retained"`
}

// HeatingRoom maps a room name to its Faikin unit ID.
// The unit ID is used to construct MQTT topics like:
//   HomeKit/<unit_id>_Thermostat/Thermostat/TargetTemperature
type HeatingRoom struct {
	Name   string `yaml:"name"`    // display name, e.g. "Bedroom"
	UnitID string `yaml:"unit_id"` // e.g. "BedroomFaikin"
}

// LightConfig maps a UI toggle to one or more zigbee2mqtt entities.
// A single toggle may control multiple physical lights (e.g. a room with
// separate ceiling and lamp entities). The toggle shows ON if any entity
// is on, and OFF only when all entities are off.
type LightConfig struct {
	Name     string   `yaml:"name"`     // display name, e.g. "Kitchen"
	Entities []string `yaml:"entities"` // zigbee2mqtt entity IDs, e.g. ["fairy_lights", "kitchen_table_1"]
}

// StateTopics returns the MQTT topics to subscribe to for reading state.
// Each entity publishes state to "{prefix}/{entity}".
func (l LightConfig) StateTopics(prefix string) []string {
	topics := make([]string, len(l.Entities))
	for i, e := range l.Entities {
		topics[i] = prefix + "/" + e
	}
	return topics
}

// SetTopic returns the MQTT topic to publish commands to for an entity.
// Commands are sent to "{prefix}/{entity}/set".
func (l LightConfig) SetTopic(prefix, entity string) string {
	return prefix + "/" + entity + "/set"
}

// GetTopic returns the MQTT topic to request current state from an entity.
// zigbee2mqtt responds by publishing the device's current state to the state topic.
func (l LightConfig) GetTopic(prefix, entity string) string {
	return prefix + "/" + entity + "/get"
}

// AvailabilityTopics returns the MQTT availability topics for each entity.
// zigbee2mqtt publishes availability to "{prefix}/{entity}/availability".
func (l LightConfig) AvailabilityTopics(prefix string) []string {
	topics := make([]string, len(l.Entities))
	for i, e := range l.Entities {
		topics[i] = prefix + "/" + e + "/availability"
	}
	return topics
}

// HeatingTopics returns the three MQTT topics for a heating unit.
func (r HeatingRoom) HeatingTopics() (power, temp, quiet string) {
	prefix := "HomeKit/" + r.UnitID
	power = prefix + "_Thermostat/Thermostat/TargetHeatingCoolingState"
	temp = prefix + "_Thermostat/Thermostat/TargetTemperature"
	quiet = prefix + "_IndoorQuiet/Switch/On"
	return
}

// LoadConfig reads and parses the YAML config file.
// It searches these paths in order and uses the first one found:
//   1. ~/.config/homescreen/config.yaml
//   2. /etc/homescreen.yaml
func LoadConfig() (*Config, error) {
	path, err := findConfigFile()
	if err != nil {
		return nil, err
	}
	return LoadConfigFrom(path)
}

// LoadConfigFrom reads and parses a YAML config file at a specific path.
func LoadConfigFrom(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read config file %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("cannot parse config file: %w", err)
	}

	if cfg.MQTT.Broker == "" {
		return nil, fmt.Errorf("mqtt.broker must be set in config")
	}

	return &cfg, nil
}

// findConfigFile returns the path to the first config file found.
// Search order: ~/.config/homescreen/config.yaml, /usr/local/etc/homescreen.yaml, /etc/homescreen.yaml
func findConfigFile() (string, error) {
	// Build the list of candidate paths
	var candidates []string

	// 1. User-level config (may fail if HOME is unset, that's fine)
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".config", "homescreen", "config.yaml"))
	}

	// 2. FreeBSD pkg convention
	candidates = append(candidates, "/usr/local/etc/homescreen.yaml")

	// 3. System-level config (good for Docker / servers)
	candidates = append(candidates, "/etc/homescreen.yaml")

	// Return the first one that exists
	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}

	return "", fmt.Errorf("no config file found; searched: %v", candidates)
}
