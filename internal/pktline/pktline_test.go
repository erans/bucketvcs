package pktline

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReader_BasicFrame(t *testing.T) {
	r := NewReader(strings.NewReader("0009abcd\n"))
	tok, err := r.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if tok.Type != Data {
		t.Fatalf("Type: got %v, want Data", tok.Type)
	}
	if string(tok.Payload) != "abcd\n" {
		t.Fatalf("Payload: got %q, want %q", tok.Payload, "abcd\n")
	}
}

func TestReader_FlushDelimResponseEnd(t *testing.T) {
	r := NewReader(strings.NewReader("000000010002"))
	for _, want := range []TokenType{Flush, Delim, ResponseEnd} {
		tok, err := r.Read()
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if tok.Type != want {
			t.Fatalf("Type: got %v, want %v", tok.Type, want)
		}
		if len(tok.Payload) != 0 {
			t.Fatalf("Payload non-empty for marker: %q", tok.Payload)
		}
	}
}

func TestReader_EOFAfterAllFramesConsumed(t *testing.T) {
	r := NewReader(strings.NewReader("0000"))
	if _, err := r.Read(); err != nil {
		t.Fatalf("Read flush: %v", err)
	}
	if _, err := r.Read(); !errors.Is(err, io.EOF) {
		t.Fatalf("Read after end: got %v, want io.EOF", err)
	}
}

func TestReader_MalformedHexLength(t *testing.T) {
	r := NewReader(strings.NewReader("zzzzhello"))
	if _, err := r.Read(); err == nil {
		t.Fatalf("Read: expected error on malformed hex length")
	}
}

func TestReader_OversizedRejected(t *testing.T) {
	r := NewReader(strings.NewReader("ffff"))
	if _, err := r.Read(); err == nil {
		t.Fatalf("Read: expected error on oversized frame")
	}
}

func TestReader_LengthTooSmall(t *testing.T) {
	r := NewReader(strings.NewReader("0003"))
	if _, err := r.Read(); err == nil {
		t.Fatalf("Read: expected error on length < 4 (and not 0/1/2)")
	}
}

func TestReader_UnexpectedEOFInPayload(t *testing.T) {
	r := NewReader(strings.NewReader("0009ab"))
	_, err := r.Read()
	if err == nil {
		t.Fatalf("Read: expected error on truncated payload")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("Read: got %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestWriter_RoundTrip(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.WritePacket([]byte("hello\n")); err != nil {
		t.Fatalf("WritePacket: %v", err)
	}
	if err := w.WriteDelim(); err != nil {
		t.Fatalf("WriteDelim: %v", err)
	}
	if err := w.WriteFlush(); err != nil {
		t.Fatalf("WriteFlush: %v", err)
	}

	r := NewReader(&buf)
	tok, err := r.Read()
	if err != nil || tok.Type != Data || string(tok.Payload) != "hello\n" {
		t.Fatalf("Read 1: %+v err=%v", tok, err)
	}
	tok, err = r.Read()
	if err != nil || tok.Type != Delim {
		t.Fatalf("Read 2 (delim): %+v err=%v", tok, err)
	}
	tok, err = r.Read()
	if err != nil || tok.Type != Flush {
		t.Fatalf("Read 3 (flush): %+v err=%v", tok, err)
	}
}

func TestWriter_RejectsOversizedPayload(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	big := bytes.Repeat([]byte{'x'}, MaxPayload+1)
	if err := w.WritePacket(big); err == nil {
		t.Fatalf("WritePacket: expected error on payload > MaxPayload")
	}
}
