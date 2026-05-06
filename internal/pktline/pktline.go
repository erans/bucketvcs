// Package pktline implements Git's pkt-line framing for the smart HTTP
// transport (and, in the future, SSH). Frames are length-prefixed with a
// 4-byte ASCII hex header that INCLUDES the 4 length bytes themselves.
//
// Special markers:
//
//	0000 = flush
//	0001 = delim   (protocol v2)
//	0002 = response-end (protocol v2)
//
// Reference: https://git-scm.com/docs/protocol-common
package pktline

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// MaxPayload is the largest allowed payload (excluding the 4-byte length
// header). Total wire frame size is therefore at most MaxPayload + 4 = 65520.
const MaxPayload = 65516

// TokenType classifies a pkt-line frame.
type TokenType int

const (
	Data TokenType = iota
	Flush
	Delim
	ResponseEnd
)

func (t TokenType) String() string {
	switch t {
	case Data:
		return "Data"
	case Flush:
		return "Flush"
	case Delim:
		return "Delim"
	case ResponseEnd:
		return "ResponseEnd"
	default:
		return fmt.Sprintf("TokenType(%d)", int(t))
	}
}

// Token is one decoded frame. For non-Data tokens, Payload is empty.
type Token struct {
	Type    TokenType
	Payload []byte
}

// Reader reads pkt-line frames from an underlying io.Reader.
type Reader struct {
	r   io.Reader
	hdr [4]byte
	buf []byte // payload buffer, grown as needed up to MaxPayload
}

func NewReader(r io.Reader) *Reader {
	return &Reader{r: r}
}

// Read returns the next pkt-line token. At true end of stream after a
// previously-returned Flush (or any clean boundary), Read returns io.EOF.
// On a truncated or malformed frame, an error is returned.
func (r *Reader) Read() (Token, error) {
	n, err := io.ReadFull(r.r, r.hdr[:])
	if err == io.EOF && n == 0 {
		return Token{}, io.EOF
	}
	if err != nil {
		return Token{}, fmt.Errorf("pktline: read length: %w", err)
	}
	var lenBytes [2]byte
	if _, err := hex.Decode(lenBytes[:], r.hdr[:]); err != nil {
		return Token{}, fmt.Errorf("pktline: malformed hex length %q: %w", string(r.hdr[:]), err)
	}
	frameLen := int(lenBytes[0])<<8 | int(lenBytes[1])
	switch frameLen {
	case 0:
		return Token{Type: Flush}, nil
	case 1:
		return Token{Type: Delim}, nil
	case 2:
		return Token{Type: ResponseEnd}, nil
	}
	if frameLen < 4 {
		return Token{}, fmt.Errorf("pktline: invalid frame length %d", frameLen)
	}
	if frameLen > MaxPayload+4 {
		return Token{}, fmt.Errorf("pktline: frame length %d exceeds max %d", frameLen, MaxPayload+4)
	}
	payloadLen := frameLen - 4
	if cap(r.buf) < payloadLen {
		r.buf = make([]byte, payloadLen)
	} else {
		r.buf = r.buf[:payloadLen]
	}
	if _, err := io.ReadFull(r.r, r.buf); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return Token{}, fmt.Errorf("pktline: read payload: %w", err)
	}
	return Token{Type: Data, Payload: r.buf}, nil
}

// Writer emits pkt-line frames to an underlying io.Writer.
type Writer struct {
	w   io.Writer
	hdr [4]byte
}

func NewWriter(w io.Writer) *Writer {
	return &Writer{w: w}
}

// WritePacket writes one Data frame whose payload is p. Returns an error if
// len(p) exceeds MaxPayload.
func (w *Writer) WritePacket(p []byte) error {
	if len(p) > MaxPayload {
		return fmt.Errorf("pktline: payload size %d exceeds max %d", len(p), MaxPayload)
	}
	frameLen := len(p) + 4
	w.hdr[0] = hexNibble(byte(frameLen >> 12))
	w.hdr[1] = hexNibble(byte(frameLen >> 8 & 0xf))
	w.hdr[2] = hexNibble(byte(frameLen >> 4 & 0xf))
	w.hdr[3] = hexNibble(byte(frameLen & 0xf))
	if _, err := w.w.Write(w.hdr[:]); err != nil {
		return fmt.Errorf("pktline: write header: %w", err)
	}
	if len(p) > 0 {
		if _, err := w.w.Write(p); err != nil {
			return fmt.Errorf("pktline: write payload: %w", err)
		}
	}
	return nil
}

// WriteFlush writes a flush-pkt (0000).
func (w *Writer) WriteFlush() error {
	_, err := w.w.Write([]byte("0000"))
	return err
}

// WriteDelim writes a delim-pkt (0001) used by protocol v2.
func (w *Writer) WriteDelim() error {
	_, err := w.w.Write([]byte("0001"))
	return err
}

// WriteResponseEnd writes a response-end-pkt (0002) used by protocol v2.
func (w *Writer) WriteResponseEnd() error {
	_, err := w.w.Write([]byte("0002"))
	return err
}

// WriteString is a convenience for WritePacket([]byte(s)).
func (w *Writer) WriteString(s string) error { return w.WritePacket([]byte(s)) }

func hexNibble(n byte) byte {
	if n < 10 {
		return '0' + n
	}
	return 'a' + (n - 10)
}
