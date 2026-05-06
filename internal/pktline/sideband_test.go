package pktline

import (
	"bytes"
	"strings"
	"testing"
)

func TestSidebandWriter_BandsAndChunking(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	sb := NewSidebandWriter(w)

	if _, err := sb.WriteData([]byte("hello")); err != nil {
		t.Fatalf("WriteData: %v", err)
	}
	if _, err := sb.WriteProgress([]byte("counting...\n")); err != nil {
		t.Fatalf("WriteProgress: %v", err)
	}
	if _, err := sb.WriteFatal([]byte("boom")); err != nil {
		t.Fatalf("WriteFatal: %v", err)
	}

	r := NewReader(&buf)
	tok, _ := r.Read()
	if tok.Type != Data || tok.Payload[0] != BandData || string(tok.Payload[1:]) != "hello" {
		t.Fatalf("frame 1: %+v", tok)
	}
	tok, _ = r.Read()
	if tok.Payload[0] != BandProgress || string(tok.Payload[1:]) != "counting...\n" {
		t.Fatalf("frame 2: %+v", tok)
	}
	tok, _ = r.Read()
	if tok.Payload[0] != BandFatal || string(tok.Payload[1:]) != "boom" {
		t.Fatalf("frame 3: %+v", tok)
	}
}

func TestSidebandWriter_LargePayloadChunks(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	sb := NewSidebandWriter(w)
	big := bytes.Repeat([]byte("x"), MaxPayload*2+100)
	n, err := sb.WriteData(big)
	if err != nil {
		t.Fatalf("WriteData: %v", err)
	}
	if n != len(big) {
		t.Fatalf("WriteData: n=%d, want %d", n, len(big))
	}

	r := NewReader(&buf)
	var got []byte
	for {
		tok, err := r.Read()
		if err != nil {
			break
		}
		if tok.Type != Data || tok.Payload[0] != BandData {
			t.Fatalf("unexpected frame: %+v", tok)
		}
		got = append(got, tok.Payload[1:]...)
	}
	if !bytes.Equal(got, big) {
		t.Fatalf("reassembled payload mismatch: len=%d want=%d", len(got), len(big))
	}
}

func TestSidebandWriter_RejectsBadBand(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	sb := NewSidebandWriter(w)
	if _, err := sb.write(0, []byte("x")); err == nil {
		t.Fatalf("write band 0: expected error")
	}
	if _, err := sb.write(4, []byte("x")); err == nil {
		t.Fatalf("write band 4: expected error")
	}
}

// Compile-time interface check.
var _ = strings.NewReader
