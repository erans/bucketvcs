package v2proto

import (
	"bytes"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/pktline"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
)

func tokensFromLines(lines ...string) []pktline.Token {
	var out []pktline.Token
	for _, l := range lines {
		switch l {
		case "FLUSH":
			out = append(out, pktline.Token{Type: pktline.Flush})
		case "DELIM":
			out = append(out, pktline.Token{Type: pktline.Delim})
		default:
			out = append(out, pktline.Token{Type: pktline.Data, Payload: []byte(l)})
		}
	}
	return out
}

func TestLsRefs_BasicAdvertisement(t *testing.T) {
	body := &manifest.Body{
		DefaultBranch: "main",
		Refs: map[string]string{
			"refs/heads/main":    "1111111111111111111111111111111111111111",
			"refs/heads/feature": "2222222222222222222222222222222222222222",
		},
	}
	args := tokensFromLines(
		"command=ls-refs\n",
		"DELIM",
		"FLUSH",
	)
	var buf bytes.Buffer
	if err := HandleLsRefs(args, body, &buf); err != nil {
		t.Fatalf("HandleLsRefs: %v", err)
	}
	got := drainPayloads(t, &buf)
	// HEAD resolves to refs/heads/main, so it is included even without symrefs.
	want := []string{
		"1111111111111111111111111111111111111111 HEAD\n",
		"2222222222222222222222222222222222222222 refs/heads/feature\n",
		"1111111111111111111111111111111111111111 refs/heads/main\n",
	}
	if !equalIgnoreOrder(got, want) {
		t.Fatalf("output: got %v, want %v", got, want)
	}
}

func TestLsRefs_SymrefAndRefPrefix(t *testing.T) {
	body := &manifest.Body{
		DefaultBranch: "main",
		Refs: map[string]string{
			"refs/heads/main": "1111111111111111111111111111111111111111",
			"refs/tags/v1":    "3333333333333333333333333333333333333333",
		},
	}
	args := tokensFromLines(
		"command=ls-refs\n",
		"DELIM",
		"symrefs\n",
		"ref-prefix refs/heads/\n",
		"FLUSH",
	)
	var buf bytes.Buffer
	if err := HandleLsRefs(args, body, &buf); err != nil {
		t.Fatalf("HandleLsRefs: %v", err)
	}
	got := drainPayloads(t, &buf)
	if len(got) != 1 {
		t.Fatalf("expected 1 line filtered to refs/heads/, got %v", got)
	}
	// HEAD is filtered out because "HEAD" does not start with "refs/heads/".
	// The symref annotation appears only on the HEAD line per protocol-v2 spec,
	// so refs/heads/main gets no annotation when HEAD is filtered.
	want := "1111111111111111111111111111111111111111 refs/heads/main\n"
	if got[0] != want {
		t.Fatalf("output[0]: got %q, want %q", got[0], want)
	}
}

func TestLsRefs_UnbornHEAD(t *testing.T) {
	body := &manifest.Body{
		DefaultBranch: "main",
		Refs:          map[string]string{},
	}
	args := tokensFromLines(
		"command=ls-refs\n",
		"DELIM",
		"unborn\n",
		"symrefs\n",
		"FLUSH",
	)
	var buf bytes.Buffer
	if err := HandleLsRefs(args, body, &buf); err != nil {
		t.Fatalf("HandleLsRefs: %v", err)
	}
	got := drainPayloads(t, &buf)
	want := []string{"unborn HEAD symref-target:refs/heads/main\n"}
	if !equalIgnoreOrder(got, want) {
		t.Fatalf("output: got %v, want %v", got, want)
	}
}

func TestLsRefs_RejectsRefPrefixWithSpace(t *testing.T) {
	body := &manifest.Body{Refs: map[string]string{}}
	args := tokensFromLines(
		"command=ls-refs\n",
		"DELIM",
		"ref-prefix refs/heads/ extra\n",
		"FLUSH",
	)
	var buf bytes.Buffer
	if err := HandleLsRefs(args, body, &buf); err == nil {
		t.Fatalf("HandleLsRefs: expected error on multi-token ref-prefix")
	}
}

func drainPayloads(t *testing.T, r *bytes.Buffer) []string {
	t.Helper()
	pr := pktline.NewReader(r)
	var out []string
	for {
		tok, err := pr.Read()
		if err != nil {
			break
		}
		if tok.Type == pktline.Data {
			out = append(out, string(tok.Payload))
		}
	}
	return out
}

func equalIgnoreOrder(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	cnt := map[string]int{}
	for _, s := range a {
		cnt[s]++
	}
	for _, s := range b {
		cnt[s]--
		if cnt[s] < 0 {
			return false
		}
	}
	return true
}

