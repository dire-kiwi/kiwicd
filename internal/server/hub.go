package server

import "sync"

// subBuffer is how many events a subscriber may fall behind before it is
// dropped.
const subBuffer = 256

// hub fans out events to SSE subscribers by topic ("runs", "job:<id>").
type hub struct {
	mu   sync.Mutex
	subs map[string]map[chan event]struct{}
}

type event struct {
	Type string
	Data any
}

func newHub() *hub {
	return &hub{subs: map[string]map[chan event]struct{}{}}
}

// subscribe returns a channel of events for topic and a function that ends
// the subscription. If the subscriber falls too far behind, the hub drops it
// and closes the channel; the subscriber must then resync from the database.
func (h *hub) subscribe(topic string) (chan event, func()) {
	ch := make(chan event, subBuffer)
	h.mu.Lock()
	if h.subs[topic] == nil {
		h.subs[topic] = map[chan event]struct{}{}
	}
	h.subs[topic][ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		h.remove(topic, ch)
		h.mu.Unlock()
	}
}

func (h *hub) remove(topic string, ch chan event) {
	delete(h.subs[topic], ch)
	if len(h.subs[topic]) == 0 {
		delete(h.subs, topic)
	}
}

func (h *hub) publish(topic, typ string, data any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[topic] {
		select {
		case ch <- event{Type: typ, Data: data}:
		default:
			// Slow subscriber: rather than silently skip this event, drop
			// the subscriber so it knows to resync.
			h.remove(topic, ch)
			close(ch)
		}
	}
}
