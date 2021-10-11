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
	establish(hub *Hub, id ksuid.KSUID, addr string, data []byte) error

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
	//	fmt.Println("dispatch", id, socketStream.Type)
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
		//fmt.Println("est", id, proxyEstMsg.Sorted)
		serverQueueHub.SetSort(id, proxyEstMsg.Sorted)
		serverLinkHub.SetSort(id, proxyEstMsg.Sorted)
		// 与外面建立连接，并把外面返回的数据放回websocket
		go establishProxy(hub, ProxyRegister{id, proxyEstMsg.Addr, estData})
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
		if requestMsg.Tag == TagEOF { //设置收到io.EOF结束符
			//fmt.Println("server receive eof")
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

	err := e.establish(hub, proxyMeta.id, proxyMeta.addr, proxyMeta.withData)
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
	serverQueueHub.Remove(id)
	return nil
}

// data: data send in establish step (can be nil).
func (e *DefaultProxyEst) establish(hub *Hub, id ksuid.KSUID, addr string, data []byte) error {
	// 安全检查addr或addr的解析地址不能为私有地址等
	if checkAddrPrivite(addr) {
		return errors.New("visit privite network, dial deny")
	}
	conn, err := net.DialTimeout("tcp", addr, time.Second*8) // todo config timeout
	if err != nil {
		return err
	}
	//收集请求发送出去
	serverLinkHub.TrySend(id, conn.(*net.TCPConn))
	defer func() {
		conn.Close()
		e.Close(id)
	}()

	// todo check exists
	hub.addNewProxy(&ProxyServer{Id: id, ProxyIns: e})
	defer hub.RemoveProxy(id)

	serverQueueHub.TrySend(id)
	writer := serverQueueHub.Get(id)
	if writer != nil {
		go func() {
			// 从外面往回接收数据
			_, err := pipe.CopyBuffer(writer, conn.(*net.TCPConn), addr)
			if err != nil {
				if strings.Contains(err.Error(), "connection reset by peer") {
				} else if strings.Contains(err.Error(), "use of closed network connection") {
				} else {
					log.Error("copy error: ", err)
				}
				// 这里不用告知客户端关闭，统一由establishProxy函数处理
			}
		}()
	}
	//fmt.Println(serverLinkHub.Len(), serverQueueHub.Len())
	//time.Sleep(time.Minute)
	//fmt.Println("wait")
	writer.Wait()
	//fmt.Println("done")
	// s.RemoveProxy(proxy.Id)
	// tellClosed is called outside this func.
	return nil
}
