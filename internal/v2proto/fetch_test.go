package v2proto

import (
	"bytes"
	"reflect"
	"testing"
)

func TestParseFetchArgs_HappyPath(t *testing.T) {
	args := tokensFromLines(
		"command=fetch\n",
		"DELIM",
		"thin-pack\n",
		"no-progress\n",
		"include-tag\n",
		"ofs-delta\n",
		"want 1111111111111111111111111111111111111111\n",
		"want 2222222222222222222222222222222222222222\n",
		"have 3333333333333333333333333333333333333333\n",
		"done\n",
		"FLUSH",
	)
	got, err := ParseFetchArgs(args)
	if err != nil {
		t.Fatalf("ParseFetchArgs: %v", err)
	}
	want := FetchRequest{
		Wants: []string{
			"1111111111111111111111111111111111111111",
			"2222222222222222222222222222222222222222",
		},
		Haves:      []string{"3333333333333333333333333333333333333333"},
		Done:       true,
		ThinPack:   true,
		NoProgress: true,
		IncludeTag: true,
		OfsDelta:   true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseFetchArgs:\n got %+v\nwant %+v", got, want)
	}
}

func TestParseFetchArgs_Shallow(t *testing.T) {
	args := tokensFromLines(
		"command=fetch\n",
		"DELIM",
		"want 1111111111111111111111111111111111111111\n",
		"deepen 3\n",
		"shallow 4444444444444444444444444444444444444444\n",
		"FLUSH",
	)
	got, err := ParseFetchArgs(args)
	if err != nil {
		t.Fatalf("ParseFetchArgs: %v", err)
	}
	if got.Depth != 3 {
		t.Fatalf("Depth: got %d, want 3", got.Depth)
	}
	wantShallow := []string{"4444444444444444444444444444444444444444"}
	if !reflect.DeepEqual(got.Shallow, wantShallow) {
		t.Fatalf("Shallow: got %v, want %v", got.Shallow, wantShallow)
	}
}

func TestParseFetchArgs_RejectsUnknown(t *testing.T) {
	args := tokensFromLines(
		"command=fetch\n",
		"DELIM",
		"want 1111111111111111111111111111111111111111\n",
		"weird-arg\n",
		"FLUSH",
	)
	if _, err := ParseFetchArgs(args); err == nil {
		t.Fatalf("ParseFetchArgs: expected error on unknown arg")
	}
}

func TestParseFetchArgs_RejectsFilter(t *testing.T) {
	args := tokensFromLines(
		"command=fetch\n",
		"DELIM",
		"want 1111111111111111111111111111111111111111\n",
		"filter blob:none\n",
		"FLUSH",
	)
	if _, err := ParseFetchArgs(args); err == nil {
		t.Fatalf("ParseFetchArgs: expected error on filter (not advertised)")
	}
}

func TestParseFetchArgs_RejectsBadOID(t *testing.T) {
	args := tokensFromLines(
		"command=fetch\n",
		"DELIM",
		"want notahex\n",
		"FLUSH",
	)
	if _, err := ParseFetchArgs(args); err == nil {
		t.Fatalf("ParseFetchArgs: expected error on bad OID")
	}
}

func TestParseFetchArgs_RequiresWant(t *testing.T) {
	args := tokensFromLines(
		"command=fetch\n",
		"DELIM",
		"done\n",
		"FLUSH",
	)
	if _, err := ParseFetchArgs(args); err == nil {
		t.Fatalf("ParseFetchArgs: expected error when no want present")
	}
}

func TestWriteAcknowledgments_AllUnknown(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteAcknowledgments(&buf, nil, []string{"3333333333333333333333333333333333333333"}, true); err != nil {
		t.Fatalf("WriteAcknowledgments: %v", err)
	}
	got := drainPayloads(t, &buf)
	want := []string{"acknowledgments\n", "NAK\n"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ack stream: got %v, want %v", got, want)
	}
}

