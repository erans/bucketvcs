package v2proto

import (
	"bytes"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/pktline"
)

func TestWriteV2Advertisement_ContainsExpectedLines(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteV2Advertisement(&buf, "git-upload-pack", "0.1"); err != nil {
		t.Fatalf("WriteV2Advertisement: %v", err)
	}
	tokens := drainTokens(t, &buf)

	wantLines := []string{
		"# service=git-upload-pack\n",
		"", // flush
		"version 2\n",
		"agent=bucketvcs/0.1\n",
		"ls-refs=unborn\n",
		"fetch\n",
		"object-format=sha1\n",
		"", // flush
	}
	if len(tokens) != len(wantLines) {
		t.Fatalf("token count: got %d, want %d (%v)", len(tokens), len(wantLines), tokens)
	}
	for i, want := range wantLines {
		if want == "" {
			if tokens[i].Type != pktline.Flush {
				t.Errorf("token %d: type %v, want Flush", i, tokens[i].Type)
			}
			continue
		}
		if tokens[i].Type != pktline.Data {
			t.Errorf("token %d: type %v, want Data", i, tokens[i].Type)
		}
		if string(tokens[i].Payload) != want {
			t.Errorf("token %d payload: got %q, want %q", i, tokens[i].Payload, want)
		}
	}
}

func TestWriteV2Advertisement_AgentPrefixGuard(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteV2Advertisement(&buf, "git-upload-pack", "0.1\nls-refs=evil"); err == nil {
		t.Fatalf("WriteV2Advertisement: expected error on agent containing newline")
	}
}

func drainTokens(t *testing.T, r *bytes.Buffer) []pktline.Token {
	t.Helper()
	pr := pktline.NewReader(r)
	var out []pktline.Token
	for {
		tok, err := pr.Read()
		if err != nil {
			break
		}
		// Copy payload so the buffer reuse in pktline doesn't bite us.
		cp := append([]byte{}, tok.Payload...)
		out = append(out, pktline.Token{Type: tok.Type, Payload: cp})
	}
	return out
}

func TestWriteV2Advertisement_RejectsServiceWithSpace(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteV2Advertisement(&buf, "git upload-pack", "0.1"); err == nil {
		t.Fatalf("WriteV2Advertisement: expected error on service with space")
	}
	if buf.Len() != 0 {
		t.Fatalf("partial bytes written on rejection: %d", buf.Len())
	}
}

func TestWriteV2Advertisement_RejectsServiceWithControlChars(t *testing.T) {
	var buf bytes.Buffer
	for _, bad := range []string{"git\nupload-pack", "git\rupload-pack", "git\x00upload-pack"} {
		buf.Reset()
		if err := WriteV2Advertisement(&buf, bad, "0.1"); err == nil {
			t.Fatalf("WriteV2Advertisement: expected error on service %q", bad)
		}
		if buf.Len() != 0 {
			t.Fatalf("partial bytes written on rejection of %q: %d", bad, buf.Len())
		}
	}
}
