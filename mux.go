package main

// Multiplexed transport: instead of one GCS object-stream per SOCKS connection (each
// paying the ~1.2s GCS round-trip on its own), ALL connections share a single pair of
// object streams (up/<seq>, down/<seq>). Each object is a batch of frames, one per
// active stream. So the per-round-trip GCS latency is amortised across every connection
// at once — ten TLS handshakes happen in one batch, not ten serial round-trips.
//
// Frame:  streamID(4) | type(1) | len(4) | data
//   type 1 = OPEN  (data = "host:port")
//   type 2 = DATA  (raw bytes for that stream)
//   type 3 = CLOSE (stream ended)

import "encoding/binary"

const (
	fOpen  = 1
	fData  = 2
	fClose = 3
)

func appendFrame(b []byte, stream uint32, typ byte, data []byte) []byte {
	var h [9]byte
	binary.BigEndian.PutUint32(h[0:4], stream)
	h[4] = typ
	binary.BigEndian.PutUint32(h[5:9], uint32(len(data)))
	b = append(b, h[:]...)
	return append(b, data...)
}

func parseFrames(b []byte, fn func(stream uint32, typ byte, data []byte)) {
	i := 0
	for i+9 <= len(b) {
		stream := binary.BigEndian.Uint32(b[i : i+4])
		typ := b[i+4]
		n := int(binary.BigEndian.Uint32(b[i+5 : i+9]))
		i += 9
		if i+n > len(b) {
			return
		}
		fn(stream, typ, b[i:i+n])
		i += n
	}
}
