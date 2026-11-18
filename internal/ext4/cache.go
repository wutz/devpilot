/*
Copyright 2026 wutz.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package ext4

import (
	"container/list"
	"sync"
)

// blockCache is a small LRU cache of metadata blocks (group descriptors,
// inode tables, extent tree nodes, indirect blocks, xattr blocks). Images
// commonly live on network filesystems, where re-reading the same metadata
// block for every inode would be slow.
type blockCache struct {
	mu  sync.Mutex
	max int
	ll  *list.List
	m   map[uint64]*list.Element
}

type cacheEntry struct {
	blk  uint64
	data []byte
}

func newBlockCache(maxBlocks int) *blockCache {
	return &blockCache{max: maxBlocks, ll: list.New(), m: make(map[uint64]*list.Element)}
}

func (c *blockCache) get(blk uint64) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[blk]; ok {
		c.ll.MoveToFront(e)
		return e.Value.(*cacheEntry).data
	}
	return nil
}

func (c *blockCache) put(blk uint64, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[blk]; ok {
		c.ll.MoveToFront(e)
		e.Value.(*cacheEntry).data = data
		return
	}
	c.m[blk] = c.ll.PushFront(&cacheEntry{blk: blk, data: data})
	for c.ll.Len() > c.max {
		old := c.ll.Back()
		c.ll.Remove(old)
		delete(c.m, old.Value.(*cacheEntry).blk)
	}
}

func (c *blockCache) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ll.Init()
	c.m = make(map[uint64]*list.Element)
}
