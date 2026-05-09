package auth

// Actor identifies an authenticated principal. A nil *Actor means anonymous.
type Actor struct {
	UserID  string
	Name    string
	IsAdmin bool
}

// Credential is the closed set of credential shapes the gateway accepts.
// New shapes are added in later milestones (M6 adds SSHKeyFingerprint).
type Credential interface{ isCredential() }

// BasicPassword is HTTP Basic auth: username + token-as-password.
type BasicPassword struct {
	Username string
	Password string
}

func (BasicPassword) isCredential() {}

// SSHKeyFingerprint is the SSH public-key authentication credential. M6
// populates it; M4 only declares it for interface stability.
type SSHKeyFingerprint struct {
	Fingerprint string
}

func (SSHKeyFingerprint) isCredential() {}

// Action is the operation the actor is attempting on a repo.
type Action int

const (
	ActionRead Action = iota
	ActionWrite
)

// Perm is the granted permission level for an actor on a repo.
type Perm int

const (
	PermNone Perm = iota
	PermRead
	PermWrite
	PermAdmin
)

// RepoFlags carries the per-repo bits relevant to authorization.
type RepoFlags struct {
	PublicRead bool
}

// Scope narrows a credential to a single repo at a specific permission
// level. Returned by Store.VerifyCredential for deploy-key SSH credentials;
// nil for HTTP token credentials and user SSH keys.
//
// When Scope is non-nil, the gateway middleware short-circuits the
// per-repo permission lookup and uses Scope.Perm directly, after asserting
// that Scope.Tenant and Scope.Repo match the requested resource.
type Scope struct {
	Tenant string
	Repo   string
	Perm   Perm // PermRead or PermWrite. PermAdmin is not allowed for deploy keys.
}

// SSHKey is the persisted shape of an entry in the ssh_keys table.
// Exactly one of (UserID) or (ScopeTenant + ScopeRepo + ScopePerm) is set.
// The schema enforces this XOR via a CHECK constraint.
type SSHKey struct {
	ID          string // bvsk_<24 base32>
	Fingerprint string // "SHA256:..." OpenSSH form
	PublicKey   []byte // raw wire-format public key bytes
	KeyType     string // "ssh-ed25519" | "ssh-rsa" | "ecdsa-sha2-..."
	Label       string

	// Set for user keys; empty for deploy keys.
	UserID string

	// Set for deploy keys; empty for user keys.
	ScopeTenant string
	ScopeRepo   string
	ScopePerm   Perm

	CreatedAt  int64
	LastUsedAt int64
	RevokedAt  int64
}
