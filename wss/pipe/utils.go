package pipe

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// 是否打开调试日志
var pipeDebug bool = false

// 数据过期时间
var expHour time.Duration = time.Duration(1) * time.Hour
var expMinute time.Duration = time.Duration(1) * time.Minute
var expFiveMinute time.Duration = time.Duration(5) * time.Minute

// DebugLog 是否记录请求关键点位和慢读写日志
var DebugLog bool = true

// 慢读写的时间
var slowTime time.Duration = time.Duration(1) * time.Second

// DebugLogDomain 记录此域名的详细请求日志，在DebugLog为true时生效
var DebugLogDomain = ""

// 状态值
const (
	StaWait  = "wait"  //表示Send函数还没有执行
	StaSend  = "send"  //表示Send函数正在执行
	StaDone  = "done"  //表示Send函数已经退出
	StaClose = "close" //表示close函数已经执行
)

type PipeWriter interface {
	Write(p []byte) (n int, err error)
	WriteEOF()
}

type buffer struct {
	eof  bool
	data []byte
}

// CopyBuffer 传输数据
func CopyBuffer(getWriter func(i int) PipeWriter, conn *net.TCPConn, d *dead, stop chan struct{}, addr string) (written int64, err error) {
	// 调试函数，方便针对域名输出日志
	debugPrint := func(args ...interface{}) {
		if strings.Contains(addr, DebugLogDomain) {
			log.Debug(args...)
		}
	}
	// 比较有没有达到记录日志条件
	debugCompare := func(action string, s1 time.Time, nr int) {
		if !DebugLog {
			return
		}
		diff := time.Since(s1)
		if diff > slowTime {
			debugPrint(time.Now().Format(time.RFC3339Nano), fmt.Sprintf(" %s %s %d cost ", addr, action, nr), diff)
		}
	}
	//如果设置过大会耗内存高
	//通过设置1k，2k，4k，8k做对比，2k速度最快
	size := 2 * 1024
	if pipeDebug {
		size = 10 //临时测试
	}
	buf := make([]byte, size)
	i := 0
	for {
		// 先检查stop 如果已经被close 不再接收新请求
		select {
		case <-stop:
			return
		default:
			// if the channel is still open, continue as normal
		}
		s1 := time.Now()
		//debugPrint("read start ", addr, " ", d.Line)
		conn.SetReadDeadline(time.Now().Add(d.Line))
		nr, er := conn.Read(buf)
		debugCompare("read done", s1, nr)

		pw := getWriter(i)
		if pw == nil {
			return 0, errors.New("pw not found")
		}
		if nr > 0 {
			//fmt.Println("copy read", nr)
			var nw int
			var ew error
			s1 := time.Now()
			nw, ew = pw.Write(buf[0:nr])
			debugCompare("write", s1, nr)
			if nw > 0 {
				written += int64(nw)
			}
			if ew != nil {
				err = fmt.Errorf("#1 %s", ew.Error())
				break
			}
			if nr != nw {
				//panic(ew)
				// 还没写入，chan被关闭了
				err = fmt.Errorf("#2 %d!=%d, %s", nr, nw, io.ErrShortWrite.Error())
				break
			}
		}
		if er == io.EOF {
			fmt.Println("test1")
			// 请求正常结束或客户端curl被ctrl+c断开都能走到这边
			debugPrint(time.Now(), " copy get and write eof")
			pw.WriteEOF()
			break
		} else if er != nil {
			fmt.Println("test2", er.Error())
			//多写一个EOF让外层从chan的读取也退出
			//比如当proxy_server向外面发送了closeWrite后，这边read已经超时退出，但是pipe.Send函数还卡着
			pw.WriteEOF()
			err = fmt.Errorf("#3 %s %s", er.Error(), addr)
			break
		}
		i++
	}
	return written, err
}

// 带保护写，防止conn变nil时退出
func safeWrite(conn *net.TCPConn, data []byte, closeWrite bool) (n int, err error) {
	defer func() {
		if err := recover(); err != nil {
			return
		}
	}()
	if conn == nil {
		return 0, errors.New("conn is closed")
	}
	if closeWrite {
		//log.Debug("send closeWrite")
		err = conn.CloseWrite()
		return 0, err
	}
	return conn.Write(data)
}

// 带有超时的读
func readWithTimeout(b chan buffer, exp time.Duration) (buffer, error) {
	for {
		select {
		case <-time.After(exp):
			return buffer{}, errors.New("time out")
		case data, ok := <-b:
			if ok {
				return data, nil
			}
			return buffer{}, errors.New("chan closed")
		}
	}
}

// 创建缓冲区
func makeBuffer() chan buffer {
	if pipeDebug {
		return make(chan buffer, 1)
	}
	// 减少内存占用
	return make(chan buffer, 5)
}

// 打印日志
func pipePrintln(a ...interface{}) (n int, err error) {
	if !pipeDebug {
		return 0, nil
	}
	return fmt.Println(a...)
}
