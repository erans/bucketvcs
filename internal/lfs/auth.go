package lfs

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

type SSHAuthResponse struct {
	Href      string            `json:"href"`
	Header    map[string]string `json:"header"`
	ExpiresAt time.Time         `json:"expires_at"`
}

// TokenIssuer is the narrow seam IssueSSHToken needs to persist a
// token row. We deliberately do NOT reuse auth.Store — token creation
// is admin-only and not part of the gateway request-path interface.
//
// The scopes argument carries the M17 TokenScope bitmask. SSH-issued LFS
// tokens currently pass auth.ScopeLegacy (=0) because the gateway's LFS
// path falls back to grant-table semantics when scopes is zero. A future
// M17 follow-up may narrow these to lfs:read/lfs:write per op.
type TokenIssuer interface {
	CreateToken(ctx context.Context, tokenID, userID, secretHash, label string, expiresAt *int64, scopes auth.TokenScope, scopeTenant, scopeRepo, scopePerm string) error
}

// IssueSSHToken mints a short-TTL HTTP token scoped to (userID, repo,
// op) and returns the LFS-spec SSHAuthResponse. The Authorization
// header uses HTTP Basic with (userName, bvts_token) because the M4
// gateway auth.go only parses Basic credentials and matches Username
// against the user's name. A future Bearer-capable gateway path could
// drop this dance.
//
// userName is the human-readable name backing userID; the gateway's
// verifyBasicPassword path requires it to match the users.name column.
func IssueSSHToken(ctx context.Context, issuer TokenIssuer, userID, userName, tenant, repo, op, baseURL string, ttl time.Duration) (SSHAuthResponse, error) {
	switch op {
	case "upload", "download":
	default:
		return SSHAuthResponse{}, fmt.Errorf("lfs.IssueSSHToken: invalid op %q (want upload or download)", op)
	}
	if ttl <= 0 {
		return SSHAuthResponse{}, fmt.Errorf("lfs.IssueSSHToken: ttl must be > 0 (got %s)", ttl)
	}
	if userID == "" || userName == "" || tenant == "" || repo == "" || baseURL == "" {
		return SSHAuthResponse{}, fmt.Errorf("lfs.IssueSSHToken: userID, userName, tenant, repo, baseURL must be non-empty")
	}
	// userName is embedded as the Basic-auth username; a colon would
	// split the credential against the gateway's r.BasicAuth() boundary
	// and silently 401 every Batch call. Control characters could
	// inject CRLF into anything that later prints the decoded credential.
	// CreateUser does not enforce these constraints today, so reject here.
	if strings.ContainsRune(userName, ':') {
		return SSHAuthResponse{}, fmt.Errorf("lfs.IssueSSHToken: userName must not contain ':' (would break Basic auth encoding)")
	}
	for _, r := range userName {
		if r < 0x20 || r == 0x7f {
			return SSHAuthResponse{}, fmt.Errorf("lfs.IssueSSHToken: userName must not contain control characters")
		}
	}

	token, id, secret, err := auth.GenerateToken()
	if err != nil {
		return SSHAuthResponse{}, fmt.Errorf("lfs.IssueSSHToken: GenerateToken: %w", err)
	}
	hash, err := auth.HashSecret(secret)
	if err != nil {
		return SSHAuthResponse{}, fmt.Errorf("lfs.IssueSSHToken: HashSecret: %w", err)
	}

	expires := time.Now().Add(ttl)
	expUnix := expires.Unix()
	label := fmt.Sprintf("lfs-ssh:%s:%s/%s", op, tenant, repo)

	if err := issuer.CreateToken(ctx, id, userID, hash, label, &expUnix, auth.ScopeLegacy, "", "", ""); err != nil {
		return SSHAuthResponse{}, fmt.Errorf("lfs.IssueSSHToken: CreateToken: %w", err)
	}

	basic := base64.StdEncoding.EncodeToString([]byte(userName + ":" + token))
	return SSHAuthResponse{
		Href:      fmt.Sprintf("%s/%s/%s.git/info/lfs", strings.TrimRight(baseURL, "/"), tenant, repo),
		Header:    map[string]string{"Authorization": "Basic " + basic},
		ExpiresAt: expires,
	}, nil
}
