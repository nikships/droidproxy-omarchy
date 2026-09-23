// Package events is the NotificationCenter replacement: a tiny in-process
// publish/subscribe bus keyed by topic name.
package events

import "sync"

// Topics mirror the macOS Notification.Name constants plus Linux-only state.
const (
	ServerStatusChanged  = "ServerStatusChanged"
	AuthDirectoryChanged = "AuthDirectoryChanged"
	MetaAccountsChanged  = "MetaAccountsChanged"
	PrefsChanged         = "PrefsChanged"
	CopilotChanged       = "CopilotChanged"
	UsageChanged         = "UsageChanged"
	UpdateChanged        = "UpdateChanged"
	AuthFlowChanged      = "AuthFlowChanged"
)

type subscriber struct {
	id int
	fn func()
}

var (
	mu     sync.Mutex
	nextID int
	subs   = map[string][]subscriber{}
)

// Subscribe registers fn for topic and returns a function that removes it.
// fn runs on the publisher's goroutine, so it must not block.
func Subscribe(topic string, fn func()) (unsubscribe func()) {
	mu.Lock()
	defer mu.Unlock()
	nextID++
	id := nextID
	subs[topic] = append(subs[topic], subscriber{id: id, fn: fn})
	return func() {
		mu.Lock()
		defer mu.Unlock()
		list := subs[topic]
		for i, s := range list {
			if s.id == id {
				subs[topic] = append(list[:i:i], list[i+1:]...)
				return
			}
		}
	}
}

// Publish calls every subscriber of topic.
func Publish(topic string) {
	mu.Lock()
	list := append([]subscriber(nil), subs[topic]...)
	mu.Unlock()
	for _, s := range list {
		s.fn()
	}
}

// Reset drops every subscriber. Tests only.
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	subs = map[string][]subscriber{}
}
