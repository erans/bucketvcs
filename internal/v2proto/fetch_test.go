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
	if err := WriteAcknowledgments(&buf, nil, []string{"3333333333333333333333333333333333333333"}); err != nil {
		t.Fatalf("WriteAcknowledgments: %v", err)
	}
	got := drainPayloads(t, &buf)
	want := []string{"acknowledgments\n", "NAK\n"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ack stream: got %v, want %v", got, want)
	}
}

func TestWriteAcknowledgments_SomeCommon(t *testing.T) {
	var buf bytes.Buffer
	commons := []string{"3333333333333333333333333333333333333333"}
	if err := WriteAcknowledgments(&buf, commons, nil); err != nil {
		t.Fatalf("WriteAcknowledgments: %v", err)
	}
	got := drainPayloads(t, &buf)
	want := []string{
		"acknowledgments\n",
		"ACK 3333333333333333333333333333333333333333 common\n",
		"ready\n",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ack stream: got %v, want %v", got, want)
	}
}

func TestParseFetchArgs_DeepenSinceAndNot(t *testing.T) {
	args := tokensFromLines(
		"command=fetch\n",
		"DELIM",
		"want 1111111111111111111111111111111111111111\n",
		"deepen-since 1700000000\n",
		"deepen-not refs/heads/main\n",
		"deepen-not refs/tags/v1\n",
		"FLUSH",
	)
	got, err := ParseFetchArgs(args)
	if err != nil {
		t.Fatalf("ParseFetchArgs: %v", err)
	}
	if got.DeepenSince != "1700000000" {
		t.Fatalf("DeepenSince: %q", got.DeepenSince)
	}
	wantNot := []string{"refs/heads/main", "refs/tags/v1"}
	if !reflect.DeepEqual(got.DeepenNot, wantNot) {
		t.Fatalf("DeepenNot: got %v, want %v", got.DeepenNot, wantNot)
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
	if err := WriteAcknowledgments(&buf, commons, nil); err != nil {
		t.Fatalf("WriteAcknowledgments: %v", err)
	}
	got := drainPayloads(t, &buf)
	want := []string{
		"acknowledgments\n",
		"ACK 3333333333333333333333333333333333333333 common\n",
		"ACK 4444444444444444444444444444444444444444 common\n",
		"ACK 5555555555555555555555555555555555555555 common\n",
		"ready\n",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ack stream: got %v, want %v", got, want)
	}
}
