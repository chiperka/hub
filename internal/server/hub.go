package server

import (
	"sync"
)

// Event is a server-sent event published on a topic.
type Event struct {
	Topic string
	Name  string
	Data  any
}

const topicGlobal = "global"

func runTopic(id string) string { return "run:" + id }

// Hub fans events out to SSE subscribers.
type Hub struct {
	mu   sync.RWMutex
	subs map[*subscription]struct{}
}

type subscription struct {
	topics map[string]bool
	ch     chan Event
}

func NewHub() *Hub { return &Hub{subs: map[*subscription]struct{}{}} }

func (h *Hub) Subscribe(topics ...string) *subscription {
	sub := &subscription{topics: map[string]bool{}, ch: make(chan Event, 512)}
	for _, t := range topics {
		sub.topics[t] = true
	}
	h.mu.Lock()
	h.subs[sub] = struct{}{}
	h.mu.Unlock()
	return sub
}

func (h *Hub) Unsubscribe(sub *subscription) {
	h.mu.Lock()
	delete(h.subs, sub)
	h.mu.Unlock()
}

// Publish delivers the event to every subscriber of its topic. Slow subscribers
// drop events rather than blocking publishers.
func (h *Hub) Publish(topic, name string, data any) {
	ev := Event{Topic: topic, Name: name, Data: data}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for sub := range h.subs {
		if !sub.topics[topic] {
			continue
		}
		select {
		case sub.ch <- ev:
		default:
		}
	}
}

// Notifier wakes long-polling claimers when new work may be available.
type Notifier struct {
	mu sync.Mutex
	ch chan struct{}
}

func NewNotifier() *Notifier { return &Notifier{ch: make(chan struct{})} }

// Wait returns a channel closed on the next Notify. Grab it *before* checking
// for work so a notification between the check and the wait is never missed.
func (n *Notifier) Wait() <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.ch
}

func (n *Notifier) Notify() {
	n.mu.Lock()
	close(n.ch)
	n.ch = make(chan struct{})
	n.mu.Unlock()
}