func TestWriteAcknowledgments_SomeCommonReady(t *testing.T) {
	var buf bytes.Buffer
	commons := []string{"3333333333333333333333333333333333333333"}
	if err := WriteAcknowledgments(&buf, commons, nil, true); err != nil {
		t.Fatalf("WriteAcknowledgments: %v", err)
	}
	got := drainPayloads(t, &buf)
	want := []string{
		"acknowledgments\n",
		"ACK 3333333333333333333333333333333333333333\n",
		"ready\n",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ack stream: got %v, want %v", got, want)
	}
}

func TestParseFetchArgs_DeepenSince(t *testing.T) {
	args := tokensFromLines(
		"command=fetch\n",
		"DELIM",
		"want 1111111111111111111111111111111111111111\n",
		"deepen-since 1700000000\n",
		"FLUSH",
	)
	got, err := ParseFetchArgs(args)
	if err != nil {
		t.Fatalf("ParseFetchArgs: %v", err)
	}
	if got.DeepenSince != "1700000000" {
		t.Fatalf("DeepenSince: %q", got.DeepenSince)
	}
}

func TestParseFetchArgs_DeepenNotMultiple(t *testing.T) {
	args := tokensFromLines(
		"command=fetch\n",
		"DELIM",
		"want 1111111111111111111111111111111111111111\n",
		"deepen-not refs/heads/main\n",
		"deepen-not refs/tags/v1\n",
		"FLUSH",
	)
	got, err := ParseFetchArgs(args)
	if err != nil {
		t.Fatalf("ParseFetchArgs: %v", err)
	}
	wantNot := []string{"refs/heads/main", "refs/tags/v1"}
	if !reflect.DeepEqual(got.DeepenNot, wantNot) {
		t.Fatalf("DeepenNot: got %v, want %v", got.DeepenNot, wantNot)
	}
}

func TestParseFetchArgs_RejectsConflictingShallow(t *testing.T) {
	cases := map[string][]string{
		"depth+since": {
			"deepen 3\n",
			"deepen-since 1700000000\n",
		},
		"depth+not": {
			"deepen 3\n",
			"deepen-not refs/heads/main\n",
		},
		"since+not": {
			"deepen-since 1700000000\n",
			"deepen-not refs/heads/main\n",
		},
		"not+since": {
			"deepen-not refs/heads/main\n",
			"deepen-since 1700000000\n",
		},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			lines := []string{
				"command=fetch\n",
				"DELIM",
				"want 1111111111111111111111111111111111111111\n",
			}
			lines = append(lines, extra...)
			lines = append(lines, "FLUSH")
			args := tokensFromLines(lines...)
			if _, err := ParseFetchArgs(args); err == nil {
				t.Fatalf("ParseFetchArgs: expected conflict error for %s", name)
			}
		})
	}
}

func TestParseFetchArgs_WantRefRejected(t *testing.T) {
	// "want-ref" requires the ref-in-want capability, which the M3
	// advertisement does not expose. The parser must reject it.
	args := tokensFromLines(
		"command=fetch\n",
		"DELIM",
		"want-ref refs/heads/main\n",
		"FLUSH",
	)
	if _, err := ParseFetchArgs(args); err == nil {
		t.Fatalf("ParseFetchArgs: expected error on unadvertised want-ref")
	}
}

func TestParseFetchArgs_RejectsBadDeepenSince(t *testing.T) {
	cases := map[string]string{
		"empty":       "deepen-since \n",
		"non-numeric": "deepen-since notanumber\n",
		"zero":        "deepen-since 0\n",
		"negative":    "deepen-since -5\n",
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			args := tokensFromLines(
				"command=fetch\n",
				"DELIM",
				"want 1111111111111111111111111111111111111111\n",
				line,
				"FLUSH",
			)
			if _, err := ParseFetchArgs(args); err == nil {
				t.Fatalf("ParseFetchArgs: expected error on bad deepen-since %q", line)
			}
		})
	}
}

func TestParseFetchArgs_RejectsBadDeepenNot(t *testing.T) {
	args := tokensFromLines(
		"command=fetch\n",
		"DELIM",
		"want 1111111111111111111111111111111111111111\n",
		"deepen-not \n",
		"FLUSH",
	)
	if _, err := ParseFetchArgs(args); err == nil {
		t.Fatalf("ParseFetchArgs: expected error on empty deepen-not")
	}
}

