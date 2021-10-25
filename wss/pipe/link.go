package pipe

import (
	"errors"
	"io"
	"net"
	"sync"

	"github.com/segmentio/ksuid"
)

type link struct {
	stream
	conn    *net.TCPConn // 当前为主连接时存储目标连接
	counter int          // 当前为主连接时存储已经收到的连接计数，用于决定是否可以向外发送数据
}

func NewLink(masterID ksuid.KSUID) *link {
	l := &link{
		counter: 0,
	}
	l.MasterID = masterID
	l.buffer = makeBuffer()
	l.status = StaWait
	l.done = make(chan struct{})
	return l
}

// 设置对外连接，仅当前为主连接调用
func (l *link) setConn(conn *net.TCPConn) {
	l.conn = conn
}

// 发送数据
func (l *link) Send(hub *LinkHub) error {
	// 如果已经在发送，返回
	if l.status == StaSend || l.status == StaDone {
		return nil
	}
	defer func() {
		l.status = StaDone
		l.done <- struct{}{}
		close(l.done)
	}()
	// 设置为开始发送
	l.status = StaSend

	for {
		// 用于循环中的退出
		if l.status == StaClose {
			return io.ErrClosedPipe
		}
		for _, id := range l.sorted {
			s := hub.Get(id)
			if s != nil {
				pipePrintln(timeNow(), l.MasterID, "link.send read from chan", id)
				b, err := readWithTimeout(s.buffer, bufReadTimeout)
				if err != nil {
					pipePrintln(timeNow(), l.MasterID, "link.send read timeout ", id)
					return err
				}
				pipePrintln(timeNow(), l.MasterID, "link.send write", id, "data:", len(b.data), "eof", b.eof)
				_, err = safeWrite(l.conn, b.data, b.eof)
				if err != nil {
					pipePrintln(timeNow(), l.MasterID, "link.send write", id, err.Error())
					return err
				}
				// 已经发送了关闭写，就不要再卡在循环里了
				if b.eof {
					pipePrintln(timeNow(), l.MasterID, "link.send eof end", id)
					return nil
				}
			} else {
				pipePrintln(timeNow(), l.MasterID, "link.send queue not found", id)
				return errors.New("queue not found")
			}
		}
	}
}

type LinkHub struct {
	links map[ksuid.KSUID]*link
	mu    *sync.RWMutex
}

func NewLinkHub() *LinkHub {
	qh := &LinkHub{
		links: make(map[ksuid.KSUID]*link),
		mu:    &sync.RWMutex{},
	}
	return qh
}

// 增加连接
func (h *LinkHub) Add(id ksuid.KSUID, masterID ksuid.KSUID) {
	h.mu.Lock()
	defer h.mu.Unlock()

	// 先初始化主连接做计数器用
	m, ok := h.links[masterID]
	if !ok {
		h.links[masterID] = NewLink(masterID)
		m = h.links[masterID]
	}

	// 所有连接
	if _, ok := h.links[id]; !ok {
		h.links[id] = NewLink(masterID)
	}
	m.counter++
	h.trySend(masterID, nil)
}

// 删除连接
func (h *LinkHub) RemoveAll(masterID ksuid.KSUID) {
	h.mu.Lock()
	defer h.mu.Unlock()

	// 存在，就删除
	if q, ok := h.links[masterID]; ok {
		for _, id := range q.sorted {
			if id == masterID {
				continue
			}
			if c, ok := h.links[id]; ok {
				c.close()
				delete(h.links, id)
			}
		}
		q.close()
		delete(h.links, masterID)
	}
}

// 取数据
func (h *LinkHub) Get(id ksuid.KSUID) *link {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if q, ok := h.links[id]; ok {
		return q
	}
	return nil
}

// 设置连接传输顺序
func (h *LinkHub) SetSort(masterID ksuid.KSUID, sort []ksuid.KSUID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if q, ok := h.links[masterID]; ok {
		q.SetSort(sort)
	} else {
		h.links[masterID] = NewLink(masterID)
		h.links[masterID].SetSort(sort)
	}
}

// 服务端根据状态决定发送
func (h *LinkHub) trySend(masterID ksuid.KSUID, conn *net.TCPConn) bool {
	if q, ok := h.links[masterID]; ok {
		if conn != nil {
			q.setConn(conn)
		}
		//fmt.Println("try", q.conn, q.counter, len(q.sorted))
		if q.conn != nil && q.counter == len(q.sorted) {
			pipePrintln(timeNow(), q.MasterID, "link.hub try", q.sorted, q.conn)
			go q.Send(h)
			return true
		}
	}
	return false
}

// 服务端根据状态决定发送
func (h *LinkHub) TrySend(masterID ksuid.KSUID, conn *net.TCPConn) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()

	return h.trySend(masterID, conn)
}

// 获取数据
func (h *LinkHub) Len() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.links)
}
