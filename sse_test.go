package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestSSEBroadcaster_AddRemove verifies that clients can be
// added and removed from the broadcaster.
func TestSSEBroadcaster_AddRemove(t *testing.T) {
	b := NewSSEBroadcaster()

	// Add two clients
	ch1 := b.addClient()
	ch2 := b.addClient()

	b.mu.Lock()
	if len(b.clients) != 2 {
		t.Errorf("expected 2 clients, got %d", len(b.clients))
	}
	b.mu.Unlock()

	// Remove one
	b.removeClient(ch1)

	b.mu.Lock()
	if len(b.clients) != 1 {
		t.Errorf("expected 1 client after removal, got %d", len(b.clients))
	}
	b.mu.Unlock()

	// Clean up
	b.removeClient(ch2)
}

// TestSSEBroadcaster_Broadcast verifies that messages are
// delivered to all connected clients.
func TestSSEBroadcaster_Broadcast(t *testing.T) {
	b := NewSSEBroadcaster()

	ch1 := b.addClient()
	ch2 := b.addClient()
	defer b.removeClient(ch1)
	defer b.removeClient(ch2)

	// Broadcast a message
	b.Broadcast(`{"test": true}`)

	// Both clients should receive it
	select {
	case msg := <-ch1:
		if msg != `{"test": true}` {
			t.Errorf("ch1 got %q, want %q", msg, `{"test": true}`)
		}
	case <-time.After(time.Second):
		t.Error("ch1 did not receive message")
	}

	select {
	case msg := <-ch2:
		if msg != `{"test": true}` {
			t.Errorf("ch2 got %q, want %q", msg, `{"test": true}`)
		}
	case <-time.After(time.Second):
		t.Error("ch2 did not receive message")
	}
}

// TestSSEBroadcaster_SlowClient verifies that a slow client
// (full buffer) doesn't block other clients.
func TestSSEBroadcaster_SlowClient(t *testing.T) {
	b := NewSSEBroadcaster()

	// Create a slow client - fill its buffer completely
	slowCh := b.addClient()
	for i := 0; i < 64; i++ { // buffer size is 64
		slowCh <- "filler"
	}

	// Create a fast client
	fastCh := b.addClient()
	defer b.removeClient(slowCh)
	defer b.removeClient(fastCh)

	// Broadcast should succeed without blocking
	done := make(chan bool, 1)
	go func() {
		b.Broadcast(`{"important": true}`)
		done <- true
	}()

	select {
	case <-done:
		// Good - broadcast didn't block
	case <-time.After(time.Second):
		t.Fatal("Broadcast blocked on slow client")
	}

	// Fast client should still get the message
	select {
	case msg := <-fastCh:
		if msg != `{"important": true}` {
			t.Errorf("fast client got %q", msg)
		}
	case <-time.After(time.Second):
		t.Error("fast client did not receive message")
	}
}

// TestSSEBroadcaster_NoClients verifies broadcasting with
// no clients doesn't panic.
func TestSSEBroadcaster_NoClients(t *testing.T) {
	b := NewSSEBroadcaster()
	// Should not panic
	b.Broadcast(`{"test": true}`)
}

// TestSSEBroadcaster_DisconnectAll verifies that DisconnectAll
// closes all client channels and removes them from the set.
func TestSSEBroadcaster_DisconnectAll(t *testing.T) {
	b := NewSSEBroadcaster()

	// Add three clients
	ch1 := b.addClient()
	ch2 := b.addClient()
	ch3 := b.addClient()

	b.mu.Lock()
	if len(b.clients) != 3 {
		t.Errorf("expected 3 clients, got %d", len(b.clients))
	}
	b.mu.Unlock()

	// Disconnect all
	b.DisconnectAll()

	// All clients should be removed
	b.mu.Lock()
	if len(b.clients) != 0 {
		t.Errorf("expected 0 clients after DisconnectAll, got %d", len(b.clients))
	}
	b.mu.Unlock()

	// All channels should be closed (reading returns immediately with zero value)
	select {
	case _, ok := <-ch1:
		if ok {
			t.Error("ch1 should be closed")
		}
	default:
		t.Error("ch1 should be closed and readable")
	}

	select {
	case _, ok := <-ch2:
		if ok {
			t.Error("ch2 should be closed")
		}
	default:
		t.Error("ch2 should be closed and readable")
	}

	select {
	case _, ok := <-ch3:
		if ok {
			t.Error("ch3 should be closed")
		}
	default:
		t.Error("ch3 should be closed and readable")
	}
}

// TestSSEBroadcaster_DisconnectAll_Empty verifies DisconnectAll
// doesn't panic when there are no clients.
func TestSSEBroadcaster_DisconnectAll_Empty(t *testing.T) {
	b := NewSSEBroadcaster()
	// Should not panic
	b.DisconnectAll()
}

// TestSSEBroadcaster_RemoveAfterDisconnectAll verifies that a handler
// cleaning up after DisconnectAll doesn't close its channel a second time.
func TestSSEBroadcaster_RemoveAfterDisconnectAll(t *testing.T) {
	b := NewSSEBroadcaster()
	ch := b.addClient()
	b.DisconnectAll()
	// Should not panic
	b.removeClient(ch)
}

// TestSSEBroadcaster_ServeHTTPEndsOnDisconnectAll verifies that an open
// stream ends when DisconnectAll is called, rather than spinning on the
// closed channel writing empty events.
func TestSSEBroadcaster_ServeHTTPEndsOnDisconnectAll(t *testing.T) {
	b := NewSSEBroadcaster()
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/events", nil)

	done := make(chan struct{})
	go func() {
		b.ServeHTTP(w, req, func(ch chan string) {})
		close(done)
	}()

	// Wait for the handler to register before disconnecting it
	deadline := time.Now().Add(time.Second)
	for b.ClientCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("client never registered")
		}
		time.Sleep(time.Millisecond)
	}
	b.DisconnectAll()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ServeHTTP kept running after DisconnectAll")
	}
	if strings.Contains(w.Body.String(), "data: \n") {
		t.Error("stream contains empty data events")
	}
}
