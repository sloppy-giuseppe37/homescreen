package main

// sse.go — Server-Sent Events (SSE) broadcaster.
//
// SSE is a simple protocol for pushing data from server to browser:
//   - Browser opens a long-lived GET request to /api/events
//   - Server sends "data: ...\n\n" lines whenever state changes
//   - Browser reconnects automatically if the connection drops
//
// This is simpler than WebSockets and sufficient for our use case
// (we only need server→client push; client→server uses POST requests).

import (
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// SSEBroadcaster manages all connected SSE clients.
// When a value changes, it sends the update to every connected browser.
type SSEBroadcaster struct {
	// clients is a set of channels, one per connected browser.
	// We send JSON strings into these channels.
	mu      sync.Mutex
	clients map[chan string]bool
}

// NewSSEBroadcaster creates a new broadcaster with no connected clients.
func NewSSEBroadcaster() *SSEBroadcaster {
	return &SSEBroadcaster{
		clients: make(map[chan string]bool),
	}
}

// addClient registers a new SSE client and returns its channel.
func (b *SSEBroadcaster) addClient() chan string {
	ch := make(chan string, 64) // buffered so slow clients don't block others
	b.mu.Lock()
	b.clients[ch] = true
	count := len(b.clients)
	b.mu.Unlock()
	log.Printf("SSE: client connected (%d total)", count)
	return ch
}

// removeClient unregisters an SSE client.
func (b *SSEBroadcaster) removeClient(ch chan string) {
	b.mu.Lock()
	delete(b.clients, ch)
	count := len(b.clients)
	b.mu.Unlock()
	close(ch)
	log.Printf("SSE: client disconnected (%d remaining)", count)
}

// Broadcast sends a JSON string to all connected clients.
// If a client's buffer is full (it's too slow), we skip it
// rather than blocking the whole system.
func (b *SSEBroadcaster) Broadcast(jsonData string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for ch := range b.clients {
		select {
		case ch <- jsonData:
			// sent successfully
		default:
			// client buffer full, skip this update
			log.Printf("SSE: dropping message for slow client")
		}
	}
}

// ServeHTTP handles the GET /api/events endpoint.
// It sends the initial state snapshot, then streams updates.
func (b *SSEBroadcaster) ServeHTTP(w http.ResponseWriter, r *http.Request, sendSnapshot func(ch chan string)) {
	// SSE requires the response to stay open and stream data.
	// We need the Flusher interface to push data immediately.
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", http.StatusInternalServerError)
		return
	}

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// Register this client
	ch := b.addClient()
	defer b.removeClient(ch)

	// Send the full current state as the initial snapshot.
	// This is called while the client channel is registered, so any
	// MQTT messages that arrive during the snapshot will be queued in ch.
	sendSnapshot(ch)
	flusher.Flush()

	// Send a heartbeat comment every 15 seconds to keep the connection
	// alive through proxies and mobile network managers that kill idle
	// TCP connections.
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	// Stream updates until the client disconnects
	for {
		select {
		case msg := <-ch:
			// Write SSE format: "data: {json}\n\n"
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()

		case <-heartbeat.C:
			// SSE comment line — ignored by EventSource but keeps the
			// TCP connection alive.
			fmt.Fprintf(w, ": heartbeat\n\n")
			flusher.Flush()

		case <-r.Context().Done():
			// Client disconnected (closed browser tab, etc.)
			return
		}
	}
}
