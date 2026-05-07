package auth

// Decide reports whether actor with perm may perform action against a repo
// with flags. The second return is a short reason string suitable for log
// output on deny; empty on allow. Anonymous (actor == nil) is treated as
// PermNone. is_admin short-circuits to allow.
func Decide(actor *Actor, perm Perm, action Action, flags RepoFlags) (bool, string) {
	if actor != nil && actor.IsAdmin {
		return true, ""
	}
	switch action {
	case ActionRead:
		if flags.PublicRead {
			return true, ""
		}
		if perm >= PermRead {
			return true, ""
		}
		if actor == nil {
			return false, "anonymous read on private repo"
		}
		return false, "user has no read permission"
	case ActionWrite:
		if perm >= PermWrite {
			return true, ""
		}
		if actor == nil {
			return false, "anonymous write"
		}
		return false, "user has no write permission"
	default:
		return false, "unknown action"
	}
}