func TestParseFetchArgs_HaveWithoutWantRejected(t *testing.T) {
	args := tokensFromLines(
		"command=fetch\n",
		"DELIM",
		"have 3333333333333333333333333333333333333333\n",
		"FLUSH",
	)
	if _, err := ParseFetchArgs(args); err == nil {
		t.Fatalf("ParseFetchArgs: expected error when only have is present")
	}
}

func TestWriteAcknowledgments_MultipleCommonsPreservesOrder(t *testing.T) {
	var buf bytes.Buffer
	commons := []string{
		"3333333333333333333333333333333333333333",
		"4444444444444444444444444444444444444444",
		"5555555555555555555555555555555555555555",
	}
	if err := WriteAcknowledgments(&buf, commons, nil, true); err != nil {
		t.Fatalf("WriteAcknowledgments: %v", err)
	}
	got := drainPayloads(t, &buf)
	want := []string{
		"acknowledgments\n",
		"ACK 3333333333333333333333333333333333333333\n",
		"ACK 4444444444444444444444444444444444444444\n",
		"ACK 5555555555555555555555555555555555555555\n",
		"ready\n",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ack stream: got %v, want %v", got, want)
	}
}

func TestWriteAcknowledgments_CommonWithoutReady(t *testing.T) {
	// Mid-negotiation: server has acknowledged some commons but is not
	// yet ready to send the pack (more rounds expected). The "ready"
	// trailer must be omitted.
	var buf bytes.Buffer
	commons := []string{"3333333333333333333333333333333333333333"}
	if err := WriteAcknowledgments(&buf, commons, nil, false); err != nil {
		t.Fatalf("WriteAcknowledgments: %v", err)
	}
	got := drainPayloads(t, &buf)
	want := []string{
		"acknowledgments\n",
		"ACK 3333333333333333333333333333333333333333\n",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ack stream: got %v, want %v", got, want)
	}
}

