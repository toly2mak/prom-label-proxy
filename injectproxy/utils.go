// Copyright 2020 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package injectproxy

import (
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

func prometheusAPIError(w http.ResponseWriter, errorMessage string, code int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)

	res := map[string]string{"status": "error", "errorType": "prom-label-proxy", "error": errorMessage}

	if err := json.NewEncoder(w).Encode(res); err != nil {
		log.Printf("error: Failed to encode json: %v", err)
	}
}

//
// Cache with TTL
//

type CacheEntry struct {
	Value      []byte
	Expiration int64
}

// Cache represents a write-through cache with TTL cleanup.
type Cache struct {
	mu    sync.RWMutex
	store map[string]CacheEntry
	ttl   time.Duration
}

func NewCache(ttl time.Duration) *Cache {
	c := &Cache{
		store: make(map[string]CacheEntry),
		ttl:   ttl,
	}
	go c.cleanup() // Start the cleanup goroutine
	return c
}

// Set adds a new entry to the cache with a write-through mechanism.
func (c *Cache) Set(key string, value []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.store[key] = CacheEntry{
		Value:      value,
		Expiration: time.Now().Add(c.ttl).UnixNano(),
	}
}

// Get retrieves an entry from the cache.
func (c *Cache) Get(key string) ([]byte, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, found := c.store[key]
	if !found || time.Now().UnixNano() > entry.Expiration {
		return nil, false
	}
	return entry.Value, true
}

// cleanup removes expired entries from the cache.
func (c *Cache) cleanup() {
	for {
		time.Sleep(c.ttl) // Sleep for the duration of TTL
		c.mu.Lock()
		for key, entry := range c.store {
			if time.Now().UnixNano() > entry.Expiration {
				delete(c.store, key)
			}
		}
		c.mu.Unlock()
	}
}

func prometheusCacheAPIResponse(w http.ResponseWriter, cacheKey string, cache *Cache) error {

	slog.Debug("prometheusCacheAPIResponse", "cacheKey", cacheKey)

	value, found := cache.Get(cacheKey)

	slog.Debug("prometheusCacheAPIResponse", "found", found)

	if found {
		w.Header().Set("X-Content-Type-Options", "cached")
		w.WriteHeader(200)

		if _, err := w.Write(value); err != nil {
			log.Printf("error: Failed to write data from cache: %v", err)
		}
		return nil

	}
	// if miss cache return error
	return fmt.Errorf("can't get from cache")

}
