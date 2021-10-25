package wss

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/genshen/wssocks/wss/pipe"
	"github.com/segmentio/ksuid"
	log "github.com/sirupsen/logrus"
)

var StoppedError = errors.New("listener stopped")

var clientQueueHub *pipe.QueueHub
var clientLinkHub *pipe.LinkHub

func init() {
	clientQueueHub = pipe.NewQueueHub()
	clientLinkHub = pipe.NewLinkHub()
}

// client part of wssocks
type Client struct {
	tcpl    *net.TCPListener
	stop    chan interface{}
	closed  bool
	wgClose sync.WaitGroup // wait for closing
	status  string         //资源释放状态
}

func NewClient() *Client {
	var client Client
	client.closed = false
	client.stop = make(chan interface{})
	return &client
}

// parse target address and proxy type, and response to socks5/https client
func (client *Client) Reply(conn net.Conn) ([]byte, int, string, error) {
	var buffer [1024]byte
	var addr string
	var proxyType int

	n, err := conn.Read(buffer[:])
	if err != nil {
		return nil, 0, "", err
	}

	// 去掉socks5之外的支持
	// select a matched proxy type
	instances := []ProxyInterface{&Socks5Client{}}
	var matchedInstance ProxyInterface = nil
	for _, proxyInstance := range instances {
		if proxyInstance.Trigger(buffer[:n]) {
			matchedInstance = proxyInstance
			break
		}
	}

	if matchedInstance == nil {
		return nil, 0, "", errors.New("only socks5 proxy")
	}

	// set address and type
	if proxyAddr, err := matchedInstance.ParseHeader(conn, buffer[:n]); err != nil {
		return nil, 0, "", err
	} else {
		proxyType = matchedInstance.ProxyType()
		addr = proxyAddr
	}
	// set data sent in establish step.
	if firstSendData, err := matchedInstance.EstablishData(buffer[:n]); err != nil {
		return nil, 0, "", err
	} else {
		// firstSendData can be nil, which means there is no data to be send during connection establishing.
		return firstSendData, proxyType, addr, nil
	}
}

// listen on local address:port and forward socks5 requests to wssocks server.
func (client *Client) ListenAndServe(record *ConnRecord, wsc []*WebSocketClient, address string, onConnected func()) error {
	netListener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	tcpl, ok := (netListener).(*net.TCPListener)
	if !ok {
		return errors.New("not a tcp listener")
	}
	client.tcpl = tcpl

	// 在client刚启动，连上ws server以后要做的事
	onConnected()
	for {
		// 先检查stop 如果已经被close 不再接收新请求
		select {
		case <-client.stop:
			return StoppedError
		default:
			// if the channel is still open, continue as normal
		}

		c, err := tcpl.Accept()
		if err != nil {
			return fmt.Errorf("tcp accept error: %w", err)
		}

		run := func(c net.Conn) {
			conn := c.(*net.TCPConn)
			// defer c.Close()
			defer conn.Close()
			// In reply, we can get proxy type, target address and first send data.
			firstSendData, proxyType, addr, err := client.Reply(conn)
			if err != nil {
				log.Error("reply error: ", err)
				return
			}
			// 在client.Close中使用wait等待
			client.wgClose.Add(1)
			defer client.wgClose.Done()

			switch proxyType {
			case ProxyTypeSocks5:
				conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
			case ProxyTypeHttps:
				conn.Write([]byte("HTTP/1.0 200 Connection Established\r\nProxy-agent: wssocks\r\n\r\n"))
			}

			// 更新输出区域的连接数据
			record.Update(ConnStatus{IsNew: true, Address: addr, Type: proxyType})
			defer record.Update(ConnStatus{IsNew: false, Address: addr, Type: proxyType})

			// 检查addr或addr的解析地址是不是私有地址等
			if checkAddrPrivite(addr) {
				if err := client.localVisit(conn, firstSendData, addr); err != nil {
					log.Error("visit error: ", err)
				}
				return
			}
			// 传输数据
			// on connection established, copy data now.
			if err := client.transData(wsc, conn, firstSendData, addr); err != nil {
				log.Error("trans error: ", err)
			}
		}
		go run(c)
	}
}

// 本地域名请求
func (client *Client) localVisit(conn *net.TCPConn, firstSendData []byte, addr string) error {
	remote, err := net.DialTimeout("tcp", addr, time.Second*8) // todo config timeout
	if err != nil {
		return err
	}
	defer remote.Close()

	stop := make(chan struct{})
	d := pipe.NewDead()
	wg := sync.WaitGroup{}
	wg.Add(2)
	go func() {
		defer wg.Done()
		if len(firstSendData) > 0 { //debug
			panic(string(firstSendData))
		}
		remote.Write(firstSendData)
		pipe.CopyBuffer(func(i int) pipe.PipeWriter {
			return pipe.NewTcp(remote.(*net.TCPConn))
		}, conn, d, stop, addr)
	}()
	go func() {
		defer wg.Done()
		pipe.CopyBuffer(func(i int) pipe.PipeWriter {
			return pipe.NewTcp(conn)
		}, remote.(*net.TCPConn), d, stop, addr)
	}()
	wg.Wait()
	close(stop)
	return nil
}

