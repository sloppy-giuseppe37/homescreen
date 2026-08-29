package main

// mqtt.go — Manages the connection to the MQTT broker.
//
// Responsibilities:
//   - Connect to the broker and subscribe to all relevant topics
//   - Keep an in-memory cache of the latest value for each topic
//   - Notify listeners (SSE clients) when any value changes
//
// The cache is NOT persistent — if the server restarts, it rebuilds
// from MQTT retained messages (heating) and by requesting current state
// from zigbee2mqtt devices (lights). The broker is the source of truth.
//
// Connection handling is deliberately forgiving: the broker runs in its own
// jail and may be down when we start, restart under us, or vanish mid-session.
// None of that is fatal. We connect in the background and retry forever, every
// (re)connection re-subscribes and re-reads device state, and every wait on the
// broker is bounded so a stalled connection can never wedge an HTTP handler.
// While the broker is away the app still serves — handlers return 503 and the
// UI shows its offline screen — and recovers on its own once it returns.

import (
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// Timings for connection resilience. Every one of these is a bound on how long
// we will wait for the broker before deciding something is wrong and retrying.
const (
	connectRetryInterval = 5 * time.Second  // between initial connection attempts
	maxReconnectInterval = 10 * time.Second // ceiling on paho's reconnect backoff — it doubles up to this, and it is also the worst case for how long the UI stays offline after the broker returns
	connectTimeout       = 10 * time.Second // per TCP/CONNACK attempt
	writeTimeout         = 10 * time.Second // per outbound packet
	keepAlive            = 20 * time.Second // PINGREQ interval — detects a dead peer
	pingTimeout          = 10 * time.Second // no PINGRESP in this long means the link is gone
	opTimeout            = 10 * time.Second // subscribe/publish token waits
	initialConnectWait   = 3 * time.Second  // startup grace before we serve without the broker
	healthCheckInterval  = 15 * time.Second // watchdog tick
	subscribeRetryDelay  = 2 * time.Second  // between subscribe attempts
)

// errNotConnected is returned by publishes attempted while the broker is away.
// Handlers turn it into a 503, which the frontend already treats as "offline".
var errNotConnected = errors.New("MQTT: not connected to broker")

// MQTTClient wraps the MQTT connection, topic subscriptions, and state cache.
type MQTTClient struct {
	client mqtt.Client
	config *Config

	// cache stores the latest value for each MQTT topic.
	// Key: full MQTT topic string, Value: the payload string (e.g. "1", "21.0")
	cacheMu sync.RWMutex
	cache   map[string]string

	// connected tracks whether we have an active MQTT connection.
	connectedMu sync.RWMutex
	connected   bool

	// onChange is called whenever a topic value changes.
	// The SSE broadcaster plugs in here to push updates to browsers.
	onChange func(topic, value string)

	// onConnectionLost is called when the MQTT connection drops.
	// The main app uses this to disconnect SSE clients.
	onConnectionLost func()

	// connGen counts connections. Subscribing and re-reading device state can
	// outlive the connection that triggered them (a broker that flaps gives us
	// overlapping OnConnect callbacks), so that work carries the generation it
	// started under and bails out once it is no longer current.
	connGen atomic.Uint64

	// stop shuts down the connection watchdog. Closed once, by Disconnect.
	stop     chan struct{}
	stopOnce sync.Once

	// Connection history, for the /status page. None of it is load-bearing:
	// it exists so someone can tell at a glance whether the link to the broker
	// has been solid or flapping.
	statsMu       sync.RWMutex
	clientID      string
	connectedAt   time.Time // when the current connection came up
	lastLostAt    time.Time // when a connection last dropped
	connects      int
	disconnects   int
	subscriptions int   // topics subscribed on the current connection
	messages      int64 // messages received since startup
	lastMessageAt time.Time
}

// MQTTStats is a snapshot of the broker connection for the status page.
// The times are pointers so that "hasn't happened yet" is a null rather than
// year 1 — omitempty does nothing for a time.Time.
type MQTTStats struct {
	Broker        string     `json:"broker"`
	ClientID      string     `json:"client_id"`
	Connected     bool       `json:"connected"`
	ConnectedAt   *time.Time `json:"connected_at"`
	LastLostAt    *time.Time `json:"last_lost_at"`
	Connects      int        `json:"connects"`
	Disconnects   int        `json:"disconnects"`
	Subscriptions int        `json:"subscriptions"`
	Messages      int64      `json:"messages"`
	LastMessageAt *time.Time `json:"last_message_at"`
	CachedTopics  int        `json:"cached_topics"`
}

// optTime returns nil for a zero time, so callers can tell "never" from a date.
func optTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// Stats returns a snapshot of the connection state and history. Safe to call
// on a client that never connected, which is the case worth reporting.
func (m *MQTTClient) Stats() MQTTStats {
	m.statsMu.RLock()
	s := MQTTStats{
		ClientID:      m.clientID,
		ConnectedAt:   optTime(m.connectedAt),
		LastLostAt:    optTime(m.lastLostAt),
		Connects:      m.connects,
		Disconnects:   m.disconnects,
		Subscriptions: m.subscriptions,
		Messages:      m.messages,
		LastMessageAt: optTime(m.lastMessageAt),
	}
	m.statsMu.RUnlock()

	s.Connected = m.IsConnected()
	if m.config != nil {
		s.Broker = m.config.MQTT.Broker
	}
	m.cacheMu.RLock()
	s.CachedTopics = len(m.cache)
	m.cacheMu.RUnlock()
	return s
}

// NewMQTTClient creates a new client and starts connecting to the broker.
//
// It does not fail when the broker is unreachable: connecting happens in the
// background and retries forever, so a broker that is down at startup (a jail
// that boots after this one, say) only delays device control, it does not stop
// the server from coming up. An error here means the configuration itself is
// unusable, which no amount of retrying would fix.
func NewMQTTClient(cfg *Config, onChange func(topic, value string), onConnectionLost func()) (*MQTTClient, error) {
	if cfg.MQTT.Broker == "" {
		return nil, fmt.Errorf("MQTT: no broker configured")
	}

	m := &MQTTClient{
		config:           cfg,
		cache:            make(map[string]string),
		onChange:         onChange,
		onConnectionLost: onConnectionLost,
		stop:             make(chan struct{}),
	}

	// Configure the MQTT client options
	opts := mqtt.NewClientOptions()
	opts.AddBroker(cfg.MQTT.Broker)
	// The client ID includes the PID because MQTT brokers evict an existing
	// client when a new one connects with the same ID. Two homescreen processes
	// sharing an ID — a leftover instance during an upgrade, a dev copy running
	// beside the service — would otherwise kick each other off forever, a loop
	// no amount of reconnecting can escape.
	hostname, _ := os.Hostname()
	shortHost := strings.Split(hostname, ".")[0]
	m.clientID = fmt.Sprintf("homescreen-%s-%d", shortHost, os.Getpid())
	opts.SetClientID(m.clientID)

	// AutoReconnect recovers from a connection that drops; ConnectRetry keeps
	// the *first* connection attempt going when the broker isn't up yet.
	// Without the latter, a failed initial connect is permanent.
	opts.SetAutoReconnect(true)
	opts.SetMaxReconnectInterval(maxReconnectInterval)
	opts.SetConnectRetry(true)
	opts.SetConnectRetryInterval(connectRetryInterval)
	opts.SetConnectTimeout(connectTimeout)
	opts.SetWriteTimeout(writeTimeout)

	// Keepalive pings turn a silently dead link — a broker jail that was
	// destroyed, a NAT table that forgot us — into a connection-lost event we
	// can act on, instead of a socket that looks fine and never delivers.
	opts.SetKeepAlive(keepAlive)
	opts.SetPingTimeout(pingTimeout)

	// We re-subscribe on every connect, so we have no use for broker-side
	// session state and want a clean slate after a broker restart.
	opts.SetCleanSession(true)

	// Safety net for messages that arrive in the window between the connection
	// coming up and our subscriptions registering their handlers.
	opts.SetDefaultPublishHandler(func(c mqtt.Client, msg mqtt.Message) {
		m.handleMessage(msg.Topic(), string(msg.Payload()))
	})

	// When we first connect (or reconnect), subscribe to all topics.
	// This ensures we re-subscribe after a broker restart.
	opts.SetOnConnectHandler(func(c mqtt.Client) {
		gen := m.connGen.Add(1)
		log.Printf("MQTT: connected to %s", cfg.MQTT.Broker)
		// subscribeAll is what reports us connected, once it has subscriptions
		// in place. A live socket with no subscriptions can serve nothing but
		// an empty cache, so it is not "connected" as far as handlers care.
		m.subscribeAll(gen)
	})

	opts.SetConnectionLostHandler(func(c mqtt.Client, err error) {
		// Invalidate any subscribe/state-request work still running for the
		// connection that just died before announcing the loss.
		m.connGen.Add(1)
		log.Printf("MQTT: connection lost: %v", err)
		m.setConnected(false)
	})

	m.client = mqtt.NewClient(opts)

	// Connect in the background. We wait briefly for the usual case — broker
	// already up — so the first request finds state loaded rather than an
	// offline page, then carry on regardless; paho keeps retrying either way.
	token := m.client.Connect()
	if !m.WaitForConnection(initialConnectWait) {
		log.Printf("MQTT: broker %s not ready yet (%v) — serving anyway, retrying every %s",
			cfg.MQTT.Broker, tokenErr(token), connectRetryInterval)
	}

	go m.supervise()

	return m, nil
}

// supervise is the last line of defence for the connection. paho reconnects on
// its own in every case it recognises, but it can also stop trying — a broker
// that refuses the CONNECT, or an attempt aborted at an awkward moment — and
// nothing would then bring the connection back. This notices that state and
// starts a fresh connection, so recovery never depends on someone restarting
// the service. It also reconciles our connected flag with reality, in case a
// state change ever slips past the callbacks.
func (m *MQTTClient) supervise() {
	ticker := time.NewTicker(healthCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
			if m.client.IsConnectionOpen() {
				continue
			}
			// Only ever downgrade here — declaring ourselves connected stays
			// the connect handler's job, after it has re-subscribed.
			m.setConnected(false)

			// IsConnected covers connected, connecting and reconnecting, so it
			// only reads false once paho has given up entirely.
			if !m.client.IsConnected() {
				log.Printf("MQTT: not connected and not retrying — reconnecting to %s", m.config.MQTT.Broker)
				m.client.Connect()
			}
		}
	}
}

