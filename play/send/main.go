package main

import (
	"fmt"
	"log"
	"net"
	"time"

	"golang.org/x/net/proxy"
)

func main() {

	address := fmt.Sprintf("%s:%d", "127.0.0.1", 1080)
	var dialProxy proxy.Dialer
	dialProxy, err := proxy.SOCKS5("tcp", address, nil, proxy.Direct)
	if err != nil {
		log.Println("socket5 err", err.Error())
		return
	}

	var conn net.Conn
	conn, err = dialProxy.Dial("tcp", "www.baidu.com:80")
	if err != nil {
		log.Println("dail err", err.Error())
		return
	}
	//go test2(conn.(*net.TCPConn))
	//测试1, 不conn.Close等服务端超时，测试2,直接conn.Close，然后Sleep看日志情况
	conn.Close()
	log.Println("wait...")
	time.Sleep(time.Minute * 10)
}

func test2(conn *net.TCPConn) {
	n1, err := conn.Write([]byte("GET "))
	fmt.Println(n1, err)
	n2, err := conn.Write([]byte("http://www.baidu.com "))
	fmt.Println(n2, err)
	n3, err := conn.Write([]byte("HTTP/1.0"))
	fmt.Println(n3, err)
	n4, err := conn.Write([]byte("\r\nConnection:Close"))
	fmt.Println(n4, err)
}
