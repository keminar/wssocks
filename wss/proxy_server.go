package wss

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/genshen/wssocks/pipe"
	"github.com/segmentio/ksuid"
	log "github.com/sirupsen/logrus"
	"nhooyr.io/websocket"
)

var serverQueueHub *pipe.QueueHub
var serverLinkHub *pipe.LinkHub

func init() {
	serverQueueHub = pipe.NewQueueHub()
	serverLinkHub = pipe.NewLinkHub()
}

func StaticServer() (int, int) {
	return serverQueueHub.Len(), serverLinkHub.Len()
}

type Connector struct {
	Conn io.ReadWriteCloser
}

// interface of establishing proxy connection with target
type ProxyEstablish interface {
	establish(hub *Hub, id ksuid.KSUID, addr string, data []byte, sorted []ksuid.KSUID) error

	// close connection
	Close(id ksuid.KSUID) error
}

type ClientData ServerData

func dispatchMessage(hub *Hub, msgType websocket.MessageType, data []byte, config WebsocksServerConfig) error {
	if msgType == websocket.MessageText {
		return dispatchDataMessage(hub, data, config)
	}
	return nil
}

func dispatchDataMessage(hub *Hub, data []byte, config WebsocksServerConfig) error {
	var socketData json.RawMessage
	socketStream := WebSocketMessage{
		Data: &socketData,
	}
	if err := json.Unmarshal(data, &socketStream); err != nil {
		fmt.Println(err)
		return err
	}

	// parsing id
	id, err := ksuid.Parse(socketStream.Id)
	if err != nil {
		fmt.Println(err)
		return err
	}
	// debug
	//if socketStream.Type != WsTpBeats {
	//	log.Debug("dispatch ", id, " ", socketStream.Type)
	//}

	switch socketStream.Type {
	case WsTpBeats: // heart beats
	case WsTpClose: // closed by client
		return hub.CloseProxyConn(id)
	case WsTpHi:
		var masterID ksuid.KSUID
		if err := json.Unmarshal(socketData, &masterID); err != nil {
			return err
		}
		writer := NewWebSocketWriter(&hub.ConcurrentWebSocket, id, context.Background())

		serverQueueHub.Add(masterID, id, writer)
		serverLinkHub.Add(id, masterID)
		//fmt.Println(time.Now(), "get client say", id, masterID)
	case WsTpEst: // establish 收到连接请求
		var proxyEstMsg ProxyEstMessage
		if err := json.Unmarshal(socketData, &proxyEstMsg); err != nil {
			return err
		}

		var estData []byte = nil
		if proxyEstMsg.WithData {
			if decodedBytes, err := base64.StdEncoding.DecodeString(proxyEstMsg.DataBase64); err != nil {
				log.Error("base64 decode error,", err)
				return err
			} else {
				estData = decodedBytes
			}
		}
		// 与外面建立连接，并把外面返回的数据放回websocket
		go establishProxy(hub, ProxyRegister{id, proxyEstMsg.Addr, estData, proxyEstMsg.Sorted})
	case WsTpData: //从websocket收到数据发送到外面
		var requestMsg ProxyData
		if err := json.Unmarshal(socketData, &requestMsg); err != nil {
			fmt.Println("json", err)
			return err
		}

		link := serverLinkHub.Get(id)
		if link == nil {
			// 远程请求的服务器主动断开，establish函数结束，连接已经被主连接释放
			// 此时客户端还来数据包则会到此, 不算出错
			//fmt.Println(time.Now(), id, requestMsg.Tag, "link not found")
			return nil
		}
		// 注意虽然WsTpData一定在WsTpEst之后收到 ，但这边代码的执行不一定会在establish中代码准备完成之后
		//    因为establish中代码执行也需要时间，同时data就会发送过来，需要当成并发来看待
		if requestMsg.Tag == TagEOF { //设置收到io.EOF结束符
			//log.Debug("server receive eof ", id)
			link.WriteEOF()
			return nil
		}
		if decodeBytes, err := base64.StdEncoding.DecodeString(requestMsg.DataBase64); err != nil {
			log.Error("base64 decode error,", err)
			return err
		} else {
			//fmt.Println("bytes", id, len(decodeBytes), string(decodeBytes))
			// 传输数据
			link.Write(decodeBytes)
			return nil
		}
	}
	return nil
}