// 传输数据
func (client *Client) transData(wsc []*WebSocketClient, conn *net.TCPConn, firstSendData []byte, addr string) error {
	var masterProxy *ProxyClient
	var masterWsc *WebSocketClient
	var masterID ksuid.KSUID
	var sorted []ksuid.KSUID
	// 注: 因为socks5首包是交互协议firstSendData永远是空
	if len(firstSendData) > 0 { //debug
		panic(string(firstSendData))
	}
	var once sync.Once
	stop := make(chan struct{})
	// 单次释放资源，因为client类对多次请求是共享的，所以不能用它的方法实现。这里定义个局部函数
	remove := func(id ksuid.KSUID) {
		close(stop)
		conn.SetReadDeadline(time.Now())
		time.Sleep(time.Second)
		clientQueueHub.RemoveAll(id)
		clientLinkHub.RemoveAll(id)
	}

	// 调试函数，方便针对域名输出日志
	debugPrint := func(args ...interface{}) {
		if !pipe.DebugLog {
			return
		}
		if strings.Contains(addr, pipe.DebugLogDomain) {
			fmt.Println(args...)
		}
	}

	d := pipe.NewDead()
	for i, w := range wsc {
		// create a with proxy with callback func
		p := w.NewProxy(func(id ksuid.KSUID, data ServerData) { //ondata 接收数据回调
			link := clientLinkHub.Get(id)
			if link == nil {
				// 已经收到了close后再收到的数据
				debugPrint(timeNow(), masterID, "link is nil", id)
				return
			}
			if data.Tag == TagData {
				debugPrint(timeNow(), masterID, fmt.Sprintf("%s receive data %d from server", id, len(data.Data)))
				link.Write(data.Data)
			} else if data.Tag == TagEOF {
				debugPrint(timeNow(), masterID, fmt.Sprintf("%s receive eof from server", id))
				//fmt.Println("client receive eof")
				link.WriteEOF()
			}
		}, func(id ksuid.KSUID, tell bool) { //onclosed 只有主连接会调到
			debugPrint(timeNow(), masterID, fmt.Sprintf("%s receive close from server", id))
			// 释放资源
			once.Do(func() {
				remove(id)
			})
		}, func(id ksuid.KSUID, err error) { //onerror
		})
		defer w.RemoveProxy(p.Id)

		// 第一个做为主id
		if i == 0 {
			masterWsc = w
			masterID = p.Id
			masterProxy = p
		}

		// 给主链接发送的顺序
		sorted = append(sorted, p.Id)
		// 让各自连接准备，对方收到后与总连接数对比决定是否开始向外转发
		// 最好放在Establish前发送，这样Establish数据得到进行setSort时map一定存在
		p.SayID(w, masterID)

		// trans incoming data from proxy client application.
		ctx, cancel := context.WithCancel(context.Background())
		writer := NewWebSocketWriterWithMutex(&w.ConcurrentWebSocket, p.Id, ctx)
		defer writer.CloseWsWriter(cancel)

		clientQueueHub.Add(masterID, p.Id, writer)
		clientLinkHub.Add(p.Id, masterID)
	}
	// 删除map中数据
	defer func() {
		once.Do(func() {
			remove(masterID)
		})
	}()

	// 补充ID，方便分析日志
	logTag := "none"
	if strings.Contains(addr, pipe.DebugLogDomain) {
		logTag = masterID.String()
	}
	debugPrint(timeNow(), masterID, addr, "start")

	// 告知服务端目标地址和协议，还有首次发送的数据包, 额外告知有几路以及顺序如何
	// 第二到N条线路不需要Establish因为不用和目标机器连接
	if err := masterProxy.Establish(masterWsc, firstSendData, addr, sorted); err != nil {
		return err
	}

	debugPrint(timeNow(), sorted)
	//初始化数据
	clientQueueHub.SetSort(masterID, sorted)
	qq := clientQueueHub.Get(masterID)
	if qq == nil {
		return errors.New("queue not found")
	}
	// 自定义读取函数从浏览器读取并写入buffer
	qq.ReadData = func() {
		_, err := pipe.CopyBuffer(func(i int) pipe.PipeWriter {
			pos := i % len(sorted)
			return clientQueueHub.Get(sorted[pos])
		}, conn, d, stop, logTag)
		if err != nil {
			if !strings.Contains(err.Error(), "use of closed network connection") {
				log.Error("copy error: ", err)
			}
			// 发送Close给server，只给主连接发送就行
			//masterWsc.TellClose(masterID)
		}
		debugPrint(timeNow(), masterID, "read request done err=", err)
	}
	clientQueueHub.TrySend(masterID)
	go func() {
		// 正常发送EOF后结束，或者整个transData函数已经退出，chan被关闭
		qq.Wait()
		debugPrint(timeNow(), masterID, "send request done")
	}()

	// 初始化
	clientLinkHub.SetSort(masterID, sorted)
	// 接收的数据发送到哪
	clientLinkHub.TrySend(masterID, conn)
	//接收数据
	back := clientLinkHub.Get(masterID)
	if back == nil {
		return errors.New("link not found")
	}

	//fmt.Println("wait")
	back.Wait()
	// 全部数据接收完成,通知服务器交换函数结束
	masterWsc.TellClose(masterID)
	debugPrint(timeNow(), masterID, "all done")
	return nil
}

// Close stops listening on the TCP address,
// But the active links are not closed and wait them to finish.
func (client *Client) Close(wait bool) error {
	if client.closed {
		return nil
	}
	close(client.stop)
	client.closed = true
	err := client.tcpl.Close()
	if wait {
		client.wgClose.Wait() // wait the active connection to finish
	}
	return err
}

func timeNow() string {
	return time.Now().Format(time.RFC3339Nano)
}