func TestLsRefs_HEADWithSymrefsNoPrefix(t *testing.T) {
	body := &manifest.Body{
		DefaultBranch: "main",
		Refs: map[string]string{
			"refs/heads/main": "1111111111111111111111111111111111111111",
		},
	}
	args := tokensFromLines(
		"command=ls-refs\n",
		"DELIM",
		"symrefs\n",
		"FLUSH",
	)
	var buf bytes.Buffer
	if err := HandleLsRefs(args, body, &buf); err != nil {
		t.Fatalf("HandleLsRefs: %v", err)
	}
	got := drainPayloads(t, &buf)
	want := []string{
		"1111111111111111111111111111111111111111 HEAD symref-target:refs/heads/main\n",
		"1111111111111111111111111111111111111111 refs/heads/main\n",
	}
	if !equalIgnoreOrder(got, want) {
		t.Fatalf("output: got %v, want %v", got, want)
	}
}

func TestLsRefs_EmptyRefsNoUnbornEmitsOnlyFlush(t *testing.T) {
	body := &manifest.Body{
		DefaultBranch: "main",
		Refs:          map[string]string{},
	}
	args := tokensFromLines(
		"command=ls-refs\n",
		"DELIM",
		"FLUSH",
	)
	var buf bytes.Buffer
	if err := HandleLsRefs(args, body, &buf); err != nil {
		t.Fatalf("HandleLsRefs: %v", err)
	}
	got := drainPayloads(t, &buf)
	if len(got) != 0 {
		t.Fatalf("expected no data frames, got %v", got)
	}
}

func TestLsRefs_EmptyDefaultBranchUnbornNoSymrefAnnotation(t *testing.T) {
	body := &manifest.Body{
		DefaultBranch: "",
		Refs:          map[string]string{},
	}
	args := tokensFromLines(
		"command=ls-refs\n",
		"DELIM",
		"unborn\n",
		"symrefs\n",
		"FLUSH",
	)
	var buf bytes.Buffer
	if err := HandleLsRefs(args, body, &buf); err != nil {
		t.Fatalf("HandleLsRefs: %v", err)
	}
	got := drainPayloads(t, &buf)
	want := []string{"unborn HEAD\n"}
	if !equalIgnoreOrder(got, want) {
		t.Fatalf("output: got %v, want %v", got, want)
	}
}

// Per gitprotocol-v2, command requests may include capability lines (e.g.
// agent=..., object-format=...) between the command line and the delim.
// iterateArgs must tolerate them rather than treating them as ls-refs args.
func TestLsRefs_TolerantesPreDelimCapabilityLines(t *testing.T) {
	body := &manifest.Body{
		DefaultBranch: "main",
		Refs: map[string]string{
			"refs/heads/main": "1111111111111111111111111111111111111111",
		},
	}
	args := tokensFromLines(
		"command=ls-refs\n",
		"agent=git/2.43.0\n",
		"object-format=sha1\n",
		"DELIM",
		"symrefs\n",
		"FLUSH",
	)
	var buf bytes.Buffer
	if err := HandleLsRefs(args, body, &buf); err != nil {
		t.Fatalf("HandleLsRefs: %v", err)
	}
	got := drainPayloads(t, &buf)
	want := []string{
		"1111111111111111111111111111111111111111 HEAD symref-target:refs/heads/main\n",
		"1111111111111111111111111111111111111111 refs/heads/main\n",
	}
	if !equalIgnoreOrder(got, want) {
		t.Fatalf("output: got %v, want %v", got, want)
	}
}

// A request with no delim (just command + capabilities + flush) carries no
// command-specific args; the handler should produce a default advertisement.
func TestLsRefs_NoDelimNoArgs(t *testing.T) {
	body := &manifest.Body{
		DefaultBranch: "main",
		Refs: map[string]string{
			"refs/heads/main": "1111111111111111111111111111111111111111",
		},
	}
	args := tokensFromLines(
		"command=ls-refs\n",
		"agent=git/2.43.0\n",
		"FLUSH",
	)
	var buf bytes.Buffer
	if err := HandleLsRefs(args, body, &buf); err != nil {
		t.Fatalf("HandleLsRefs: %v", err)
	}
	got := drainPayloads(t, &buf)
	want := []string{
		"1111111111111111111111111111111111111111 HEAD\n",
		"1111111111111111111111111111111111111111 refs/heads/main\n",
	}
	if !equalIgnoreOrder(got, want) {
		t.Fatalf("output: got %v, want %v", got, want)
	}
}
