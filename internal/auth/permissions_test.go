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