func establishProxy(hub *Hub, proxyMeta ProxyRegister) {
	var e ProxyEstablish
	e = &DefaultProxyEst{}

	err := e.establish(hub, proxyMeta.id, proxyMeta.addr, proxyMeta.withData, proxyMeta.sorted)
	if err != nil {
		log.Info(fmt.Sprintf("establish %s %s\n", proxyMeta.addr, err.Error()))
	}
	// 与后端服务器交互结束，让客户端断开
	hub.tellClosed(proxyMeta.id) // tell client to close connection.
}

// interface implementation for socks5 and https proxy.
type DefaultProxyEst struct {
	status string
}

func (e *DefaultProxyEst) Close(id ksuid.KSUID) error {
	if e.status == "closed" {
		return nil
	}
	e.status = "closed"
	//fmt.Println(time.Now(), "close", id)
	serverLinkHub.RemoveAll(id)
	serverQueueHub.RemoveAll(id)
	return nil
}

// data: data send in establish step (can be nil).
func (e *DefaultProxyEst) establish(hub *Hub, id ksuid.KSUID, addr string, data []byte, sorted []ksuid.KSUID) error {
	// 安全检查addr或addr的解析地址不能为私有地址等
	if checkAddrPrivite(addr) {
		return errors.New("visit privite network, dial deny")
	}
	logTag := addr + ":" + id.String()
	// 调试函数，方便针对域名输出日志
	debugPrint := func(args ...interface{}) {
		if !pipe.DebugLog {
			return
		}
		if strings.Contains(addr, pipe.DebugLogDomain) {
			log.Debug(args...)
		}
	}
	debugPrint(timeNow(), fmt.Sprintf(" %s start\n", logTag))

	conn, err := net.DialTimeout("tcp", addr, time.Second*8) // todo config timeout
	if err != nil {
		return err
	}
	defer func() {
		conn.Close()
		e.Close(id)
	}()

	// 要先执行，初始化数据
	serverLinkHub.SetSort(id, sorted)
	//收集请求发送出去到后端
	serverLinkHub.TrySend(id, conn.(*net.TCPConn))
	// SetSort后一定会存在
	link := serverLinkHub.Get(id)
	if link == nil {
		return errors.New("link not found")
	}
	// 定义一个超时, 默认是一个较长超时
	d := pipe.NewDead()
	go func() {
		link.Wait()
		debugPrint(timeNow(), fmt.Sprintf(" %s send request done\n", logTag))
		// 写已经结束，修改读超时为短超时, 因为发现有时发送了closeWrite还是会一直卡住read
		d.Line = time.Duration(5) * time.Second
		// 马上执行一次，让当前卡住的读也用短超时
		conn.SetReadDeadline(time.Now().Add(d.Line))
	}()

	// todo check exists
	hub.addNewProxy(&ProxyServer{Id: id, ProxyIns: e})
	defer hub.RemoveProxy(id)

	// 从后端接收数据
	serverQueueHub.SetSort(id, sorted)
	back := serverQueueHub.Get(id)
	if back == nil {
		return errors.New("queue not found")
	}
	back.Read = func() {
		// 从外面往回接收数据
		_, err := pipe.CopyBuffer(func(i int) pipe.PipeWriter {
			pos := i % len(sorted)
			return serverQueueHub.Get(sorted[pos])
		}, conn.(*net.TCPConn), d, logTag)
		if err != nil {
			if strings.Contains(err.Error(), "connection reset by peer") {
			} else if strings.Contains(err.Error(), "use of closed network connection") {
			} else {
				log.Error("copy error: ", err)
			}
			// 这里不用告知客户端关闭，统一由establishProxy函数处理
		}
		debugPrint(timeNow(), fmt.Sprintf(" %s get response done err=", logTag), err)
	}
	// 务必要写在SetSort调用后面
	serverQueueHub.TrySend(id)

	//fmt.Println("wait")
	back.Wait()
	debugPrint(timeNow(), fmt.Sprintf(" %s all done\n", logTag))
	// s.RemoveProxy(proxy.Id)
	// tellClosed is called outside this func.
	return nil
}
