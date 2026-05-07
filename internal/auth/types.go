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
