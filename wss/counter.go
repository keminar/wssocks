package wss

import (
	"fmt"
	"sync"
)

type size int64

func (s *size) String() string {
	t := int64(*s)
	if t > 1024*1024 {
		return fmt.Sprintf("%dM", t/1024/1024)
	} else if t > 1024 {
		return fmt.Sprintf("%dK", t/1024)
	} else {
		return fmt.Sprintf("%d", t)
	}
}

type counter struct {
	num  int64 // 次数
	size size  // 字节数
	mu   *sync.RWMutex
}

func NewCounter() *counter {
	return &counter{num: 0, size: 0, mu: &sync.RWMutex{}}
}

func (c *counter) Add(s int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.num++
	c.size += size(s)
}

func (c *counter) Get() (int64, size) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.num, c.size
}
