package auth

import "testing"

func TestDecide(t *testing.T) {
	type tc struct {
		name   string
		actor  *Actor
		perm   Perm
		action Action
		flags  RepoFlags
		want   bool
	}
	admin := &Actor{UserID: "u1", Name: "admin", IsAdmin: true}
	user := &Actor{UserID: "u2", Name: "alice"}
	cases := []tc{
		{"anon read public", nil, PermNone, ActionRead, RepoFlags{PublicRead: true}, true},
		{"anon read private", nil, PermNone, ActionRead, RepoFlags{}, false},
		{"anon write public", nil, PermNone, ActionWrite, RepoFlags{PublicRead: true}, false},
		{"anon write private", nil, PermNone, ActionWrite, RepoFlags{}, false},
		{"user-none read public", user, PermNone, ActionRead, RepoFlags{PublicRead: true}, true},
		{"user-none read private", user, PermNone, ActionRead, RepoFlags{}, false},
		{"user-none write public", user, PermNone, ActionWrite, RepoFlags{PublicRead: true}, false},
		{"user-none write private", user, PermNone, ActionWrite, RepoFlags{}, false},
		{"user-read read", user, PermRead, ActionRead, RepoFlags{}, true},
		{"user-read write", user, PermRead, ActionWrite, RepoFlags{}, false},
		{"user-write read", user, PermWrite, ActionRead, RepoFlags{}, true},
		{"user-write write", user, PermWrite, ActionWrite, RepoFlags{}, true},
		{"user-admin read", user, PermAdmin, ActionRead, RepoFlags{}, true},
		{"user-admin write", user, PermAdmin, ActionWrite, RepoFlags{}, true},
		{"is-admin none read", admin, PermNone, ActionRead, RepoFlags{}, true},
		{"is-admin none write", admin, PermNone, ActionWrite, RepoFlags{}, true},
		{"is-admin none write public", admin, PermNone, ActionWrite, RepoFlags{PublicRead: true}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := Decide(c.actor, c.perm, c.action, c.flags)
			if got != c.want {
				t.Fatalf("got %v want %v", got, c.want)
			}
		})
	}
}

func TestDecideReasonNonEmptyOnDeny(t *testing.T) {
	ok, reason := Decide(nil, PermNone, ActionWrite, RepoFlags{})
	if ok {
		t.Fatal("expected deny")
	}
	if reason == "" {
		t.Fatal("expected non-empty reason on deny")
	}
}

// TestDecide_NilActorIgnoresPerm guards against a caller mistakenly
// passing a non-none perm with a nil actor. Decide must normalize and
// reject anonymous writes/reads-on-private regardless of the perm value.
func TestDecide_NilActorIgnoresPerm(t *testing.T) {
	cases := []struct {
		name   string
		perm   Perm
		action Action
		flags  RepoFlags
		want   bool
	}{
		{"nil+PermAdmin+write+private", PermAdmin, ActionWrite, RepoFlags{}, false},
		{"nil+PermWrite+write+private", PermWrite, ActionWrite, RepoFlags{}, false},
		{"nil+PermRead+write+public", PermRead, ActionWrite, RepoFlags{PublicRead: true}, false},
		{"nil+PermAdmin+read+private", PermAdmin, ActionRead, RepoFlags{}, false},
		{"nil+PermAdmin+read+public", PermAdmin, ActionRead, RepoFlags{PublicRead: true}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := Decide(nil, c.perm, c.action, c.flags)
			if got != c.want {
				t.Fatalf("Decide(nil, %v, %v, %+v) = %v want %v", c.perm, c.action, c.flags, got, c.want)
			}
		})
	}
}

// TestDecide_DeployKeyScopeSymmetry asserts that a synthetic deploy-key
// actor + scope-derived perm produces identical Decide outcomes to a real
// user with the equivalent grant. This is the contract that lets the SSH
// session handler (M6 Task 21) rely on Decide without a separate code path
// for scoped credentials.
func TestDecide_DeployKeyScopeSymmetry(t *testing.T) {
	flags := RepoFlags{}
	cases := []struct {
		name   string
		perm   Perm
		action Action
		want   bool
	}{
		{"read_perm read_action", PermRead, ActionRead, true},
		{"read_perm write_action", PermRead, ActionWrite, false},
		{"write_perm read_action", PermWrite, ActionRead, true},
		{"write_perm write_action", PermWrite, ActionWrite, true},
	}
	deployActor := &Actor{UserID: "deploy:bvsk_xyz", Name: "deploy-key:ci"}
	userActor := &Actor{UserID: "u_abc", Name: "alice"}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotDeploy, _ := Decide(deployActor, tc.perm, tc.action, flags)
			gotUser, _ := Decide(userActor, tc.perm, tc.action, flags)
			if gotDeploy != gotUser {
				t.Fatalf("symmetry violation: deploy=%v user=%v perm=%v action=%v",
					gotDeploy, gotUser, tc.perm, tc.action)
			}
			if gotDeploy != tc.want {
				t.Fatalf("got %v, want %v (perm=%v action=%v)",
					gotDeploy, tc.want, tc.perm, tc.action)
			}
		})
	}
}
