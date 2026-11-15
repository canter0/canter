package wrencomputer

import (
	"crypto/sha256"
	"fmt"
	"sync"
	"time"
)

// ClientPool shares pinned transports, not authorization. Callers must perform
// the normal account, conversation, lifecycle and control checks on every call.
// Host identity and the credential both participate in the cache identity.
type ClientPool struct {
	mu      sync.Mutex
	entries map[string]poolEntry
}
type poolEntry struct {
	client *Client
	used   time.Time
}

func (p *ClientPool) Get(config Config) (*Client, error) {
	key := fmt.Sprintf("%s:%d:%s:%s:%x", config.Address, config.Port, config.Username, config.HostKeySHA256, sha256.Sum256(config.PrivateKeyPEM))
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if p.entries == nil {
		p.entries = make(map[string]poolEntry)
	}
	if entry, ok := p.entries[key]; ok {
		entry.used = now
		p.entries[key] = entry
		return entry.client, nil
	}
	for key, entry := range p.entries {
		if now.Sub(entry.used) > 2*time.Minute {
			entry.client.Close()
			delete(p.entries, key)
		}
	}
	client, err := NewSSH(config)
	if err != nil {
		return nil, err
	}
	// The client closes its own idle transport even if the cache is full.
	if len(p.entries) < 64 {
		p.entries[key] = poolEntry{client, now}
	}
	return client, nil
}
