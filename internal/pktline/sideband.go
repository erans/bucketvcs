package pktline

import "fmt"

// Side-band channel ids (Git side-band-64k).
const (
	BandData     byte = 1
	BandProgress byte = 2
	BandFatal    byte = 3
)

// SidebandPayloadMax is the largest application-level payload writable in a
// single side-band frame: one byte for the band id, the rest for data.
const SidebandPayloadMax = MaxPayload - 1

// SidebandWriter wraps a *Writer to emit side-band-multiplexed frames.
// Calls to WriteData/WriteProgress/WriteFatal automatically chunk payloads
// larger than SidebandPayloadMax across multiple frames.
type SidebandWriter struct {
	w   *Writer
	buf []byte
}

func NewSidebandWriter(w *Writer) *SidebandWriter {
	return &SidebandWriter{w: w, buf: make([]byte, MaxPayload)}
}

// WriteData writes p on band 1 (BandData), chunking as needed. Returns the
// number of payload bytes written or an error from the underlying writer.
func (s *SidebandWriter) WriteData(p []byte) (int, error) { return s.write(BandData, p) }

// WriteProgress writes p on band 2 (BandProgress).
func (s *SidebandWriter) WriteProgress(p []byte) (int, error) { return s.write(BandProgress, p) }

// WriteFatal writes p on band 3 (BandFatal).
func (s *SidebandWriter) WriteFatal(p []byte) (int, error) { return s.write(BandFatal, p) }

func (s *SidebandWriter) write(band byte, p []byte) (int, error) {
	if band < 1 || band > 3 {
		return 0, fmt.Errorf("pktline: invalid sideband %d", band)
	}
	written := 0
	for len(p) > 0 {
		chunk := p
		if len(chunk) > SidebandPayloadMax {
			chunk = chunk[:SidebandPayloadMax]
		}
		s.buf = s.buf[:0]
		s.buf = append(s.buf, band)
		s.buf = append(s.buf, chunk...)
		if err := s.w.WritePacket(s.buf); err != nil {
			return written, err
		}
		written += len(chunk)
		p = p[len(chunk):]
	}
	return written, nil
}
