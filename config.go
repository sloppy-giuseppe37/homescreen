package main

// config.go — Loads the YAML configuration file that tells us:
//   - Where the MQTT broker is
//   - What zones/rooms/lights exist and their MQTT topic mappings
//
// Config is searched in order:
//   1. ~/.config/homescreen/config.yaml  (user-level, for dev/desktop)
//   2. /etc/homescreen.yaml              (system-level, for Docker/servers)

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration structure.
type Config struct {
	MQTT  MQTTConfig   `yaml:"mqtt"`
	Zones []ZoneConfig `yaml:"zones"`
}

// MQTTConfig holds the broker connection details.
type MQTTConfig struct {
	Broker string `yaml:"broker"` // e.g. "tcp://localhost:1883"
}

// ZoneConfig represents a physical zone in the house (e.g. "Upstairs").
// Each zone can have heating units and/or lights.
type ZoneConfig struct {
	Name    string         `yaml:"name"`
	Heating []HeatingRoom  `yaml:"heating"` // may be empty
	Lights  []LightConfig  `yaml:"lights"`  // may be empty
}

// HeatingRoom maps a room name to its Faikin unit ID.
// The unit ID is used to construct MQTT topics like:
//   HomeKit/<unit_id>_Thermostat/Thermostat/TargetTemperature
type HeatingRoom struct {
	Name   string `yaml:"name"`    // display name, e.g. "Bedroom"
	UnitID string `yaml:"unit_id"` // e.g. "BedroomFaikin"
}

// LightConfig maps a light name to its MQTT topic.
// The topic is the full MQTT topic path — no pattern, just a literal string.
type LightConfig struct {
	Name  string `yaml:"name"`  // display name, e.g. "Kids' Room — Lamp"
	Topic string `yaml:"topic"` // full MQTT topic, e.g. "HomeKit/KidsLamp/Lightbulb/On"
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
// Search order: ~/.config/homescreen/config.yaml, /etc/homescreen.yaml
func findConfigFile() (string, error) {
	// Build the list of candidate paths
	var candidates []string

	// 1. User-level config (may fail if HOME is unset, that's fine)
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".config", "homescreen", "config.yaml"))
	}

	// 2. System-level config (good for Docker / servers)
	candidates = append(candidates, "/etc/homescreen.yaml")

	// Return the first one that exists
	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}

	return "", fmt.Errorf("no config file found; searched: %v", candidates)
}
