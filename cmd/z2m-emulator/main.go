// z2m-emulator pretends to be a zigbee2mqtt bridge, for development machines
// that have a broker but no Zigbee hardware.
//
// It answers {prefix}/{entity}/set and /get the way zigbee2mqtt does: it
// applies the command to an in-memory bulb and publishes the bulb's state to
// {prefix}/{entity}. Without it, homescreen's light POSTs succeed but no state
// ever comes back, so lights never change in the UI.
//
// Bulbs are the light entities in homescreen's config (read from the same
// paths homescreen searches, or -config), plus any entity that is sent a
// command. All start OFF at full brightness and are dimmable unless listed in
// -on-off. State is not persisted. On every (re)connection the emulator
// publishes every bulb's state, as zigbee2mqtt does when it starts, so the
// order the two programs start in doesn't matter.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"gopkg.in/yaml.v3"
)

// config is the subset of homescreen's config file the emulator needs.
type config struct {
	MQTT struct {
		Broker      string `yaml:"broker"`
		TopicPrefix string `yaml:"topic_prefix"`
	} `yaml:"mqtt"`
	Zones []struct {
		Lights []struct {
			Entities []string `yaml:"entities"`
		} `yaml:"lights"`
	} `yaml:"zones"`
}

type bulb struct {
	on         bool
	brightness int
	dimmable   bool
}

// payload is what zigbee2mqtt publishes for a light: brightness only for
// bulbs that support it.
func (b *bulb) payload() []byte {
	m := map[string]any{"state": "OFF"}
	if b.on {
		m["state"] = "ON"
	}
	if b.dimmable {
		m["brightness"] = b.brightness
	}
	out, _ := json.Marshal(m)
	return out
}

type emulator struct {
	client  mqtt.Client
	prefix  string
	onOff   map[string]bool
	latency time.Duration

	mu    sync.Mutex
	bulbs map[string]*bulb
}

func (e *emulator) bulb(entity string) *bulb {
	b, ok := e.bulbs[entity]
	if !ok {
		b = &bulb{brightness: 254, dimmable: !e.onOff[entity]}
		e.bulbs[entity] = b
	}
	return b
}

// publish sends a bulb's state. Not retained, matching zigbee2mqtt's default.
func (e *emulator) publish(entity string, payload []byte) {
	e.client.Publish(e.prefix+"/"+entity, 1, false, payload)
}

func (e *emulator) publishAll() {
	e.mu.Lock()
	states := make(map[string][]byte, len(e.bulbs))
	for entity, b := range e.bulbs {
		states[entity] = b.payload()
	}
	e.mu.Unlock()
	for entity, p := range states {
		e.publish(entity, p)
	}
	log.Printf("published state of %d bulbs", len(states))
}

// handle serves {prefix}/{entity}/set and {prefix}/{entity}/get.
func (e *emulator) handle(_ mqtt.Client, msg mqtt.Message) {
	rest := strings.TrimPrefix(msg.Topic(), e.prefix+"/")
	i := strings.LastIndex(rest, "/")
	if i < 0 {
		return
	}
	entity, verb := rest[:i], rest[i+1:]
	if (verb != "set" && verb != "get") || entity == "bridge" || strings.HasPrefix(entity, "bridge/") {
		return
	}

	e.mu.Lock()
	b := e.bulb(entity)
	if verb == "set" {
		var cmd struct {
			State      *string `json:"state"`
			Brightness *int    `json:"brightness"`
		}
		if err := json.Unmarshal(msg.Payload(), &cmd); err != nil {
			e.mu.Unlock()
			log.Printf("%s: ignoring bad command %q: %v", entity, msg.Payload(), err)
			return
		}
		if cmd.State != nil {
			switch strings.ToUpper(*cmd.State) {
			case "ON":
				b.on = true
			case "OFF":
				b.on = false
			case "TOGGLE":
				b.on = !b.on
			}
		}
		// Like a real bulb, brightness on a dimmable bulb also switches it:
		// on for 1–254, off for 0.
		if cmd.Brightness != nil && b.dimmable {
			n := max(0, min(254, *cmd.Brightness))
			if n == 0 {
				b.on = false
			} else {
				b.brightness, b.on = n, true
			}
		}
	}
	p := b.payload()
	e.mu.Unlock()

	log.Printf("%s %s %s → %s", entity, verb, msg.Payload(), p)
	time.AfterFunc(e.latency, func() { e.publish(entity, p) })
}

func findConfig() (string, error) {
	var candidates []string
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".config", "homescreen", "config.yaml"))
	}
	candidates = append(candidates, "/usr/local/etc/homescreen.yaml", "/etc/homescreen.yaml")
	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("no config file found; searched: %v", candidates)
}

func main() {
	configPath := flag.String("config", "", "homescreen config file (default: the paths homescreen searches)")
	onOff := flag.String("on-off", "", "comma-separated entities that switch but don't dim")
	latency := flag.Duration("latency", 100*time.Millisecond, "delay before reporting a bulb's new state")
	flag.Parse()

	path := *configPath
	if path == "" {
		var err error
		if path, err = findConfig(); err != nil {
			log.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatal(err)
	}
	var cfg config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		log.Fatalf("%s: %v", path, err)
	}
	if cfg.MQTT.Broker == "" {
		cfg.MQTT.Broker = "tcp://localhost:1883"
	}
	if cfg.MQTT.TopicPrefix == "" {
		cfg.MQTT.TopicPrefix = "zigbee2mqtt"
	}

	e := &emulator{
		prefix:  cfg.MQTT.TopicPrefix,
		onOff:   map[string]bool{},
		latency: *latency,
		bulbs:   map[string]*bulb{},
	}
	for _, entity := range strings.Split(*onOff, ",") {
		if entity = strings.TrimSpace(entity); entity != "" {
			e.onOff[entity] = true
		}
	}
	for _, zone := range cfg.Zones {
		for _, light := range zone.Lights {
			for _, entity := range light.Entities {
				e.bulb(entity)
			}
		}
	}
	log.Printf("config %s: %d bulbs under %q", path, len(e.bulbs), e.prefix)

	host, _ := os.Hostname()
	opts := mqtt.NewClientOptions().
		AddBroker(cfg.MQTT.Broker).
		SetClientID(fmt.Sprintf("z2m-emulator-%s-%d", strings.Split(host, ".")[0], os.Getpid())).
		SetConnectRetry(true).
		SetConnectRetryInterval(5 * time.Second).
		SetAutoReconnect(true).
		SetMaxReconnectInterval(10 * time.Second).
		SetOnConnectHandler(func(c mqtt.Client) {
			log.Printf("connected to %s", cfg.MQTT.Broker)
			// "#" rather than "+/set", since entity names may contain "/".
			// handle ignores everything but /set and /get, including our own
			// state publishes.
			if t := c.Subscribe(e.prefix+"/#", 1, e.handle); !t.WaitTimeout(10*time.Second) || t.Error() != nil {
				log.Fatalf("subscribe failed: %v", t.Error()) // let the supervisor restart us
			}
			go e.publishAll()
		}).
		SetConnectionLostHandler(func(_ mqtt.Client, err error) {
			log.Printf("connection lost: %v", err)
		})
	e.client = mqtt.NewClient(opts)
	e.client.Connect() // with ConnectRetry this retries in the background forever

	select {}
}