func TestWriteAcknowledgments_NoHavesEmitsNothing(t *testing.T) {
	// Initial fetch: client sent no haves, so the server must not emit
	// an acknowledgments section at all (per protocol-v2).
	var buf bytes.Buffer
	if err := WriteAcknowledgments(&buf, nil, nil, true); err != nil {
		t.Fatalf("WriteAcknowledgments: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("expected empty stream, got %d bytes: %q", buf.Len(), buf.String())
	}
}

func TestParseFetchArgs_DeepenRelative(t *testing.T) {
	args := tokensFromLines(
		"command=fetch\n",
		"DELIM",
		"want 1111111111111111111111111111111111111111\n",
		"deepen 2\n",
		"deepen-relative\n",
		"FLUSH",
	)
	got, err := ParseFetchArgs(args)
	if err != nil {
		t.Fatalf("ParseFetchArgs: %v", err)
	}
	if got.Depth != 2 {
		t.Fatalf("Depth: got %d, want 2", got.Depth)
	}
	if !got.DeepenRelative {
		t.Fatalf("DeepenRelative: got false, want true")
	}
}

// TestParseFetchArgs_PackfileURIs_Single covers the Phase 8 single-proto
// happy path: a single "packfile-uris=https" arg parses into a one-entry
// PackfileURIs slice.
func TestParseFetchArgs_PackfileURIs_Single(t *testing.T) {
	args := tokensFromLines(
		"command=fetch\n",
		"DELIM",
		"want 1111111111111111111111111111111111111111\n",
		"packfile-uris=https\n",
		"FLUSH",
	)
	got, err := ParseFetchArgs(args)
	if err != nil {
		t.Fatalf("ParseFetchArgs: %v", err)
	}
	if !reflect.DeepEqual(got.PackfileURIs, []string{"https"}) {
		t.Fatalf("PackfileURIs: got %v, want [https]", got.PackfileURIs)
	}
}

// TestParseFetchArgs_PackfileURIs_RejectsEmpty rejects "packfile-uris=" with
// no value (an empty CSV is not a valid client opt-in).
func TestParseFetchArgs_PackfileURIs_RejectsEmpty(t *testing.T) {
	args := tokensFromLines(
		"command=fetch\n",
		"DELIM",
		"want 1111111111111111111111111111111111111111\n",
		"packfile-uris=\n",
		"FLUSH",
	)
	if _, err := ParseFetchArgs(args); err == nil {
		t.Fatalf("ParseFetchArgs: expected error on empty packfile-uris value")
	}
}

// TestParseFetchArgs_PackfileURIs_FiltersUnknownProto verifies that unknown
// protocol schemes are silently dropped (capability negotiation semantics):
//   - ftp only → PackfileURIs nil/empty, no error
//   - https,ftp → PackfileURIs ["https"], no error (ftp filtered out)
//   - ftp,gopher → PackfileURIs nil/empty, no error (all filtered)
func TestParseFetchArgs_PackfileURIs_FiltersUnknownProto(t *testing.T) {
	t.Run("ftp_only", func(t *testing.T) {
		args := tokensFromLines(
			"command=fetch\n",
			"DELIM",
			"want 1111111111111111111111111111111111111111\n",
			"packfile-uris=ftp\n",
			"FLUSH",
		)
		got, err := ParseFetchArgs(args)
		if err != nil {
			t.Fatalf("ParseFetchArgs: expected no error for unknown scheme, got %v", err)
		}
		if len(got.PackfileURIs) != 0 {
			t.Fatalf("PackfileURIs: got %v, want nil/empty (ftp filtered)", got.PackfileURIs)
		}
	})
	t.Run("https_and_ftp", func(t *testing.T) {
		args := tokensFromLines(
			"command=fetch\n",
			"DELIM",
			"want 1111111111111111111111111111111111111111\n",
			"packfile-uris=https,ftp\n",
			"FLUSH",
		)
		got, err := ParseFetchArgs(args)
		if err != nil {
			t.Fatalf("ParseFetchArgs: expected no error for https,ftp, got %v", err)
		}
		if !reflect.DeepEqual(got.PackfileURIs, []string{"https"}) {
			t.Fatalf("PackfileURIs: got %v, want [https] (ftp filtered)", got.PackfileURIs)
		}
	})
	t.Run("ftp_and_gopher", func(t *testing.T) {
		args := tokensFromLines(
			"command=fetch\n",
			"DELIM",
			"want 1111111111111111111111111111111111111111\n",
			"packfile-uris=ftp,gopher\n",
			"FLUSH",
		)
		got, err := ParseFetchArgs(args)
		if err != nil {
			t.Fatalf("ParseFetchArgs: expected no error for all-unknown schemes, got %v", err)
		}
		if len(got.PackfileURIs) != 0 {
			t.Fatalf("PackfileURIs: got %v, want nil/empty (all filtered)", got.PackfileURIs)
		}
	})
}

// TestParseFetchArgs_PackfileURIs_MultipleArgs verifies multiple
// packfile-uris= lines accumulate (per protocol-v2 semantics for repeated
// args). Two "packfile-uris=https" lines yield a 2-entry slice.
func TestParseFetchArgs_PackfileURIs_MultipleArgs(t *testing.T) {
	args := tokensFromLines(
		"command=fetch\n",
		"DELIM",
		"want 1111111111111111111111111111111111111111\n",
		"packfile-uris=https\n",
		"packfile-uris=https\n",
		"FLUSH",
	)
	got, err := ParseFetchArgs(args)
	if err != nil {
		t.Fatalf("ParseFetchArgs: %v", err)
	}
	if !reflect.DeepEqual(got.PackfileURIs, []string{"https", "https"}) {
		t.Fatalf("PackfileURIs: got %v, want [https https]", got.PackfileURIs)
	}
}

// TestParseFetchArgs_PackfileURIs_RejectsEmptyEntryInCSV catches
// "packfile-uris=https,," — a malformed CSV with empty entries that
// would otherwise produce a zero-length proto string downstream.
func TestParseFetchArgs_PackfileURIs_RejectsEmptyEntryInCSV(t *testing.T) {
	args := tokensFromLines(
		"command=fetch\n",
		"DELIM",
		"want 1111111111111111111111111111111111111111\n",
		"packfile-uris=https,,https\n",
		"FLUSH",
	)
	if _, err := ParseFetchArgs(args); err == nil {
		t.Fatalf("ParseFetchArgs: expected error on empty CSV entry")
	}
}
