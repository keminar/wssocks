package main

import (
	"fmt"
	"net"
	"time"
)

func main() {
	conn, err := net.DialTimeout("tcp", "www.baidu.com:80", time.Second*8) // todo config timeout
	if err != nil {
		return
	}
	// 多次close会报错, 不会导致panic
	/*
		err = conn.Close()
		fmt.Println(err)
		err = conn.Close()
		fmt.Println(err)
		err = conn.Close()
		fmt.Println(err)
	*/

	// 函数内和外是同一个conn
	/*go test1(conn)
	conn.Close()
	time.Sleep(time.Second * 2)*/

	// 函数内和外还是同一个conn
	go test2(conn.(*net.TCPConn))
	conn.Close()
	time.Sleep(time.Second * 2)

	// 先close会发送不成功
	/*
		conn.Close()
		n1, err := conn.Write([]byte("GET "))
		fmt.Println(n1, err)
		n2, err := conn.Write([]byte("http://www.baidu.com "))
		fmt.Println(n2, err)
		n3, err := conn.Write([]byte("HTTP/1.0"))
		fmt.Println(n3, err)
	*/
}

func test1(conn net.Conn) {
	time.Sleep(time.Second)
	n1, err := conn.Write([]byte("GET "))
	fmt.Println(n1, err)
	n2, err := conn.Write([]byte("http://www.baidu.com "))
	fmt.Println(n2, err)
	n3, err := conn.Write([]byte("HTTP/1.0"))
	fmt.Println(n3, err)
}

func test2(conn *net.TCPConn) {
	n1, err := conn.Write([]byte("GET "))
	fmt.Println(n1, err)
	n2, err := conn.Write([]byte("http://www.baidu.com "))
	fmt.Println(n2, err)
	n3, err := conn.Write([]byte("HTTP/1.0"))
	fmt.Println(n3, err)
}
