package main

// Multiplexed transport: instead of one GCS object-stream per SOCKS connection (each
// paying the ~1.2s GCS round-trip on its own), ALL connections share a single pair of
// object streams (up/<seq>, down/<seq>). Each object is a batch of frames, one per
// active stream. So the per-round-trip GCS latency is amortised across every connection
// at once — ten TLS handshakes happen in one batch, not ten serial round-trips.
//
// Frame:  streamID(4) | type(1) | streamSeq(4) | len(4) | data
//   type 1 = OPEN  (data = "host:port")
//   type 2 = DATA  (raw bytes for that stream)
//   type 3 = CLOSE (stream ended)
//
// streamSeq is a PER-STREAM counter (0,1,2…) on that stream's DATA frames. The receiver
// checks it: if a stream's DATA ever arrives with an unexpected streamSeq (a gap or a
// duplicate — i.e. the transport lost or reordered a batch), the receiver closes THAT
// stream instead of delivering corrupted bytes to the destination. So a transport glitch
// becomes a single dropped connection (which the app retries), never a "bad request" from
// a truncated request reaching the server.

import "encoding/binary"

const (
	fOpen  = 1
	fData  = 2
	fClose = 3
)

func appendFrame(b []byte, stream uint32, typ byte, sseq uint32, data []byte) []byte {
	var h [13]byte
	binary.BigEndian.PutUint32(h[0:4], stream)
	h[4] = typ
	binary.BigEndian.PutUint32(h[5:9], sseq)
	binary.BigEndian.PutUint32(h[9:13], uint32(len(data)))
	b = append(b, h[:]...)
	return append(b, data...)
}

func parseFrames(b []byte, fn func(stream uint32, typ byte, sseq uint32, data []byte)) {
	i := 0
	for i+13 <= len(b) {
		stream := binary.BigEndian.Uint32(b[i : i+4])
		typ := b[i+4]
		sseq := binary.BigEndian.Uint32(b[i+5 : i+9])
		n := int(binary.BigEndian.Uint32(b[i+9 : i+13]))
		i += 13
		if i+n > len(b) {
			return
		}
		fn(stream, typ, sseq, b[i:i+n])
		i += n
	}
}
