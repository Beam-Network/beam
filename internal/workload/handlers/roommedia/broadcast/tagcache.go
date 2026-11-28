package broadcast

import (
	"bytes"
	"container/list"
	"hash/maphash"
	"sync"

	mc "github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
)

const (
	tagPlaceholder = "0000000000000000"
	tagOther       = "1111111111111111"
)

const tagCacheBytes = 32 << 20

type tagCache struct {
	seed maphash.Seed

	mu    sync.Mutex
	byKey map[uint64]*list.Element
	order *list.List
	size  int
}

type taggedPlaylist struct {
	key     uint64
	body    []byte
	tagged  []byte
	offsets []int
}

func newTagCache() *tagCache {
	return &tagCache{seed: maphash.MakeSeed(), byKey: map[uint64]*list.Element{}, order: list.New()}
}

func (c *tagCache) tag(body []byte, tag string) []byte {
	if len(tag) != len(tagPlaceholder) {
		return body
	}
	form := c.form(body)
	tagged := make([]byte, len(form.tagged))
	copy(tagged, form.tagged)
	for _, offset := range form.offsets {
		copy(tagged[offset:], tag)
	}
	return tagged
}

func (c *tagCache) form(body []byte) *taggedPlaylist {
	key := maphash.Bytes(c.seed, body)
	c.mu.Lock()
	if element, ok := c.byKey[key]; ok {
		if form := element.Value.(*taggedPlaylist); bytes.Equal(form.body, body) {
			c.order.MoveToFront(element)
			c.mu.Unlock()
			return form
		}
	}
	c.mu.Unlock()
	form := &taggedPlaylist{key: key, body: body, tagged: []byte(mc.TagMediaPlaylist(string(body), tagPlaceholder))}
	other := mc.TagMediaPlaylist(string(body), tagOther)
	for at := 0; at < len(form.tagged); at++ {
		if form.tagged[at] != other[at] {
			form.offsets = append(form.offsets, at)
			at += len(tagPlaceholder) - 1
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.byKey[key]; ok {
		c.remove(element)
	}
	c.byKey[key] = c.order.PushFront(form)
	c.size += len(form.body) + len(form.tagged) + 8*len(form.offsets)
	for c.size > tagCacheBytes && c.order.Len() > 1 {
		c.remove(c.order.Back())
	}
	return form
}

func (c *tagCache) remove(element *list.Element) {
	form := element.Value.(*taggedPlaylist)
	c.order.Remove(element)
	delete(c.byKey, form.key)
	c.size -= len(form.body) + len(form.tagged) + 8*len(form.offsets)
}
