package pipe

import (
	"errors"
	"sync"
	"time"

	"github.com/segmentio/ksuid"
)

type queue struct {
	stream
	writers  map[ksuid.KSUID]PipeWriter // 每一个连接
	ReadData func()                     // 读取数据函数
}

func NewQueue() *queue {
	q := &queue{
		writers: make(map[ksuid.KSUID]PipeWriter),
	}
	q.buffer = makeBuffer()
	q.status = StaWait
	q.done = make(chan struct{})
	return q
}

// 从缓冲区读取并发送到各个连接
func (q *queue) Send(hub *QueueHub) error {
	// 如果已经在发送，返回
	if q.status == StaSend || q.status == StaDone {
		return nil
	}
	defer func() {
		q.status = StaDone
		q.done <- struct{}{}
		close(q.done)
	}()
	pipePrintln(time.Now(), " queue start")
	// 设置为开始发送
	q.status = StaSend

	ret := make(chan error)
	for id, w := range q.writers {
		s := hub.Get(id)
		if s != nil {
			//只要有一个返回的，整个函数就返回，剩下的chan会被close然后退出
			go func(w PipeWriter, s *queue, id ksuid.KSUID) {
				for {
					// 如果状态已经关闭，则返回
					if q.status == StaClose {
						ret <- nil
						return
					}

					pipePrintln("read start", id)
					b, err := readWithTimeout(s.buffer, expFiveMinute)
					if err != nil {
						pipePrintln("queue read ", err.Error(), " ", id)
						ret <- err
						return
					}
					if b.eof {
						w.WriteEOF()
						ret <- nil
						return
					}
					pipePrintln("queue.send to:", id, "data:", string(b.data))
					_, e := w.Write(b.data)
					if e != nil {
						pipePrintln("queue.send write", e.Error())
						ret <- e
						return
					}
				}
			}(w, s, id)
		} else {
			pipePrintln(id, "queue.send queue not found")
			return errors.New("queue not found")
		}
	}
	return <-ret
}

type QueueHub struct {
	queue map[ksuid.KSUID]*queue
	mu    *sync.RWMutex
}

func NewQueueHub() *QueueHub {
	qh := &QueueHub{
		queue: make(map[ksuid.KSUID]*queue),
		mu:    &sync.RWMutex{},
	}
	return qh
}

// 把连接都加进来，不用保证顺序
func (h *QueueHub) Add(masterID ksuid.KSUID, id ksuid.KSUID, w PipeWriter) {
	h.mu.Lock()
	defer h.mu.Unlock()

	// 不存在，就先创建
	if _, ok := h.queue[masterID]; !ok {
		h.queue[masterID] = NewQueue()
	}
	h.queue[masterID].writers[id] = w
	if masterID != id { //不能给重置了。
		h.queue[id] = NewQueue()
	}
	h.trySend(masterID)
}

// 删除连接
func (h *QueueHub) RemoveAll(masterID ksuid.KSUID) {
	h.mu.Lock()
	defer h.mu.Unlock()

	// 存在，就删除
	if q, ok := h.queue[masterID]; ok {
		for _, id := range q.sorted {
			if id == masterID {
				continue
			}
			if c, ok := h.queue[id]; ok {
				c.close()
				delete(h.queue, id)
			}
		}
		q.close()
		delete(h.queue, masterID)
	}
}

// 获取数据
func (h *QueueHub) Get(masterID ksuid.KSUID) *queue {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if q, ok := h.queue[masterID]; ok {
		return q
	}
	return nil
}

// 设置全部连接
func (h *QueueHub) SetSort(masterID ksuid.KSUID, sort []ksuid.KSUID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if q, ok := h.queue[masterID]; ok {
		q.SetSort(sort)
	} else {
		h.queue[masterID] = NewQueue()
		h.queue[masterID].SetSort(sort)
	}
}

// 根据状态决定是否可开启发送
func (h *QueueHub) trySend(masterID ksuid.KSUID) bool {
	if q, ok := h.queue[masterID]; ok {
		if len(q.writers) == len(q.sorted) {
			pipePrintln("queue try", q.sorted)
			go q.ReadData()
			go q.Send(h)
			return true
		}
	}
	return false
}

// 根据状态决定是否可开启发送
func (h *QueueHub) TrySend(masterID ksuid.KSUID) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()

	return h.trySend(masterID)
}

// 获取数据
func (h *QueueHub) Len() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.queue)
}
