package main

// mqtt.go — Manages the connection to the MQTT broker.
//
// Responsibilities:
//   - Connect to the broker and subscribe to all relevant topics
//   - Keep an in-memory cache of the latest value for each topic
//   - Notify listeners (SSE clients) when any value changes
//
// The cache is NOT persistent — if the server restarts, it rebuilds
// from MQTT retained messages. The broker is the source of truth.

import (
	"fmt"
	"log"
	"strings"
	"sync"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// MQTTClient wraps the MQTT connection, topic subscriptions, and state cache.
type MQTTClient struct {
	client mqtt.Client
	config *Config

	// cache stores the latest value for each MQTT topic.
	// Key: full MQTT topic string, Value: the payload string (e.g. "1", "21.0")
	cacheMu sync.RWMutex
	cache   map[string]string

	// onChange is called whenever a topic value changes.
	// The SSE broadcaster plugs in here to push updates to browsers.
	onChange func(topic, value string)
}

// NewMQTTClient creates a new client, connects to the broker, and subscribes
// to all topics defined in the config.
func NewMQTTClient(cfg *Config, onChange func(topic, value string)) (*MQTTClient, error) {
	m := &MQTTClient{
		config:   cfg,
		cache:    make(map[string]string),
		onChange: onChange,
	}

	// Configure the MQTT client options
	opts := mqtt.NewClientOptions()
	opts.AddBroker(cfg.MQTT.Broker)
	opts.SetClientID("homescreen-server")
	opts.SetAutoReconnect(true)

	// When we first connect (or reconnect), subscribe to all topics.
	// This ensures we re-subscribe after a broker restart.
	opts.SetOnConnectHandler(func(c mqtt.Client) {
		log.Printf("MQTT: connected to %s", cfg.MQTT.Broker)
		m.subscribeAll()
	})

	opts.SetConnectionLostHandler(func(c mqtt.Client, err error) {
		log.Printf("MQTT: connection lost: %v", err)
	})

	// Create and connect
	m.client = mqtt.NewClient(opts)
	token := m.client.Connect()
	token.Wait()
	if err := token.Error(); err != nil {
		return nil, fmt.Errorf("MQTT connect failed: %w", err)
	}

	return m, nil
}

// subscribeAll subscribes to every MQTT topic we care about,
// as defined in the config file.
func (m *MQTTClient) subscribeAll() {
	// Build a list of all topics we need to listen to
	topics := m.allTopics()

	for _, topic := range topics {
		// Subscribe to each topic with QoS 1 (at least once delivery)
		t := topic // capture for closure
		token := m.client.Subscribe(t, 1, func(c mqtt.Client, msg mqtt.Message) {
			m.handleMessage(msg.Topic(), string(msg.Payload()))
		})
		token.Wait()
		if err := token.Error(); err != nil {
			log.Printf("MQTT: failed to subscribe to %s: %v", t, err)
		} else {
			log.Printf("MQTT: subscribed to %s", t)
		}
	}
}

// allTopics returns every MQTT topic string we need to subscribe to,
// derived from the config (heating rooms + lights).
func (m *MQTTClient) allTopics() []string {
	var topics []string

	for _, zone := range m.config.Zones {
		// Each heating room has 3 topics (power, temperature, quiet)
		for _, room := range zone.Heating {
			power, temp, quiet := room.HeatingTopics()
			topics = append(topics, power, temp, quiet)
		}

		// Each light has 1 topic
		for _, light := range zone.Lights {
			topics = append(topics, light.Topic)
		}
	}

	return topics
}

// handleMessage is called for every incoming MQTT message.
// It updates the cache and notifies listeners.
func (m *MQTTClient) handleMessage(topic, value string) {
	// Trim whitespace — some MQTT publishers add trailing newlines
	value = strings.TrimSpace(value)

	log.Printf("MQTT: %s = %s", topic, value)

	// Update the cache
	m.cacheMu.Lock()
	m.cache[topic] = value
	m.cacheMu.Unlock()

	// Notify listeners (this sends SSE events to all connected browsers)
	if m.onChange != nil {
		m.onChange(topic, value)
	}
}

// GetValue reads the cached value for a topic.
// Returns the value and whether it was found.
func (m *MQTTClient) GetValue(topic string) (string, bool) {
	m.cacheMu.RLock()
	defer m.cacheMu.RUnlock()
	val, ok := m.cache[topic]
	return val, ok
}

// Publish sends a value to an MQTT topic.
// retained=true means the broker remembers this value for new subscribers.
func (m *MQTTClient) Publish(topic, value string) error {
	token := m.client.Publish(topic, 1, true, value)
	token.Wait()
	return token.Error()
}

// Disconnect cleanly shuts down the MQTT connection.
func (m *MQTTClient) Disconnect() {
	m.client.Disconnect(250) // wait up to 250ms for in-flight messages
}
