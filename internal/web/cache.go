package web

import (
	"container/list"
	"sync"

	"github.com/mihai-max/ao3/internal/epub"
)

// bookCache keeps recently opened EPUBs in memory so paging through
// chapters does not re-read the zip each time.
type bookCache struct {
	mu       sync.Mutex
	maxBytes int64
	bytes    int64
	ll       *list.List
	items    map[int64]*list.Element
}

type cacheEntry struct {
	id   int64
	book *epub.Book
}

func newBookCache(maxBytes int64) *bookCache {
	return &bookCache{maxBytes: maxBytes, ll: list.New(), items: map[int64]*list.Element{}}
}

func (c *bookCache) get(id int64) *epub.Book {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[id]; ok {
		c.ll.MoveToFront(el)
		return el.Value.(*cacheEntry).book
	}
	return nil
}

func (c *bookCache) put(id int64, b *epub.Book) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[id]; ok {
		c.ll.MoveToFront(el)
		return
	}
	c.items[id] = c.ll.PushFront(&cacheEntry{id: id, book: b})
	c.bytes += int64(len(b.Data))
	for c.bytes > c.maxBytes && c.ll.Len() > 1 {
		el := c.ll.Back()
		e := el.Value.(*cacheEntry)
		c.ll.Remove(el)
		delete(c.items, e.id)
		c.bytes -= int64(len(e.book.Data))
	}
}
