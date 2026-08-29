package main

// mqtt_test.go — Tests for MQTT connection resilience.
//
// The broker runs in its own jail and can be down at startup, restart under us,
// or disappear mid-session. These tests cover each of those, using a throwaway
// Mosquitto instance on a spare port that the test itself starts and kills.
// Tests needing that broker are skipped when SKIP_INTEGRATION is set or when
// mosquitto isn't installed.

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"text/template"
	"time"
)

// freePort returns a TCP port that nothing is listening on.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("cannot find a free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// testBroker is a Mosquitto instance the test controls, so it can be stopped
// and started to simulate the broker's jail going away and coming back.
type testBroker struct {
	t    *testing.T
	port int
	conf string
	cmd  *exec.Cmd
}

// newTestBroker prepares a broker on a free port without starting it.
func newTestBroker(t *testing.T) *testBroker {
	t.Helper()
	if os.Getenv("SKIP_INTEGRATION") != "" {
		t.Skip("SKIP_INTEGRATION is set")
	}
	if _, err := exec.LookPath("mosquitto"); err != nil {
		t.Skip("mosquitto not installed")
	}

	port := freePort(t)
	conf := filepath.Join(t.TempDir(), "mosquitto.conf")
	// Anonymous access on a loopback listener — Mosquitto 2.x refuses both by
	// default, and the test broker holds nothing worth protecting.
	body := fmt.Sprintf("listener %d 127.0.0.1\nallow_anonymous true\npersistence false\n", port)
	if err := os.WriteFile(conf, []byte(body), 0o644); err != nil {
		t.Fatalf("cannot write broker config: %v", err)
	}

	b := &testBroker{t: t, port: port, conf: conf}
	t.Cleanup(b.stop)
	return b
}

func (b *testBroker) url() string { return fmt.Sprintf("tcp://127.0.0.1:%d", b.port) }

// start launches the broker and waits for it to accept connections.
func (b *testBroker) start() {
	b.t.Helper()
	cmd := exec.Command("mosquitto", "-c", b.conf)
	if err := cmd.Start(); err != nil {
		b.t.Fatalf("cannot start mosquitto: %v", err)
	}
	b.cmd = cmd

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", b.port), 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	b.t.Fatalf("mosquitto did not start listening on port %d", b.port)
}

// stop kills the broker, mimicking its jail going away.
func (b *testBroker) stop() {
	if b.cmd == nil || b.cmd.Process == nil {
		return
	}
	_ = b.cmd.Process.Kill()
	_, _ = b.cmd.Process.Wait()
	b.cmd = nil
}

// waitFor polls cond until it holds or timeout elapses.
func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestNewMQTTClient_NoBroker verifies that an unreachable broker is not a
// startup failure — the server has to come up so it can serve its offline page
// and connect later, which is the whole point of the retry loop.
func TestNewMQTTClient_NoBroker(t *testing.T) {
	cfg := testConfig()
	cfg.MQTT.Broker = fmt.Sprintf("tcp://127.0.0.1:%d", freePort(t))

	client, err := NewMQTTClient(cfg, nil, nil)
	if err != nil {
		t.Fatalf("expected no error with an unreachable broker, got %v", err)
	}
	defer client.Disconnect()

	if client.IsConnected() {
		t.Error("expected IsConnected() to be false with no broker")
	}
	// Handlers depend on publishes failing fast rather than queueing while the
	// broker is away; a blocking publish here would hang an HTTP request.
	done := make(chan error, 1)
	go func() { done <- client.Publish("some/topic", "1") }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("expected Publish to fail while disconnected")
		}
	case <-time.After(2 * time.Second):
		t.Error("Publish blocked while disconnected")
	}
}

// TestNewMQTTClient_NoBrokerConfigured verifies that unusable configuration
// still fails loudly — retrying cannot fix a missing broker address.
func TestNewMQTTClient_NoBrokerConfigured(t *testing.T) {
	cfg := testConfig()
	cfg.MQTT.Broker = ""

	if _, err := NewMQTTClient(cfg, nil, nil); err == nil {
		t.Error("expected an error when no broker is configured")
	}
}

// TestMQTTClient_ConnectsWhenBrokerAppears covers the reboot ordering problem:
// homescreen starts before the broker's jail is up, and must connect by itself
// once the broker appears.
func TestMQTTClient_ConnectsWhenBrokerAppears(t *testing.T) {
	broker := newTestBroker(t)

	cfg := testConfig()
	cfg.MQTT.Broker = broker.url()

	client, err := NewMQTTClient(cfg, nil, nil)
	if err != nil {
		t.Fatalf("NewMQTTClient: %v", err)
	}
	defer client.Disconnect()

	if client.IsConnected() {
		t.Fatal("connected before the broker was started")
	}

	broker.start()

	if !client.WaitForConnection(20 * time.Second) {
		t.Fatal("client did not connect after the broker came up")
	}
}

