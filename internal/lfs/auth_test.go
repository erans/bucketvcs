package lfs

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

type fakeTokenIssuer struct {
	tokens    []fakeTokenRow
	createErr error
}

type fakeTokenRow struct {
	tokenID    string
	userID     string
	hash       string
	label      string
	expiresAt  int64
	hasExpires bool
}

func (f *fakeTokenIssuer) CreateToken(ctx context.Context, tokenID, userID, secretHash, label string, expiresAt *int64, scopes auth.TokenScope, scopeTenant, scopeRepo, scopePerm string) error {
	if f.createErr != nil {
		return f.createErr
	}
	row := fakeTokenRow{tokenID: tokenID, userID: userID, hash: secretHash, label: label}
	if expiresAt != nil {
		row.expiresAt = *expiresAt
		row.hasExpires = true
	}
	f.tokens = append(f.tokens, row)
	return nil
}

func TestIssueSSHToken_HappyPath_Upload(t *testing.T) {
	iss := &fakeTokenIssuer{}
	resp, err := IssueSSHToken(context.Background(), iss, "alice-userid", "alice", "acme", "foo", "upload",
		"https://gw.example", 15*time.Minute)
	if err != nil {
		t.Fatalf("IssueSSHToken: %v", err)
	}
	authz := resp.Header["Authorization"]
	if !strings.HasPrefix(authz, "Basic ") {
		t.Fatalf("Authorization header: got %q, want Basic ...", authz)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(authz, "Basic "))
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	user, secret, ok := strings.Cut(string(raw), ":")
	if !ok || user != "alice" || !strings.HasPrefix(secret, "bvts_") {
		t.Errorf("decoded basic credential: user=%q secret_prefix=%q", user, secret[:min(8, len(secret))])
	}
	if resp.Href != "https://gw.example/acme/foo.git/info/lfs" {
		t.Errorf("Href: got %q", resp.Href)
	}
	if time.Until(resp.ExpiresAt) > 15*time.Minute || time.Until(resp.ExpiresAt) < 14*time.Minute {
		t.Errorf("ExpiresAt out of window: %v", resp.ExpiresAt)
	}
	if len(iss.tokens) != 1 {
		t.Fatalf("expected 1 token persisted; got %d", len(iss.tokens))
	}
	row := iss.tokens[0]
	if row.userID != "alice-userid" || !row.hasExpires || row.label != "lfs-ssh:upload:acme/foo" {
		t.Errorf("token row mismatch: %+v", row)
	}
}

func TestIssueSSHToken_InvalidOp(t *testing.T) {
	iss := &fakeTokenIssuer{}
	_, err := IssueSSHToken(context.Background(), iss, "u", "n", "a", "b", "delete",
		"https://gw.example", time.Minute)
	if err == nil || !strings.Contains(err.Error(), "invalid op") {
		t.Fatalf("want invalid op error, got %v", err)
	}
}

func TestIssueSSHToken_TTLClampedFloor(t *testing.T) {
	iss := &fakeTokenIssuer{}
	_, err := IssueSSHToken(context.Background(), iss, "u", "n", "a", "b", "upload",
		"https://gw.example", 0)
	if err == nil || !strings.Contains(err.Error(), "ttl") {
		t.Fatalf("want ttl error, got %v", err)
	}
}

func TestIssueSSHToken_CreateTokenError(t *testing.T) {
	iss := &fakeTokenIssuer{createErr: errors.New("db down")}
	_, err := IssueSSHToken(context.Background(), iss, "u", "n", "a", "b", "upload",
		"https://gw.example", time.Minute)
	if err == nil || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("want wrapped issuer error, got %v", err)
	}
}

func TestIssueSSHToken_RequiresUserName(t *testing.T) {
	iss := &fakeTokenIssuer{}
	_, err := IssueSSHToken(context.Background(), iss, "uid", "", "acme", "foo", "upload",
		"https://gw.example", time.Minute)
	if err == nil || !strings.Contains(err.Error(), "userName") {
		t.Fatalf("want userName-required error, got %v", err)
	}
}

func TestIssueSSHToken_RejectsColonInUserName(t *testing.T) {
	iss := &fakeTokenIssuer{}
	_, err := IssueSSHToken(context.Background(), iss, "uid", "bob:role", "acme", "foo", "upload",
		"https://gw.example", time.Minute)
	if err == nil || !strings.Contains(err.Error(), "':'") {
		t.Fatalf("want colon-rejection error, got %v", err)
	}
}

func TestIssueSSHToken_RejectsControlCharsInUserName(t *testing.T) {
	iss := &fakeTokenIssuer{}
	for _, bad := range []string{"alice\n", "al\rice", "alice\x00", "alice\x7f"} {
		_, err := IssueSSHToken(context.Background(), iss, "uid", bad, "acme", "foo", "upload",
			"https://gw.example", time.Minute)
		if err == nil || !strings.Contains(err.Error(), "control characters") {
			t.Errorf("userName=%q: want control-char rejection, got %v", bad, err)
		}
	}
}