// setConnected records the connection state, firing the connection-lost
// callback on a true→false transition no matter where the change was noticed.
func (m *MQTTClient) setConnected(connected bool) {
	m.connectedMu.Lock()
	was := m.connected
	m.connected = connected
	m.connectedMu.Unlock()

	if !was || connected {
		return
	}

	m.statsMu.Lock()
	m.lastLostAt = time.Now()
	m.disconnects++
	m.connectedAt = time.Time{}
	m.statsMu.Unlock()

	if m.onConnectionLost != nil {
		m.onConnectionLost()
	}
}

// waitToken waits for an MQTT operation to complete, treating a broker that
// never answers as a failure rather than blocking the caller forever.
func waitToken(t mqtt.Token, timeout time.Duration) error {
	if !t.WaitTimeout(timeout) {
		return fmt.Errorf("timed out after %s", timeout)
	}
	return t.Error()
}

// tokenErr reports why a token has not completed successfully, for logging.
func tokenErr(t mqtt.Token) error {
	if err := t.Error(); err != nil {
		return err
	}
	return errors.New("connection attempt still in progress")
}

// subscribeAll subscribes to every MQTT topic we care about, as defined in the
// config file. After subscribing, it requests current state from all light
// entities (zigbee2mqtt doesn't retain state by default, so without this the
// cache is empty after restart).
//
// It runs on every connection, not just the first, which is what makes a broker
// restart a non-event: subscriptions and cached state are rebuilt from scratch.
// A failed subscribe is retried rather than logged and forgotten — giving up
// would leave that topic silently dead until the next reconnect. gen is the
// connection generation this work belongs to; if it is superseded, the work is
// abandoned to whichever connect handler came after it.
func (m *MQTTClient) subscribeAll(gen uint64) {
	// One SubscribeMultiple beats a round trip per topic, and gives the whole
	// set the same all-or-nothing retry.
	filters := make(map[string]byte)
	for _, topic := range m.allTopics() {
		filters[topic] = 1 // QoS 1 (at least once delivery)
	}

	for {
		if !m.current(gen) {
			log.Printf("MQTT: abandoning subscribe — connection changed")
			return
		}

		token := m.client.SubscribeMultiple(filters, func(c mqtt.Client, msg mqtt.Message) {
			m.handleMessage(msg.Topic(), string(msg.Payload()))
		})
		if err := waitToken(token, opTimeout); err != nil {
			log.Printf("MQTT: subscribe failed: %v — retrying in %s", err, subscribeRetryDelay)
			select {
			case <-m.stop:
				return
			case <-time.After(subscribeRetryDelay):
			}
			continue
		}

		log.Printf("MQTT: subscribed to %d topics", len(filters))
		break
	}

	m.statsMu.Lock()
	m.subscriptions = len(filters)
	m.connectedAt = time.Now()
	m.connects++
	m.statsMu.Unlock()

	// Only now can we answer requests with real state.
	m.setConnected(true)

	// Request current state from all light entities.
	// Heating topics use retained messages so they arrive with the subscription,
	// but zigbee2mqtt light state is not retained — we must ask for it.
	m.requestLightStates(gen)
}

