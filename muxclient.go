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
	conns  sync.Map // uint32 -> *cstream
	nextID uint32
}

type cstream struct {
	conn   net.Conn
	outSeq uint32 // next up-DATA streamSeq to send (read loop only)
	inSeq  uint32 // next down-DATA streamSeq expected (downReader only)
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
	s := &cstream{conn: c}
	m.conns.Store(id, s)
	m.out.enq(id, fOpen, 0, []byte(target))
	c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}) // optimistic success (saves a round-trip)
	buf := make([]byte, 256*1024)
	for {
		n, err := c.Read(buf)
		if n > 0 {
			m.out.enq(id, fData, s.outSeq, append([]byte(nil), buf[:n]...))
			s.outSeq++
		}
		if err != nil {
			break
		}
	}
	m.conns.Delete(id)
	m.out.enq(id, fClose, 0, nil)
	c.Close()
}

func (m *muxClient) closeAllClient() {
	m.conns.Range(func(k, v any) bool {
		m.conns.Delete(k)
		v.(*cstream).conn.Close()
		return true
	})
}

func (m *muxClient) route(b []byte) {
	parseFrames(b, func(stream uint32, typ byte, sseq uint32, data []byte) {
		v, ok := m.conns.Load(stream)
		if !ok {
			return
		}
		s := v.(*cstream)
		switch typ {
		case fData:
			if sseq != s.inSeq { // gap/dup — reset this stream cleanly, never deliver garbled bytes
				if muxDebug {
					log.Printf("mux client: stream %d down-gap got %d want %d — closing", stream, sseq, s.inSeq)
				}
				m.conns.Delete(stream)
				s.conn.Close()
				return
			}
			s.inSeq++
			s.conn.Write(data)
		case fClose:
			m.conns.Delete(stream)
			s.conn.Close()
		}
	})
}

// downReader pulls down/<seq> in order (parallel prefetch window for throughput), demuxes,
// and delivers each frame to its stream. If it stalls on a missing object (loss or a relay
// restart), it re-lists and resyncs (a generation counter abandons the stuck prefetches);
// the per-stream seq in route() cleanly resets any stream that lost a batch.
func (m *muxClient) downReader() {
	var gen int64
	fetch := func(seq int, g int64) []byte {
		name := fmt.Sprintf("down/%d", seq)
		for atomic.LoadInt64(&gen) == g {
			b, st, err := m.g.Get(name)
			if err == nil && st == 200 {
				go m.g.Delete(name)
				return b
			}
			time.Sleep(muxPoll)
		}
		return nil // abandoned by a resync
	}
	for { // (re)start the window from the current position
		g := atomic.LoadInt64(&gen)
		next := 0
		if lo := minSeq(m.g, "down"); lo > 0 {
			next = lo
		}
		inflight := map[int]chan []byte{}
		start := func(seq int) {
			ch := make(chan []byte, 1)
			inflight[seq] = ch
			go func() { ch <- fetch(seq, g) }()
		}
		for i := 0; i < muxWindow; i++ {
			start(next + i)
		}
		resync := false
		for !resync {
			select {
			case b := <-inflight[next]:
				delete(inflight, next)
				if b == nil { // abandoned
					resync = true
					break
				}
				m.route(b)
				start(next + muxWindow)
				next++
			case <-time.After(muxStall):
				if lo := minSeq(m.g, "down"); lo >= 0 && lo != next {
					if muxDebug {
						log.Printf("mux client: down resync %d -> %d", next, lo)
					}
					if lo < next {
						m.closeAllClient() // relay restarted — drop stale streams
					}
					atomic.AddInt64(&gen, 1) // abandon in-flight fetches; outer loop restarts
					resync = true
				}
			}
		}
	}
}

func runMuxClient(args []string) {
	fs := flag.NewFlagSet("muxclient", flag.ExitOnError)
	key := fs.String("key", "/root/gcs-key.json", "service account key")
	bucket := fs.String("bucket", "cyclevpn-xport-eu", "GCS bucket")
	listen := fs.String("listen", "127.0.0.1:10921", "SOCKS5 listen")
	flush := fs.Duration("flush", muxFlush, "batch coalesce window")
	win := fs.Int("window", muxWindow, "parallel down prefetches")
	dbg := fs.Bool("debug", false, "log resyncs and per-stream gaps")
	fs.Parse(args)
	muxFlush, muxWindow, muxDebug = *flush, *win, *dbg
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
