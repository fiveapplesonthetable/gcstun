package main

// Entry side (runs on the RU box). A SOCKS5 server. For each connection it opens a
// session by writing req/<sid> to GCS, pushes the app's bytes as up/<sid>/<seq>
// objects, and pulls the response as down/<sid>/<seq> objects — all over the
// whitelisted storage.googleapis.com, so TSPU never sees a denylisted destination.

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"time"
)

var (
	pollInterval = 60 * time.Millisecond
	upChunk      = 256 * 1024 // app->target: modest (requests are usually small)
	downWindow   = 12         // concurrent downstream prefetches (pipelining for throughput)
	cdebug       bool
)

type session struct {
	g    *GCS
	sid  string
	conn net.Conn
}

func randID() string {
	var b [9]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// waitGet polls a sequential object until it exists, returns its bytes. Gives up when
// stop() is true (connection closed).
func (g *GCS) waitGet(obj string, stop func() bool) ([]byte, bool) {
	for !stop() {
		b, st, err := g.Get(obj)
		if err == nil && st == 200 {
			return b, true
		}
		time.Sleep(pollInterval)
	}
	return nil, false
}

func (s *session) upPump(dead func() bool) {
	buf := make([]byte, upChunk)
	seq := 0
	for {
		n, err := s.conn.Read(buf)
		last := err != nil
		if n > 0 || last {
			body := make([]byte, n+1)
			if last {
				body[0] = 1
			}
			copy(body[1:], buf[:n])
			for tries := 0; tries < 20 && !dead(); tries++ {
				if e := s.g.Put(fmt.Sprintf("up/%s/%d", s.sid, seq), body); e == nil {
					break
				}
				time.Sleep(pollInterval)
			}
			seq++
		}
		if last {
			return
		}
	}
}

// downPump reads down/<sid>/<seq> objects and writes them to the app IN ORDER, but
// prefetches a window of chunks CONCURRENTLY so throughput approaches the raw ~24 MB/s
// GCS read speed instead of being capped by one round-trip-at-a-time.
func (s *session) downPump(dead func() bool) {
	type res struct {
		data []byte
		last bool
		ok   bool
	}
	fetch := func(seq int) res {
		obj := fmt.Sprintf("down/%s/%d", s.sid, seq)
		b, ok := s.g.waitGet(obj, dead)
		if !ok || len(b) < 1 {
			return res{ok: false}
		}
		go s.g.Delete(obj)
		return res{data: b[1:], last: b[0]&1 == 1, ok: true}
	}
	inflight := map[int]chan res{}
	start := func(seq int) {
		ch := make(chan res, 1)
		inflight[seq] = ch
		go func() { ch <- fetch(seq) }()
	}
	for i := 0; i < downWindow; i++ {
		start(i)
	}
	next := 0
	for !dead() {
		ch := inflight[next]
		r := <-ch
		delete(inflight, next)
		if !r.ok {
			return
		}
		if len(r.data) > 0 {
			if _, err := s.conn.Write(r.data); err != nil {
				return
			}
		}
		if r.last {
			return
		}
		start(next + downWindow) // keep the window full
		next++
	}
}

func handleSocks(c net.Conn, g *GCS) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(30 * time.Second))
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(c, hdr); err != nil || hdr[0] != 5 {
		return
	}
	io.ReadFull(c, make([]byte, hdr[1]))
	c.Write([]byte{5, 0})
	req := make([]byte, 4)
	if _, err := io.ReadFull(c, req); err != nil || req[1] != 1 { // CONNECT only
		c.Write([]byte{5, 7, 0, 1, 0, 0, 0, 0, 0, 0})
		return
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
		return
	}
	pb := make([]byte, 2)
	io.ReadFull(c, pb)
	port := int(binary.BigEndian.Uint16(pb))
	c.SetDeadline(time.Time{})

	sid := randID()
	if err := g.Put(fmt.Sprintf("req/%s", sid), []byte(fmt.Sprintf("%s:%d", host, port))); err != nil {
		if cdebug {
			log.Printf("open %s:%d: %v", host, port, err)
		}
		c.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})

	done := make(chan struct{})
	var closed bool
	dead := func() bool { return closed }
	s := &session{g: g, sid: sid, conn: c}
	go func() { s.upPump(dead); close(done) }()
	s.downPump(dead)
	closed = true
	<-doneOrTimeout(done)
	g.Put(fmt.Sprintf("close/%s", sid), []byte("1"))
}

func doneOrTimeout(done chan struct{}) chan struct{} {
	out := make(chan struct{})
	go func() {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		close(out)
	}()
	return out
}

func runClient(args []string) {
	fs := flag.NewFlagSet("client", flag.ExitOnError)
	key := fs.String("key", "/root/gcs-key.json", "service account key")
	bucket := fs.String("bucket", "cyclevpn-xport-eu", "GCS bucket")
	listen := fs.String("listen", "127.0.0.1:10900", "SOCKS5 listen")
	poll := fs.Duration("poll", pollInterval, "GCS poll interval")
	win := fs.Int("window", downWindow, "concurrent downstream prefetches")
	dbg := fs.Bool("debug", false, "debug logs")
	fs.Parse(args)
	pollInterval, downWindow, cdebug = *poll, *win, *dbg
	kb, err := os.ReadFile(*key)
	die(err)
	g, err := NewGCS(kb, *bucket)
	die(err)
	ln, err := net.Listen("tcp", *listen)
	die(err)
	log.Printf("gcstun client: SOCKS5 %s <-> gs://%s", *listen, *bucket)
	for {
		c, err := ln.Accept()
		if err != nil {
			continue
		}
		go handleSocks(c, g)
	}
}