// current reports whether gen is still the live connection, i.e. whether work
// started for that connection is still worth doing.
func (m *MQTTClient) current(gen uint64) bool {
	return m.connGen.Load() == gen && m.client.IsConnectionOpen()
}

// requestLightStates publishes to {prefix}/{entity}/get for each light entity.
// zigbee2mqtt responds by publishing the device's current state to the state topic,
// which populates our cache.
func (m *MQTTClient) requestLightStates(gen uint64) {
	prefix := m.config.MQTT.TopicPrefix
	for _, zone := range m.config.Zones {
		for _, light := range zone.Lights {
			for _, entity := range light.Entities {
				if !m.current(gen) {
					log.Printf("MQTT: abandoning state requests — connection changed")
					return
				}
				topic := light.GetTopic(prefix, entity)
				// Publishing {"state":""} to the /get topic asks zigbee2mqtt
				// to report the device's current state.
				payload := `{"state":""}`
				token := m.client.Publish(topic, 0, false, payload)
				if err := waitToken(token, opTimeout); err != nil {
					// Not worth retrying: the next reconnect asks again, and a
					// light that reports late just arrives late.
					log.Printf("MQTT: failed to request state from %s: %v", topic, err)
				}
			}
		}
	}
}

// ModeTopicName is the MQTT topic where the heating/cooling mode is stored.
// The value is "heating" or "cooling", published as a retained message.
const ModeTopicName = "homescreen/config/heating_mode"

