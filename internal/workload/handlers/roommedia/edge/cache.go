package edge

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Cache struct {
	mu        sync.Mutex
	memLimit  int64
	memSize   int64
	mem       map[string]*list.Element
	memOrder  *list.List
	dir       string
	diskLimit int64
	diskSize  int64
	disk      map[string]*list.Element
	diskOrder *list.List
}

type cacheEntry struct {
	key     string
	body    []byte
	size    int64
	expires time.Time
}

var sharedCache struct {
	sync.Mutex
	cache *Cache
}

func sharedCacheFor(dir string, diskBytes, memoryBytes int64) *Cache {
	sharedCache.Lock()
	defer sharedCache.Unlock()
	if sharedCache.cache != nil {
		return sharedCache.cache
	}
	sharedCache.cache = newCache(dir, diskBytes, memoryBytes)
	return sharedCache.cache
}

func newCache(dir string, diskBytes, memoryBytes int64) *Cache {
	if memoryBytes <= 0 {
		memoryBytes = 512 << 20
	}
	cache := &Cache{memLimit: memoryBytes, mem: map[string]*list.Element{}, memOrder: list.New(),
		disk: map[string]*list.Element{}, diskOrder: list.New()}
	if dir != "" {
		cache.dir = filepath.Join(dir, "edge")
		_ = os.RemoveAll(cache.dir)
		if err := os.MkdirAll(cache.dir, 0o700); err == nil {
			cache.diskLimit = diskBytes
			if cache.diskLimit <= 0 {
				cache.diskLimit = 10 << 30
			}
		} else {
			cache.dir = ""
		}
	}
	return cache
}

func (c *Cache) path(key string) string {
	digest := sha256.Sum256([]byte(key))
	name := hex.EncodeToString(digest[:])
	return filepath.Join(c.dir, name[:2], name)
}

func (c *Cache) Get(key string) ([]byte, bool) {
	c.mu.Lock()
	if element, ok := c.mem[key]; ok {
		entry := element.Value.(*cacheEntry)
		if entry.expires.IsZero() || time.Now().Before(entry.expires) {
			c.memOrder.MoveToFront(element)
			c.mu.Unlock()
			return entry.body, true
		}
		c.removeMem(element)
	}
	element, onDisk := c.disk[key]
	if onDisk {
		c.diskOrder.MoveToFront(element)
	}
	c.mu.Unlock()
	if !onDisk {
		return nil, false
	}
	body, err := os.ReadFile(c.path(key))
	if err != nil {
		return nil, false
	}
	c.putMem(key, body, time.Time{})
	return body, true
}

func (c *Cache) Put(key string, body []byte, ttl time.Duration) {
	expires := time.Time{}
	if ttl > 0 {
		expires = time.Now().Add(ttl)
	}
	c.putMem(key, body, expires)
	if ttl == 0 && c.dir != "" {
		c.putDisk(key, body)
	}
}

func (c *Cache) putMem(key string, body []byte, expires time.Time) {
	size := int64(len(body)) + int64(len(key))
	c.mu.Lock()
	defer c.mu.Unlock()
	if size > c.memLimit/4 {
		return
	}
	if element, ok := c.mem[key]; ok {
		c.removeMem(element)
	}
	c.mem[key] = c.memOrder.PushFront(&cacheEntry{key: key, body: body, size: size, expires: expires})
	c.memSize += size
	for c.memSize > c.memLimit {
		c.removeMem(c.memOrder.Back())
	}
}

func (c *Cache) removeMem(element *list.Element) {
	entry := element.Value.(*cacheEntry)
	c.memOrder.Remove(element)
	delete(c.mem, entry.key)
	c.memSize -= entry.size
}

func (c *Cache) putDisk(key string, body []byte) {
	size := int64(len(body))
	c.mu.Lock()
	if _, exists := c.disk[key]; exists || size > c.diskLimit/4 {
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	path := c.path(key)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.disk[key] = c.diskOrder.PushFront(&cacheEntry{key: key, size: size})
	c.diskSize += size
	for c.diskSize > c.diskLimit {
		oldest := c.diskOrder.Back()
		entry := oldest.Value.(*cacheEntry)
		c.diskOrder.Remove(oldest)
		delete(c.disk, entry.key)
		c.diskSize -= entry.size
		_ = os.Remove(c.path(entry.key))
	}
}

func (c *Cache) UsedBytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.diskSize + c.memSize
}

func CacheUsedBytes() int64 {
	sharedCache.Lock()
	cache := sharedCache.cache
	sharedCache.Unlock()
	if cache == nil {
		return 0
	}
	return cache.UsedBytes()
}
