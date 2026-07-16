package main

// Exit side (runs on the Contabo box). Discovers new sessions by listing req/ in GCS,
// opens the real TCP connection to the destination, streams the response back as
// down/<sid>/<seq> objects, and applies up/<sid>/<seq> objects to the destination.
// It only ever talks to GCS and the open internet — never to Russia directly.

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	downChunk   = 2 * 1024 * 1024       // accumulate up to this before writing (few GCS ops)
	flushAfter  = 100 * time.Millisecond // ...but flush sooner so small responses are prompt
	writeWindow = 12                     // concurrent downstream uploads (don't stall the target read)
	rpoll       = 60 * time.Millisecond
	rdebug      bool

	producedBytes int64
	readNs        int64 // time the read loop spends in conn.Read (waiting for the destination)
	flushNs       int64 // time it spends in flush (blocked on the upload semaphore)
)

func rateLog() {
	var last, lr, lf int64
	for {
		time.Sleep(2 * time.Second)
		cur := atomic.LoadInt64(&producedBytes)
		r := atomic.LoadInt64(&readNs)
		f := atomic.LoadInt64(&flushNs)
		if rdebug && cur != last {
			log.Printf("relay produced %.1f MB/s | read-wait %.0fms/s flush-wait %.0fms/s",
				float64(cur-last)/2e6, float64(r-lr)/2e6, float64(f-lf)/2e6)
		}
		last, lr, lf = cur, r, f
	}
}

type rsession struct {
	g      *GCS
	sid    string
	conn   net.Conn
	closed bool
	mu     sync.Mutex
}

func (s *rsession) dead() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.closed }
func (s *rsession) kill()      { s.mu.Lock(); s.closed = true; s.mu.Unlock(); s.conn.Close() }

// downLoop: target -> down/<sid>/<seq> objects. Accumulates up to downChunk, but flushes
// within flushAfter of the first buffered byte so small/interactive replies aren't delayed.
func (s *rsession) downLoop() {
	seq := 0
	acc := make([]byte, 0, downChunk)
	tmp := make([]byte, 256*1024)
	var first time.Time
	sem := make(chan struct{}, writeWindow)
	var wg sync.WaitGroup
	flush := func(last bool) {
		body := make([]byte, len(acc)+1)
		if last {
			body[0] = 1
		}
		copy(body[1:], acc)
		n := seq
		seq++
		atomic.AddInt64(&producedBytes, int64(len(acc)))
		acc = acc[:0]
		first = time.Time{}
		// upload concurrently (up to writeWindow) so reading the target never stalls on a write
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			for tries := 0; tries < 20 && !s.dead(); tries++ {
				if e := s.g.Put(fmt.Sprintf("down/%s/%d", s.sid, n), body); e == nil {
					return
				}
				time.Sleep(rpoll)
			}
		}()
	}
	defer wg.Wait()
	for !s.dead() {
		s.conn.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
		t0 := time.Now()
		n, err := s.conn.Read(tmp)
		atomic.AddInt64(&readNs, int64(time.Since(t0)))
		if n > 0 {
			if first.IsZero() {
				first = time.Now()
			}
			acc = append(acc, tmp[:n]...)
		}
		eof := err != nil && !isTimeout(err)
		if len(acc) >= downChunk || (len(acc) > 0 && !first.IsZero() && time.Since(first) >= flushAfter) || eof {
			t1 := time.Now()
			flush(eof)
			atomic.AddInt64(&flushNs, int64(time.Since(t1)))
		}
		if eof {
			return
		}
	}
}

// upLoop: up/<sid>/<seq> objects -> target. Half-closes the target's write side on the
// last chunk (flag bit 0), preserving proper TCP half-close.
func (s *rsession) upLoop() {
	seq := 0
	for !s.dead() {
		obj := fmt.Sprintf("up/%s/%d", s.sid, seq)
		b, ok := s.g.waitGet(obj, s.dead)
		if !ok {
			return
		}
		go s.g.Delete(obj)
		if len(b) < 1 {
			return
		}
		if len(b) > 1 {
			if _, err := s.conn.Write(b[1:]); err != nil {
				return
			}
		}
		if b[0]&1 == 1 {
			if tc, ok := s.conn.(*net.TCPConn); ok {
				tc.CloseWrite()
			}
			return
		}
		seq++
	}
}

func (r *relay) handle(sid, target string) {
	conn, err := net.DialTimeout("tcp", target, 15*time.Second)
	if err != nil {
		if rdebug {
			log.Printf("dial %s: %v", target, err)
		}
		// signal failure: a last empty down chunk so the entry's SOCKS reply can fail fast
		r.g.Put(fmt.Sprintf("down/%s/0", sid), []byte{1})
		return
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
		// NOTE: do NOT SetReadBuffer here — manually setting SO_RCVBUF disables Linux
		// receive-window autotuning and clamps to net.core.rmem_max (~208KB), which
		// caps target->relay throughput to rmem_max/RTT (~5 MB/s). Autotuning grows the
		// window far larger on its own.
	}
	s := &rsession{g: r.g, sid: sid, conn: conn}
	go s.upLoop()
	go func() {
		// watch for the entry's close signal
		for !s.dead() {
			if _, st, _ := r.g.Get("close/" + sid); st == 200 {
				r.g.Delete("close/" + sid)
				s.kill()
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
	}()
	s.downLoop()
	s.kill()
}

type relay struct {
	g    *GCS
	seen map[string]bool
	mu   sync.Mutex
}

func (r *relay) loop() {
	for {
		names, err := r.g.List("req/", 200)
		if err != nil {
			time.Sleep(rpoll)
			continue
		}
		for _, name := range names {
			sid := strings.TrimPrefix(name, "req/")
			if sid == "" || strings.Contains(sid, "/") {
				continue
			}
			r.mu.Lock()
			if r.seen[sid] {
				r.mu.Unlock()
				continue
			}
			r.seen[sid] = true
			r.mu.Unlock()
			body, st, err := r.g.Get(name)
			if err != nil || st != 200 {
				continue
			}
			r.g.Delete(name)
			target := string(body)
			if rdebug {
				log.Printf("session %s -> %s", sid, target)
			}
			go r.handle(sid, target)
		}
		time.Sleep(rpoll)
	}
}

func isTimeout(err error) bool {
	ne, ok := err.(net.Error)
	return ok && ne.Timeout()
}

func runRelay(args []string) {
	fs := flag.NewFlagSet("relay", flag.ExitOnError)
	key := fs.String("key", "/root/gcs-key.json", "service account key")
	bucket := fs.String("bucket", "cyclevpn-xport-eu", "GCS bucket")
	poll := fs.Duration("poll", rpoll, "GCS poll interval")
	chunk := fs.Int("chunk", downChunk, "max downstream chunk bytes")
	win := fs.Int("window", writeWindow, "concurrent downstream uploads")
	dbg := fs.Bool("debug", false, "debug logs")
	fs.Parse(args)
	rpoll, downChunk, writeWindow, rdebug = *poll, *chunk, *win, *dbg
	kb, err := os.ReadFile(*key)
	die(err)
	g, err := NewGCS(kb, *bucket)
	die(err)
	r := &relay{g: g, seen: map[string]bool{}}
	go rateLog()
	log.Printf("gcstun relay: gs://%s -> internet", *bucket)
	r.loop()
}

var _ = io.EOF
