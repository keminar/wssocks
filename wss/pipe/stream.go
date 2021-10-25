package pipe

import (
	"errors"
	"time"

	"github.com/segmentio/ksuid"
)

type stream struct {
	masterID ksuid.KSUID
	buffer   chan buffer // 连接数据缓冲区
	status   string      // 当前状态
	done     chan struct{}
	sorted   []ksuid.KSUID // 连接发送的顺序
}

// 设置排序，主连接调用
func (s *stream) SetSort(sort []ksuid.KSUID) {
	s.sorted = sort
}

// 写入缓冲区
func (s *stream) Write(data []byte) (n int, err error) {
	tmp := make([]byte, len(data))
	copy(tmp, data)
	return s.writeBuf(&buffer{eof: false, data: tmp})
}

// 发送EOF
func (s *stream) WriteEOF() {
	s.writeBuf(&buffer{eof: true, data: []byte{}})
}

// 基础方法
func (s *stream) writeBuf(b *buffer) (n int, err error) {
	if s.status == StaDone {
		return 0, errors.New("send is over")
	}
	defer func() {
		// 捕获异常
		if err := recover(); err != nil {
			// 如果走到这边，函数返回值是0, nil
			pipePrintln(timeNow(), s.masterID, "stream.writer recover", err)
			return
		}
	}()
	select {
	case <-time.After(bufWriteTimeout):
		return 0, errors.New("write timeout")
	case s.buffer <- *b:
	}
	return len(b.data), nil
}

// 堵塞等待
func (s *stream) Wait() {
	<-s.done
}

// 释放资源
func (s *stream) close() {
	if s.status == StaClose {
		return
	}
	s.status = StaClose
	if s.buffer != nil {
		close(s.buffer)
	}
}