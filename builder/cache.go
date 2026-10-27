package builder

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// Cache maps a step's cache key — the parent image, the image config the
// step produces, the step itself and the digest of its inputs — to the
// image a previous build committed for it. A hit is used only after the
// engine confirms the image still exists.
type Cache interface {
	Get(key string) (string, bool)
	Put(key, imageID string)
}

// MemoryCache is a bounded in-memory Cache (the oldest entries go first).
type MemoryCache struct {
	mu    sync.Mutex
	m     map[string]string
	order []string
	Max   int
}

// NewMemoryCache holds at most max entries (4096 when max <= 0).
func NewMemoryCache(max int) *MemoryCache {
	if max <= 0 {
		max = 4096
	}
	return &MemoryCache{m: map[string]string{}, Max: max}
}

// Get implements Cache.
func (c *MemoryCache) Get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[key]
	return v, ok
}

// Put implements Cache.
func (c *MemoryCache) Put(key, id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[key]; !ok {
		c.order = append(c.order, key)
	}
	c.m[key] = id
	for len(c.order) > c.Max {
		delete(c.m, c.order[0])
		c.order = c.order[1:]
	}
}

type cacheEntry struct {
	Key string `json:"key"`
	ID  string `json:"id"`
}

// FileCache is a MemoryCache kept in a JSON file between builds.
type FileCache struct {
	*MemoryCache
	path string
}

// OpenFileCache loads path (a missing file is an empty cache; a damaged
// one is discarded: the cache only ever saves work).
func OpenFileCache(path string, max int) *FileCache {
	fc := &FileCache{MemoryCache: NewMemoryCache(max), path: path}
	b, err := os.ReadFile(path)
	if err != nil || len(b) > 64<<20 {
		return fc
	}
	var es []cacheEntry
	if json.Unmarshal(b, &es) != nil {
		return fc
	}
	for _, e := range es {
		if len(e.Key) == 64 && len(e.ID) <= 128 {
			fc.Put(e.Key, e.ID)
		}
	}
	return fc
}

// Save writes the cache (0600, through a temporary file and a rename).
func (fc *FileCache) Save() error {
	fc.mu.Lock()
	es := make([]cacheEntry, 0, len(fc.order))
	for _, k := range fc.order {
		es = append(es, cacheEntry{k, fc.m[k]})
	}
	fc.mu.Unlock()
	b, err := json.Marshal(es)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(fc.path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(fc.path), ".build-cache-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), fc.path)
}

// DefaultCachePath is where wctr build keeps its cache natively.
func DefaultCachePath() (string, error) {
	d, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	if d == "" {
		return "", fs.ErrNotExist
	}
	return filepath.Join(d, "wasmcontainers", "build-cache.json"), nil
}
