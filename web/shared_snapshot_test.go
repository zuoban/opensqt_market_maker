package web

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"opensqt/position"
)

type countedJSON struct {
	calls atomic.Int32
	err   error
}

func (v *countedJSON) MarshalJSON() ([]byte, error) {
	v.calls.Add(1)
	return []byte(`{"sequence":42}`), v.err
}

type preparedWSConnection struct {
	*fakeWSConnection
	prepared *websocket.PreparedMessage
}

func (c *preparedWSConnection) WritePreparedMessage(message *websocket.PreparedMessage) error {
	c.prepared = message
	return nil
}

func TestBroadcastEncodesOnceForConcurrentConnections(t *testing.T) {
	h := newHub()
	defer h.close()
	value := &countedJSON{}
	const count = 8
	clients := make([]*wsClient, count)
	connections := make([]*preparedWSConnection, count)
	for i := range clients {
		connections[i] = &preparedWSConnection{fakeWSConnection: newFakeWSConnection()}
		clients[i] = newWSClient(h, connections[i])
		h.register(clients[i])
	}
	h.broadcast(value)
	if value.calls.Load() != 0 {
		t.Fatal("broadcast encoded JSON instead of leaving it to the writers")
	}
	var wg sync.WaitGroup
	for _, client := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := client.writeJSON(<-client.send); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := value.calls.Load(); got != 1 {
		t.Fatalf("JSON encoding calls = %d, want 1 for %d connections", got, count)
	}
	for _, conn := range connections {
		if conn.prepared == nil || conn.prepared != connections[0].prepared {
			t.Fatal("connections did not reuse the same prepared message")
		}
	}
}

func TestSharedMessagePropagatesEncodingFailure(t *testing.T) {
	want := errors.New("cannot encode snapshot")
	value := &countedJSON{err: want}
	message := &sharedWSMessage{value: value}
	for range 2 {
		conn := &preparedWSConnection{fakeWSConnection: newFakeWSConnection()}
		client := newWSClient(newHub(), conn)
		if err := client.writeJSON(message); !errors.Is(err, want) {
			t.Fatalf("write error = %v, want %v", err, want)
		}
		if conn.prepared != nil {
			t.Fatal("invalid JSON reached the socket")
		}
	}
	if value.calls.Load() != 1 {
		t.Fatal("failed encoding was retried for every client")
	}
}

func TestSharedBroadcastReachesMultipleWebSocketClients(t *testing.T) {
	server := startServer(t, "")
	defer server.Shutdown(time.Second)
	clients := make([]*websocket.Conn, 2)
	for i := range clients {
		conn, _, err := websocket.DefaultDialer.Dial("ws://"+server.Addr()+"/ws", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		clients[i] = conn
		if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatal(err)
		}
		// Reading the initial snapshot also confirms hub registration has finished.
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Fatal(err)
		}
	}
	value := &countedJSON{}
	server.hub.broadcast(struct {
		Type string       `json:"type"`
		Data *countedJSON `json:"data"`
	}{Type: "shared-test", Data: value})
	for _, conn := range clients {
		for {
			var message struct {
				Type string `json:"type"`
				Data struct {
					Sequence int `json:"sequence"`
				} `json:"data"`
			}
			if err := conn.ReadJSON(&message); err != nil {
				t.Fatal(err)
			}
			if message.Type == "shared-test" {
				if message.Data.Sequence != 42 {
					t.Fatalf("received sequence = %d, want 42", message.Data.Sequence)
				}
				break
			}
		}
	}
	if value.calls.Load() != 1 {
		t.Fatalf("real WebSocket broadcast encoded %d times", value.calls.Load())
	}
}

func TestReadOnlyCacheViewSurvivesRefresh(t *testing.T) {
	source := &staticPositionSnapshotSource{snapshot: position.PositionSnapshot{
		Slots: []position.SlotSnapshot{{Price: 100}},
	}}
	cache := newPositionCache(source, time.Second)
	cache.refresh()
	old, _, _ := cache.viewReadOnly()
	source.snapshot.Slots[0].Price = 200
	cache.refresh()
	current, _, _ := cache.viewReadOnly()
	if old.Slots[0].Price != 100 || current.Slots[0].Price != 200 {
		t.Fatal("refresh mutated an older view still being serialized")
	}
	copy, _, _ := cache.View()
	copy.Slots[0].Price = 300
	if current.Slots[0].Price != 200 {
		t.Fatal("public View allowed mutation of the shared cache")
	}
}

var benchmarkCachedPosition position.PositionSnapshot

func BenchmarkCachedPositionRead(b *testing.B) {
	source := &staticPositionSnapshotSource{snapshot: position.PositionSnapshot{
		Slots: make([]position.SlotSnapshot, 5000),
	}}
	cache := newPositionCache(source, time.Second)
	cache.refresh()
	b.Run("copy", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			benchmarkCachedPosition, _, _ = cache.View()
		}
	})
	b.Run("readOnly", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			benchmarkCachedPosition, _, _ = cache.viewReadOnly()
		}
	})
}