// TestMQTTClient_RecoversFromBrokerRestart verifies the mid-session case: the
// broker dies, we notice, and once it returns we reconnect *and* re-subscribe.
// The re-subscription is what the message assertion is really testing — a
// reconnect that lost its subscriptions would look connected but go deaf.
func TestMQTTClient_RecoversFromBrokerRestart(t *testing.T) {
	broker := newTestBroker(t)
	broker.start()

	cfg := testConfig()
	cfg.MQTT.Broker = broker.url()

	received := make(chan string, 16)
	lost := make(chan struct{}, 16)
	client, err := NewMQTTClient(cfg, func(topic, value string) {
		received <- topic + "=" + value
	}, func() {
		lost <- struct{}{}
	})
	if err != nil {
		t.Fatalf("NewMQTTClient: %v", err)
	}
	defer client.Disconnect()

	if !client.WaitForConnection(20 * time.Second) {
		t.Fatal("client did not connect to the test broker")
	}

	// The mode topic is one of the topics allTopics() subscribes to.
	if err := client.Publish(ModeTopicName, "heating"); err != nil {
		t.Fatalf("publish before restart: %v", err)
	}
	select {
	case <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("no message before the broker restart — subscriptions never worked")
	}

	broker.stop()

	if !waitFor(30*time.Second, func() bool { return !client.IsConnected() }) {
		t.Fatal("client still reports connected after the broker was killed")
	}
	select {
	case <-lost:
	case <-time.After(5 * time.Second):
		t.Fatal("connection-lost callback never fired — SSE clients would hang")
	}

	broker.start()

	if !client.WaitForConnection(60 * time.Second) {
		t.Fatal("client did not reconnect after the broker came back")
	}

	// Drain anything buffered from before, then prove messages flow again.
	for len(received) > 0 {
		<-received
	}
	if err := client.Publish(ModeTopicName, "cooling"); err != nil {
		t.Fatalf("publish after restart: %v", err)
	}
	select {
	case <-received:
	case <-time.After(10 * time.Second):
		t.Fatal("no message after reconnect — subscriptions were not restored")
	}
}

// TestServer_ServesAndRecoversAroundBrokerOutage is the whole point of the
// exercise, end to end: the server comes up with no broker, answers 503 (which
// the frontend and service worker render as the offline screen) instead of
// dying, and starts serving real pages once the broker appears — then survives
// the broker going away and coming back again, with no restart.
func TestServer_ServesAndRecoversAroundBrokerOutage(t *testing.T) {
	broker := newTestBroker(t)

	cfg := testConfig()
	cfg.MQTT.Broker = broker.url()

	broadcaster := NewSSEBroadcaster()
	var app *App
	client, err := NewMQTTClient(cfg, func(topic, value string) {
		if app != nil {
			if event := app.TopicToEvent(topic, value); event != "" {
				broadcaster.Broadcast(event)
			}
		}
	}, broadcaster.DisconnectAll)
	if err != nil {
		t.Fatalf("NewMQTTClient: %v", err)
	}
	defer client.Disconnect()

	app = &App{
		Config:      cfg,
		MQTT:        client,
		Broadcaster: broadcaster,
		Template:    template.Must(template.ParseFiles("templates/index.html")),
	}
	mux := http.NewServeMux()
	app.SetupRoutes(mux)
	server := httptest.NewServer(mux)
	defer server.Close()

	get := func() int {
		resp, err := http.Get(server.URL + "/")
		if err != nil {
			t.Fatalf("GET /: %v", err)
		}
		defer resp.Body.Close()
		// Read the page out rather than hanging up mid-render, which the
		// server would otherwise log as a write failure.
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	if code := get(); code != http.StatusServiceUnavailable {
		t.Fatalf("with no broker: got %d, want 503", code)
	}

	broker.start()
	if !client.WaitForConnection(20 * time.Second) {
		t.Fatal("client did not connect after the broker came up")
	}
	if code := get(); code != http.StatusOK {
		t.Fatalf("with the broker up: got %d, want 200", code)
	}

	broker.stop()
	if !waitFor(30*time.Second, func() bool { return !client.IsConnected() }) {
		t.Fatal("client still reports connected after the broker was killed")
	}
	if code := get(); code != http.StatusServiceUnavailable {
		t.Fatalf("after the broker died: got %d, want 503", code)
	}

	broker.start()
	if !client.WaitForConnection(60 * time.Second) {
		t.Fatal("client did not reconnect after the broker came back")
	}
	if code := get(); code != http.StatusOK {
		t.Fatalf("after the broker returned: got %d, want 200", code)
	}
}
