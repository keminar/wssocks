package wss

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/genshen/wssocks/pipe"
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

		go func() {
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
		}()
	}
}

// 本地域名请求
func (client *Client) localVisit(conn *net.TCPConn, firstSendData []byte, addr string) error {
	remote, err := net.DialTimeout("tcp", addr, time.Second*8) // todo config timeout
	if err != nil {
		return err
	}
	defer remote.Close()

	d := pipe.NewDead()
	wg := sync.WaitGroup{}
	wg.Add(2)
	go func() {
		defer wg.Done()
		if len(firstSendData) > 0 { //debug
			panic(string(firstSendData))
		}
		remote.Write(firstSendData)
		pipe.CopyBuffer(pipe.NewTcp(remote.(*net.TCPConn)), conn, d, addr)
	}()
	go func() {
		defer wg.Done()
		pipe.CopyBuffer(pipe.NewTcp(conn), remote.(*net.TCPConn), d, addr)
	}()
	wg.Wait()
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
	// 单次释放资源，因为client类对多次请求是共享的，所以不能用它的方法实现。这里定义个局部函数
	remove := func(id ksuid.KSUID) {
		clientQueueHub.Remove(id)
		clientLinkHub.RemoveAll(id)
	}

	// 前置定义，为了在onclosed回调时也能用
	logTag := addr
	// 调试函数，方便针对域名输出日志
	debugPrint := func(args ...interface{}) {
		if !pipe.DebugLog {
			return
		}
		if strings.Contains(addr, pipe.DebugLogDomain) {
			log.Debug(args...)
		}
	}
	for i, w := range wsc {
		// create a with proxy with callback func
		p := w.NewProxy(func(id ksuid.KSUID, data ServerData) { //ondata 接收数据回调
			link := clientLinkHub.Get(id)
			if link == nil {
				return
			}
			if data.Tag == TagData {
				link.Write(data.Data)
			} else if data.Tag == TagEOF {
				//fmt.Println("client receive eof")
				link.WriteEOF()
			}
		}, func(id ksuid.KSUID, tell bool) { //onclosed 只有主连接会调到
			debugPrint(timeNow(), fmt.Sprintf(" %s receive close from server\n", logTag))
			//服务器出错让关闭，关闭双向的通道, 结束wait等待
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
	logTag = logTag + ":" + masterID.String()
	debugPrint(timeNow(), fmt.Sprintf(" %s start\n", logTag))

	// 告知服务端目标地址和协议，还有首次发送的数据包, 额外告知有几路以及顺序如何
	// 第二到N条线路不需要Establish因为不用和目标机器连接
	if err := masterProxy.Establish(wsc[0], firstSendData, addr, sorted); err != nil {
		return err
	}

	//发送数据
	qq := clientQueueHub.Get(masterID)
	if qq == nil {
		return errors.New("queue not found")
	}
	// 设置发送顺序
	qq.SetSort(sorted)

	go func() {
		qq.Send()
		debugPrint(timeNow(), fmt.Sprintf(" %s send request done\n", logTag))
	}()

	go func() {
		d := pipe.NewDead()
		_, err := pipe.CopyBuffer(qq, conn, d, logTag) //io.Copy(qq, conn)
		if err != nil {
			if !strings.Contains(err.Error(), "use of closed network connection") {
				log.Error("copy error: ", err)
			}
			// 发送Close给server，只给主连接发送就行
			masterWsc.TellClose(masterID)
		}
		debugPrint(timeNow(), fmt.Sprintf(" %s copy request done err=", logTag), err)
	}()

	//接收数据
	oo := clientLinkHub.Get(masterID)
	if oo == nil {
		return errors.New("link not found")
	}
	// 设置接收的数据发送到哪
	oo.SetConn(conn)
	oo.SetSort(sorted)
	go func() {
		oo.Send(clientLinkHub)
		debugPrint(timeNow(), fmt.Sprintf(" %s get response done\n", logTag))
	}()

	//fmt.Println(clientLinkHub.Len(), clientQueueHub.Len())
	//time.Sleep(time.Minute)
	//fmt.Println("wait")
	oo.Wait()
	debugPrint(timeNow(), fmt.Sprintf(" %s all done\n", logTag))
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
