package cache

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// entryOverhead approximates the per-entry bookkeeping (map slot, list element,
// struct, string headers) counted against MaxBytes in addition to key+value.
const entryOverhead = 96

// Memory is a process-local LRU cache bounded by MaxBytes (key + value +
// overhead per entry). Expired entries are dropped when read; otherwise
// eviction is least-recently-used, so an expired entry occupies its bytes until
// it is read, overwritten or evicted.
// Values are copied in and out, so callers may reuse their slices.
type Memory struct {
	mu            sync.Mutex
	maxBytes      int64
	maxValueBytes int64
	used          int64
	ll            *list.List // front = most recently used
	items         map[string]*list.Element
	now           func() time.Time
	evictions     int64
}

type memEntry struct {
	key     string
	value   []byte
	expires time.Time
}

// NewMemory returns a cache holding at most maxBytes; a single value may not
// exceed maxValueBytes (<= 0 means maxBytes/8).
func NewMemory(maxBytes, maxValueBytes int64) *Memory {
	if maxValueBytes <= 0 || maxValueBytes > maxBytes {
		maxValueBytes = maxBytes / 8
	}
	return &Memory{maxBytes: maxBytes, maxValueBytes: maxValueBytes, ll: list.New(),
		items: map[string]*list.Element{}, now: time.Now}
}

func size(e *memEntry) int64 { return int64(len(e.key)+len(e.value)) + entryOverhead }

func (m *Memory) Get(_ context.Context, key string) ([]byte, bool, error) {
	if err := checkKey(key); err != nil {
		return nil, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	el, ok := m.items[key]
	if !ok {
		return nil, false, nil
	}
	e := el.Value.(*memEntry)
	if !m.now().Before(e.expires) {
		m.remove(el)
		return nil, false, nil
	}
	m.ll.MoveToFront(el)
	return append([]byte(nil), e.value...), true, nil
}

func (m *Memory) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	if err := checkKey(key); err != nil {
		return err
	}
	if ttl <= 0 {
		return ErrInvalidTTL
	}
	if int64(len(value)) > m.maxValueBytes {
		return ErrValueTooLarge
	}
	e := &memEntry{key: key, value: append([]byte(nil), value...), expires: m.now().Add(ttl)}
	m.mu.Lock()
	defer m.mu.Unlock()
	if el, ok := m.items[key]; ok {
		m.remove(el)
	}
	m.used += size(e)
	m.items[key] = m.ll.PushFront(e)
	for m.used > m.maxBytes {
		m.remove(m.ll.Back())
		m.evictions++
	}
	return nil
}

func (m *Memory) Delete(_ context.Context, key string) error {
	if err := checkKey(key); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if el, ok := m.items[key]; ok {
		m.remove(el)
	}
	return nil
}

func (m *Memory) remove(el *list.Element) {
	e := m.ll.Remove(el).(*memEntry)
	delete(m.items, e.key)
	m.used -= size(e)
}

// Stats reports accounted bytes, entry count and LRU evictions.
func (m *Memory) Stats() (usedBytes int64, entries int, evictions int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.used, len(m.items), m.evictions
}
