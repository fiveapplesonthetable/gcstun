package main

// Multiplexed entry (RU box). A SOCKS5 server where EVERY connection is a "stream"
// carried over one shared pair of GCS object streams. All streams' bytes are batched
// into up/<seq> objects; the response batches (down/<seq>) are demuxed back to each
// stream. Ten connections handshaking share ONE GCS round-trip per batch instead of
// paying ~1.2s each.

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

type muxClient struct {
	g      *GCS
	out    *outbox
	conns  sync.Map // uint32 -> net.Conn
	nextID uint32
}

// socksTarget does the SOCKS5 handshake and returns the requested host:port.
func socksTarget(c net.Conn) (string, bool) {
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(c, hdr); err != nil || hdr[0] != 5 {
		return "", false
	}
	io.ReadFull(c, make([]byte, hdr[1]))
	c.Write([]byte{5, 0})
	req := make([]byte, 4)
	if _, err := io.ReadFull(c, req); err != nil || req[1] != 1 {
		c.Write([]byte{5, 7, 0, 1, 0, 0, 0, 0, 0, 0})
		return "", false
	}
	var host string
	switch req[3] {
	case 1:
		b := make([]byte, 4)
		io.ReadFull(c, b)
		host = net.IP(b).String()
	case 3:
		l := make([]byte, 1)
		io.ReadFull(c, l)
		b := make([]byte, l[0])
		io.ReadFull(c, b)
		host = string(b)
	case 4:
		b := make([]byte, 16)
		io.ReadFull(c, b)
		host = net.IP(b).String()
	default:
		return "", false
	}
	pb := make([]byte, 2)
	io.ReadFull(c, pb)
	return fmt.Sprintf("%s:%d", host, int(binary.BigEndian.Uint16(pb))), true
}

func (m *muxClient) handle(c net.Conn) {
	target, ok := socksTarget(c)
	if !ok {
		c.Close()
		return
	}
	id := atomic.AddUint32(&m.nextID, 1)
	m.conns.Store(id, c)
	m.out.enq(id, fOpen, []byte(target))
	c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}) // optimistic success (saves a round-trip)
	buf := make([]byte, 256*1024)
	for {
		n, err := c.Read(buf)
		if n > 0 {
			m.out.enq(id, fData, append([]byte(nil), buf[:n]...))
		}
		if err != nil {
			break
		}
	}
	m.conns.Delete(id)
	m.out.enq(id, fClose, nil)
	c.Close()
}

// downReader pulls down/<seq> in order (with a parallel prefetch window for throughput),
// demuxes each batch, and writes each frame to its stream's SOCKS connection.
func (m *muxClient) downReader() {
	fetch := func(seq int) []byte {
		name := fmt.Sprintf("down/%d", seq)
		for {
			b, st, err := m.g.Get(name)
			if err == nil && st == 200 {
				go m.g.Delete(name)
				return b
			}
			time.Sleep(muxPoll)
		}
	}
	inflight := map[int]chan []byte{}
	start := func(seq int) {
		ch := make(chan []byte, 1)
		inflight[seq] = ch
		go func() { ch <- fetch(seq) }()
	}
	for i := 0; i < muxWindow; i++ {
		start(i)
	}
	next := 0
	for {
		b := <-inflight[next]
		delete(inflight, next)
		parseFrames(b, func(stream uint32, typ byte, data []byte) {
			switch typ {
			case fData:
				if v, ok := m.conns.Load(stream); ok {
					v.(net.Conn).Write(data)
				}
			case fClose:
				if v, ok := m.conns.LoadAndDelete(stream); ok {
					v.(net.Conn).Close()
				}
			}
		})
		start(next + muxWindow)
		next++
	}
}

func runMuxClient(args []string) {
	fs := flag.NewFlagSet("muxclient", flag.ExitOnError)
	key := fs.String("key", "/root/gcs-key.json", "service account key")
	bucket := fs.String("bucket", "cyclevpn-xport-eu", "GCS bucket")
	listen := fs.String("listen", "127.0.0.1:10921", "SOCKS5 listen")
	flush := fs.Duration("flush", muxFlush, "batch coalesce window")
	win := fs.Int("window", muxWindow, "parallel down prefetches")
	fs.Parse(args)
	muxFlush, muxWindow = *flush, *win
	kb, err := os.ReadFile(*key)
	die(err)
	g, err := NewGCS(kb, *bucket)
	die(err)
	m := &muxClient{g: g, out: &outbox{g: g, prefix: "up"}}
	go m.out.run()
	go m.downReader()
	ln, err := net.Listen("tcp", *listen)
	die(err)
	log.Printf("gcstun muxclient: SOCKS5 %s <-> gs://%s (multiplexed)", *listen, *bucket)
	for {
		c, err := ln.Accept()
		if err != nil {
			continue
		}
		go m.handle(c)
	}
}
