package main

import (
	"fmt"
	"io"
	"net"
	"time"
)

type dead struct {
	Line time.Duration
}

func NewDead() *dead {
	return &dead{Line: time.Minute}
}

func main() {
	conn, err := net.DialTimeout("tcp", "www.baidu.com:80", time.Second*8) // todo config timeout
	if err != nil {
		return
	}

	fmt.Println("connected")
	n1, err := conn.Write([]byte("GET http://www.baidu.com HTTP/1.0\r\n"))
	fmt.Println(n1, err)
	n2, err := conn.Write([]byte("Host: www.baidu.com\r\n"))
	fmt.Println(n2, err)
	n3, err := conn.Write([]byte("Connection:close\r\n\r\n"))
	fmt.Println(n3, err)

	conn.(*net.TCPConn).CloseWrite()

	d := NewDead()
	go func() {
		time.Sleep(3 * time.Second)
		//让本次read快失败
		conn.(*net.TCPConn).SetReadDeadline(time.Now().Add(time.Second))
		//让下次read快失败
		d.Line = time.Second
	}()
	read(conn.(*net.TCPConn), d)
}

func read(conn *net.TCPConn, d *dead) {

	s1 := time.Now()
	buf := make([]byte, 100)
	for {
		fmt.Println("line", d.Line)
		conn.SetReadDeadline(time.Now().Add(d.Line))
		n, e := conn.Read(buf)
		if e == io.EOF {
			fmt.Println("eof")
			return
		}
		fmt.Println(time.Since(s1), string(buf[0:n]), e)
		time.Sleep(time.Second)
	}
}
