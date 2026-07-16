package main

// Multiplexed exit. Reads ONE ordered stream of up/<seq> objects (each a batch of
// frames for many connections), demuxes, and dials/writes per stream. All destinations'
// responses are batched back into ONE ordered stream of down/<seq> objects. The GCS
// round-trip latency is thus shared by every active connection at once.

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"time"
)

var (
	muxFlush  = 15 * time.Millisecond // coalesce outgoing frames this long (latency floor per batch)
	muxChunk  = 1024 * 1024           // ...but flush early past this size (throughput for bulk)
	muxPoll   = 40 * time.Millisecond // poll interval waiting for the next object
	muxWindow = 16                    // client: parallel down prefetches (throughput)
)

// batched object writer shared by both ends.
type outbox struct {
	g      *GCS
	prefix string
	mu     sync.Mutex
	buf    []byte
	dirty  time.Time
	seq    int
}

func (o *outbox) enq(stream uint32, typ byte, data []byte) {
	o.mu.Lock()
	if len(o.buf) == 0 {
		o.dirty = time.Now()
	}
	o.buf = appendFrame(o.buf, stream, typ, data)
	o.mu.Unlock()
}

func (o *outbox) run() {
	for {
		time.Sleep(3 * time.Millisecond)
		o.mu.Lock()
		if len(o.buf) == 0 || (len(o.buf) < muxChunk && time.Since(o.dirty) < muxFlush) {
			o.mu.Unlock()
			continue
		}
		buf, seq := o.buf, o.seq
		o.buf, o.seq = nil, o.seq+1
		o.mu.Unlock()
		name := fmt.Sprintf("%s/%d", o.prefix, seq)
		go func() {
			// Retry until success: a flat sequence cannot tolerate a lost object — if any
			// down/<seq> or up/<seq> were ever dropped, the reader (which polls that name
			// forever) would hang the whole tunnel. So every batch MUST eventually land.
			for {
				if o.g.Put(name, buf) == nil {
					return
				}
				time.Sleep(muxPoll)
			}
		}()
	}
}

// per-stream state on the exit; buffers DATA that arrives before the dial completes.
type rstream struct {
	mu      sync.Mutex
	conn    net.Conn
	pending [][]byte
	ready   bool
	closed  bool
}

func (s *rstream) data(b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if s.ready {
		s.conn.Write(b)
	} else {
		s.pending = append(s.pending, b)
	}
}
func (s *rstream) setReady(c net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		c.Close()
		return
	}
	s.conn, s.ready = c, true
	for _, p := range s.pending {
		c.Write(p)
	}
	s.pending = nil
}
func (s *rstream) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.conn != nil {
		s.conn.Close()
	}
}

type muxRelay struct {
	g     *GCS
	out   *outbox
	conns sync.Map // uint32 -> *rstream
}

// dialStream dials the target for an already-registered stream. The stream MUST be
// stored in m.conns synchronously by the caller before this runs, so that DATA frames
// arriving in the same batch as the OPEN are buffered into the stream (not dropped
// because the async dial hadn't registered it yet — that was the single-connection hang).
func (m *muxRelay) dialStream(stream uint32, s *rstream, target string) {
	conn, err := net.DialTimeout("tcp", target, 15*time.Second)
	if err != nil {
		m.conns.Delete(stream)
		m.out.enq(stream, fClose, nil)
		return
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	s.setReady(conn)
	buf := make([]byte, 256*1024)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			m.out.enq(stream, fData, append([]byte(nil), buf[:n]...))
		}
		if err != nil {
			break
		}
	}
	m.conns.Delete(stream)
	s.close()
	m.out.enq(stream, fClose, nil)
}

func (m *muxRelay) run() {
	go m.out.run()
	seq := 0
	for {
		name := fmt.Sprintf("up/%d", seq)
		var buf []byte
		for {
			b, st, err := m.g.Get(name)
			if err == nil && st == 200 {
				buf = b
				break
			}
			time.Sleep(muxPoll)
		}
		go m.g.Delete(name)
		parseFrames(buf, func(stream uint32, typ byte, data []byte) {
			switch typ {
			case fOpen:
				s := &rstream{}
				m.conns.Store(stream, s) // register BEFORE dialing so same-batch DATA buffers
				go m.dialStream(stream, s, string(append([]byte(nil), data...)))
			case fData:
				if v, ok := m.conns.Load(stream); ok {
					v.(*rstream).data(append([]byte(nil), data...))
				}
			case fClose:
				if v, ok := m.conns.LoadAndDelete(stream); ok {
					v.(*rstream).close()
				}
			}
		})
		seq++
	}
}

func runMuxRelay(args []string) {
	fs := flag.NewFlagSet("muxrelay", flag.ExitOnError)
	key := fs.String("key", "/root/gcs-key.json", "service account key")
	bucket := fs.String("bucket", "cyclevpn-xport-eu", "GCS bucket")
	flush := fs.Duration("flush", muxFlush, "batch coalesce window")
	fs.Parse(args)
	muxFlush = *flush
	kb, err := os.ReadFile(*key)
	die(err)
	g, err := NewGCS(kb, *bucket)
	die(err)
	m := &muxRelay{g: g, out: &outbox{g: g, prefix: "down"}}
	log.Printf("gcstun muxrelay: gs://%s -> internet (multiplexed)", *bucket)
	m.run()
}
