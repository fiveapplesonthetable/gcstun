package main

import (
	"fmt"
	"os"
	"time"
)

func die(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func loadGCS(keyPath, bucket string) *GCS {
	kb, err := os.ReadFile(keyPath)
	die(err)
	g, err := NewGCS(kb, bucket)
	die(err)
	return g
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: gcstun <bench-write|bench-read|relay|client> ...")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "bench-write":
		// bench-write <key> <bucket> <mb>
		g := loadGCS(os.Args[2], os.Args[3])
		mb := 20
		fmt.Sscanf(os.Args[4], "%d", &mb)
		data := make([]byte, mb*1024*1024)
		t := time.Now()
		die(g.Put("bench/w.bin", data))
		d := time.Since(t).Seconds()
		fmt.Printf("write %dMB in %.2fs = %.1f MB/s\n", mb, d, float64(mb)/d)
	case "bench-read":
		// bench-read <key> <bucket> <obj>
		g := loadGCS(os.Args[2], os.Args[3])
		t := time.Now()
		b, st, err := g.Get(os.Args[4])
		die(err)
		d := time.Since(t).Seconds()
		fmt.Printf("read %d bytes (http %d) in %.2fs = %.1f MB/s\n", len(b), st, d, float64(len(b))/1e6/d)
	case "relay":
		runRelay(os.Args[2:])
	case "client":
		runClient(os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, "unknown:", os.Args[1])
		os.Exit(2)
	}
}
