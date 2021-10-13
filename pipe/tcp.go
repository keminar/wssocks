package pipe

//给本地网络交互数据时使用

import (
	"net"
)

type tcp struct {
	conn *net.TCPConn
}

func NewTcp(conn *net.TCPConn) *tcp {
	return &tcp{conn: conn}
}

// 写入
func (q *tcp) Write(data []byte) (n int, err error) {
	return q.conn.Write(data)
}

// 发送EOF
func (q *tcp) WriteEOF() {
	q.conn.CloseWrite()
}
