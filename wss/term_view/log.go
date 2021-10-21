package term_view

import (
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/genshen/wssocks/wss"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh/terminal"
)

type ProgressLog struct {
	Writer *Writer // terminal writer  todo defer Flush
	record *wss.ConnRecord
	last   int64
}

func NewPLog(cr *wss.ConnRecord) *ProgressLog {
	plog := ProgressLog{
		record: cr,
	}
	plog.Writer = NewWriter()
	plog.last = time.Now().Unix() - 1 //首次允许执行
	return &plog
}

// set progress log(connection table),
// the connection table is write into p.Write
// (p.Write is a bytes buffer, only really output to screen when calling Flush).
func (p *ProgressLog) SetLogBuffer(r *wss.ConnRecord) {
	//return
	if time.Now().Unix() == p.last { //减少一些并发打印
		return
	}
	p.last = time.Now().Unix()
	_, terminalRows, err := terminal.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		logrus.Error(err)
		return
	}
	// log size is ok for terminal (at least one row)
	w := new(tabwriter.Writer)

	w.Init(p.Writer, 0, 0, 5, ' ', 0)
	defer w.Flush()

	if terminalRows > len(r.Wsc)+1 {
		for i, ws := range r.Wsc {
			un, us := ws.Upload.Get()
			dn, ds := ws.Download.Get()
			fmt.Fprintf(w, "Conn(%d): Up:(%d/%s)\tDown:(%d/%s)\t\n", i, un, us.String(), dn, ds.String())
			terminalRows--
		}

		// 显示当前传输全局变量释放情况
		_, _ = fmt.Fprintf(w, "Pipe Status: \tQueueLen: %d LinkLen: %d\t\n", r.QueueHubLen, r.LinkHubLen)
		terminalRows--
	}

	if terminalRows >= 2 { // at least 2 lines left: one for show more records and one for new line(\n).
		var recordsHiden = len(r.Addresses)
		_, _ = fmt.Fprintf(w, "TARGETs\tCONNECTIONs\t\n")
		terminalRows--
		// have rows left
		for addr, size := range r.Addresses {
			if terminalRows <= 2 {
				// hide left records
				break
			} else {
				_, _ = fmt.Fprintf(w, "%s\t%d\t\n", addr, size)
				terminalRows--
				recordsHiden--
			}
		}
		// log total connection size.
		if recordsHiden == 0 {
			_, _ = fmt.Fprintf(w, "TOTAL\t%d\t\n", r.ConnSize)
		} else {
			_, _ = w.Write([]byte(fmt.Sprintf("TOTAL\t%d\t(%d record(s) hidden)\t\n",
				r.ConnSize, recordsHiden)))
		}
	}
}

// write interface: write buffer data directly to stdout.
func (p *ProgressLog) Write(buf []byte) (int, error) {
	p.record.Mutex.Lock()
	defer p.record.Mutex.Unlock()
	p.SetLogBuffer(p.record)             // call Writer.Write() to set log data into buffer
	err := p.Writer.Flush(func() error { // flush buffer
		if _, err := p.Writer.OutDev.Write(buf); err != nil { // just write buff to stdout, and keep progress log.
			return err
		}
		return nil
	})
	return len(buf), err
}
