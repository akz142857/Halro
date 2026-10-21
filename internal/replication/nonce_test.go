package replication

import "testing"

func TestNonceCacheRejectsReplayAndStaysBoundedPerPeer(t *testing.T) {
	cache := NewNonceCache(2)
	var first, second, third [NonceBytes]byte
	first[0], second[0], third[0] = 1, 2, 3
	if cache.Seen("halro-1", first) || !cache.Seen("halro-1", first) {
		t.Fatal("nonce replay was not detected")
	}
	if cache.Seen("halro-1", second) || cache.Seen("halro-1", third) {
		t.Fatal("fresh nonce was reported as a replay")
	}
	if cache.Seen("halro-1", first) {
		t.Fatal("evicted nonce remained in the bounded cache")
	}
	if cache.Seen("halro-2", third) || !cache.Seen("halro-2", third) {
		t.Fatal("nonce histories were not independent per peer")
	}
}
