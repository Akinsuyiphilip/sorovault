package api

import (
	"container/list"
	"sync"
	"time"
)

const interfaceCacheBytes = 16 << 20

type specCacheKey struct {
	network    string
	contractID string
	wasmHash   string
}

type cachedInterface struct {
	body      []byte
	decodedAt time.Time
}

type interfaceCacheEntry struct {
	key   specCacheKey
	value cachedInterface
	size  int
}

type interfaceCache struct {
	mu       sync.Mutex
	capacity int
	size     int
	entries  map[specCacheKey]*list.Element
	lru      list.List
}

func newInterfaceCache(capacity int) *interfaceCache {
	return &interfaceCache{
		capacity: capacity,
		entries:  make(map[specCacheKey]*list.Element),
	}
}

func (c *interfaceCache) get(key specCacheKey) (cachedInterface, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	element, ok := c.entries[key]
	if !ok {
		return cachedInterface{}, false
	}
	c.lru.MoveToFront(element)
	return element.Value.(*interfaceCacheEntry).value, true
}

func (c *interfaceCache) add(key specCacheKey, value cachedInterface) {
	size := len(key.network) + len(key.contractID) + len(key.wasmHash) + len(value.body)
	if c.capacity <= 0 || size > c.capacity {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if old, ok := c.entries[key]; ok {
		c.remove(old)
	}
	for c.size+size > c.capacity {
		c.remove(c.lru.Back())
	}

	entry := &interfaceCacheEntry{key: key, value: value, size: size}
	c.entries[key] = c.lru.PushFront(entry)
	c.size += size
}

func (c *interfaceCache) remove(element *list.Element) {
	entry := element.Value.(*interfaceCacheEntry)
	delete(c.entries, entry.key)
	c.size -= entry.size
	c.lru.Remove(element)
}