// allTopics returns every MQTT topic string we need to subscribe to,
// derived from the config (heating rooms + lights).
func (m *MQTTClient) allTopics() []string {
	var topics []string

	// Global mode topic (heating vs cooling)
	topics = append(topics, ModeTopicName)

	for _, zone := range m.config.Zones {
		// Each heating room has 3 topics (power, temperature, quiet)
		for _, room := range zone.Heating {
			power, temp, quiet := room.HeatingTopics()
			topics = append(topics, power, temp, quiet)
		}

		// Each light has one state topic + one availability topic per entity
		for _, light := range zone.Lights {
			topics = append(topics, light.StateTopics(m.config.MQTT.TopicPrefix)...)
			topics = append(topics, light.AvailabilityTopics(m.config.MQTT.TopicPrefix)...)
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

	m.statsMu.Lock()
	m.messages++
	m.lastMessageAt = time.Now()
	m.statsMu.Unlock()

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
	return m.publish(topic, 1, true, value)
}

// PublishNonRetained publishes a message without the retained flag.
func (m *MQTTClient) PublishNonRetained(topic, value string) error {
	return m.publish(topic, 0, false, value)
}

// publish sends one message, failing fast instead of queueing when the broker
// is away. Commands are only meaningful now — replaying a light switch minutes
// later, once the connection is back, would be worse than reporting the failure
// and letting the caller (and the UI) see that we are offline.
func (m *MQTTClient) publish(topic string, qos byte, retained bool, value string) error {
	if m.client == nil || !m.IsConnected() {
		return errNotConnected
	}
	if err := waitToken(m.client.Publish(topic, qos, retained, value), opTimeout); err != nil {
		return fmt.Errorf("MQTT: publish to %s failed: %w", topic, err)
	}
	return nil
}

// Disconnect cleanly shuts down the MQTT connection and stops the watchdog.
func (m *MQTTClient) Disconnect() {
	if m.stop != nil {
		m.stopOnce.Do(func() { close(m.stop) })
	}
	if m.client != nil {
		m.client.Disconnect(250) // wait up to 250ms for in-flight messages
	}
}

// IsConnected returns whether the MQTT client has an active connection.
// Handlers use this to decide whether they can serve a request at all.
func (m *MQTTClient) IsConnected() bool {
	m.connectedMu.RLock()
	defer m.connectedMu.RUnlock()
	return m.connected
}

// WaitForConnection blocks until the broker connection is up or timeout
// elapses, reporting whether it came up. Nothing in the server needs to wait
// like this — it is for callers that have no offline behaviour to fall back on,
// such as tests.
func (m *MQTTClient) WaitForConnection(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if m.IsConnected() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}
