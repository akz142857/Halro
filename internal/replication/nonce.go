package replication

import "sync"

type nonceHistory struct {
	order [][NonceBytes]byte
	seen  map[[NonceBytes]byte]struct{}
}

// NonceCache is a bounded, process-lifetime replay cache. A restart still gets
// a fresh local nonce and TLS exporter; the cache prevents a captured hello
// from being accepted twice within one member process.
type NonceCache struct {
	mu      sync.Mutex
	perPeer int
	peers   map[string]*nonceHistory
}

func NewNonceCache(perPeer int) *NonceCache {
	if perPeer < 1 {
		perPeer = 1
	}
	return &NonceCache{perPeer: perPeer, peers: make(map[string]*nonceHistory)}
}

// Seen records nonce and reports whether it was already present.
func (c *NonceCache) Seen(nodeID string, nonce [NonceBytes]byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	history := c.peers[nodeID]
	if history == nil {
		history = &nonceHistory{seen: make(map[[NonceBytes]byte]struct{})}
		c.peers[nodeID] = history
	}
	if _, ok := history.seen[nonce]; ok {
		return true
	}
	history.order = append(history.order, nonce)
	history.seen[nonce] = struct{}{}
	if len(history.order) > c.perPeer {
		oldest := history.order[0]
		history.order = history.order[1:]
		delete(history.seen, oldest)
	}
	return false
}
