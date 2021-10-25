package pipe

import (
	"errors"
	"sync"

	"github.com/segmentio/ksuid"
)

type queue struct {
	stream
	writers  map[ksuid.KSUID]PipeWriter // 每一个连接
	ReadData func()                     // 读取数据函数
}

func NewQueue(masterID ksuid.KSUID) *queue {
	q := &queue{
		writers: make(map[ksuid.KSUID]PipeWriter),
	}
	q.masterID = masterID
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
	pipePrintln(timeNow(), q.masterID, "queue.send start")
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
						pipePrintln(timeNow(), q.masterID, "queue.send status is closed", id)
						ret <- nil
						return
					}

					pipePrintln(timeNow(), q.masterID, "queue.send read from chan", id)
					b, err := readWithTimeout(s.buffer, expFiveMinute)
					if err != nil {
						//chan closed 或 timeout
						pipePrintln(timeNow(), q.masterID, "queue.send read", err.Error(), id)
						ret <- err
						return
					}

					pipePrintln(timeNow(), q.masterID, "queue.send write", id, "data:", len(b.data), "eof", b.eof)
					if b.eof {
						pipePrintln(timeNow(), q.masterID, "queue.send write", id, "write eof end")
						w.WriteEOF()
						ret <- nil
						return
					}
					_, e := w.Write(b.data)
					if e != nil {
						pipePrintln(timeNow(), q.masterID, "queue.send write", id, e.Error())
						ret <- e
						return
					}
				}
			}(w, s, id)
		} else {
			pipePrintln(timeNow(), q.masterID, "queue.send queue not found", id)
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
		h.queue[masterID] = NewQueue(masterID)
	}
	h.queue[masterID].writers[id] = w
	if masterID != id { //不能给重置了。
		h.queue[id] = NewQueue(masterID)
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
		h.queue[masterID] = NewQueue(masterID)
		h.queue[masterID].SetSort(sort)
	}
}

// 根据状态决定是否可开启发送
func (h *QueueHub) trySend(masterID ksuid.KSUID) bool {
	if q, ok := h.queue[masterID]; ok {
		if len(q.writers) == len(q.sorted) {
			pipePrintln(timeNow(), q.masterID, "queue.hub try", q.sorted)
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
