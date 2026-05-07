package pktline

import (
	"bytes"
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

func TestSidebandWriter_ExactSidebandPayloadMax(t *testing.T) {
	// Exactly SidebandPayloadMax bytes must fit in a single frame
	// (no chunking).
	var buf bytes.Buffer
	w := NewWriter(&buf)
	sb := NewSidebandWriter(w)
	p := bytes.Repeat([]byte("x"), SidebandPayloadMax)
	n, err := sb.WriteData(p)
	if err != nil {
		t.Fatalf("WriteData: %v", err)
	}
	if n != len(p) {
		t.Fatalf("WriteData: n=%d, want %d", n, len(p))
	}

	r := NewReader(&buf)
	tok, err := r.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if tok.Type != Data {
		t.Fatalf("Type: %v", tok.Type)
	}
	if len(tok.Payload) != SidebandPayloadMax+1 {
		t.Fatalf("frame payload size: got %d, want %d (band byte + %d data)",
			len(tok.Payload), SidebandPayloadMax+1, SidebandPayloadMax)
	}
	if tok.Payload[0] != BandData {
		t.Fatalf("band byte: %d, want %d", tok.Payload[0], BandData)
	}
	if !bytes.Equal(tok.Payload[1:], p) {
		t.Fatalf("data mismatch")
	}
	// Should be exactly one frame.
	if _, err := r.Read(); err == nil {
		t.Fatalf("expected EOF after one frame, got another frame")
	}
}

func TestSidebandWriter_OneOverSidebandPayloadMax(t *testing.T) {
	// SidebandPayloadMax + 1 bytes must split into exactly two frames:
	// the first carries SidebandPayloadMax bytes; the second carries 1 byte.
	var buf bytes.Buffer
	w := NewWriter(&buf)
	sb := NewSidebandWriter(w)
	p := bytes.Repeat([]byte("y"), SidebandPayloadMax+1)
	n, err := sb.WriteData(p)
	if err != nil {
		t.Fatalf("WriteData: %v", err)
	}
	if n != len(p) {
		t.Fatalf("WriteData: n=%d, want %d", n, len(p))
	}

	r := NewReader(&buf)
	tok, err := r.Read()
	if err != nil {
		t.Fatalf("Read 1: %v", err)
	}
	if len(tok.Payload) != SidebandPayloadMax+1 {
		t.Fatalf("frame 1 size: got %d, want %d", len(tok.Payload), SidebandPayloadMax+1)
	}
	tok, err = r.Read()
	if err != nil {
		t.Fatalf("Read 2: %v", err)
	}
	if len(tok.Payload) != 2 {
		t.Fatalf("frame 2 size: got %d, want 2 (band byte + 1 data)", len(tok.Payload))
	}
	if tok.Payload[0] != BandData || tok.Payload[1] != 'y' {
		t.Fatalf("frame 2 contents: %v", tok.Payload)
	}
	if _, err := r.Read(); err == nil {
		t.Fatalf("expected EOF after two frames")
	}
}
