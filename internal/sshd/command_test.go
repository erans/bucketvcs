package sshd

import (
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

func TestParseExecCommand(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantOp  ExecOp
		wantT   string
		wantR   string
		wantErr bool
	}{
		// Accepted shapes
		{"single-quoted upload", `git-upload-pack 'acme/web.git'`, OpUpload, "acme", "web", false},
		{"double-quoted upload", `git-upload-pack "acme/web.git"`, OpUpload, "acme", "web", false},
		{"unquoted upload", `git-upload-pack acme/web.git`, OpUpload, "acme", "web", false},
		{"leading-slash upload", `git-upload-pack /acme/web.git`, OpUpload, "acme", "web", false},
		{"single-quoted receive", `git-receive-pack 'acme/web.git'`, OpReceive, "acme", "web", false},
		{"unquoted receive", `git-receive-pack acme/web.git`, OpReceive, "acme", "web", false},
		// Rejected
		{"upload-archive forbidden", `git-upload-archive 'acme/web.git'`, 0, "", "", true},
		{"shell forbidden", `bash`, 0, "", "", true},
		{"git-shell forbidden", `git-shell -c ls`, 0, "", "", true},
		{"empty", ``, 0, "", "", true},
		{"missing .git suffix", `git-upload-pack 'acme/web'`, 0, "", "", true},
		{"traversal", `git-upload-pack '../web.git'`, 0, "", "", true},
		{"trailing garbage after quote", `git-upload-pack 'acme/web.git'extra`, 0, "", "", true},
		{"mixed quotes", `git-upload-pack 'acme/web.git"`, 0, "", "", true},
		{"percent-encoding", `git-upload-pack acme%2fweb.git`, 0, "", "", true},
		{"too many slashes", `git-upload-pack a/b/c.git`, 0, "", "", true},
		{"NUL byte", "git-upload-pack 'acme/\x00web.git'", 0, "", "", true},
		{"backslash", `git-upload-pack 'acme\web.git'`, 0, "", "", true},
		{"invalid char in name", `git-upload-pack 'acme$/web.git'`, 0, "", "", true},
		{"only quotes", `git-upload-pack ''`, 0, "", "", true},
		{"no arg", `git-upload-pack`, 0, "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseExecCommand(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("got %+v, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if got.Op != tc.wantOp || got.Tenant != tc.wantT || got.Repo != tc.wantR {
				t.Fatalf("got %+v, want op=%v tenant=%q repo=%q", got, tc.wantOp, tc.wantT, tc.wantR)
			}
		})
	}
}

func TestParseExecCommand_RequiredAction(t *testing.T) {
	// upload-pack → ActionRead
	cmd, err := ParseExecCommand("git-upload-pack 'acme/web.git'")
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Op.RequiredAction() != auth.ActionRead {
		t.Fatalf("upload-pack: got %v, want ActionRead (%v)", cmd.Op.RequiredAction(), auth.ActionRead)
	}

	// receive-pack → ActionWrite
	cmd, err = ParseExecCommand("git-receive-pack 'acme/web.git'")
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Op.RequiredAction() != auth.ActionWrite {
		t.Fatalf("receive-pack: got %v, want ActionWrite (%v)", cmd.Op.RequiredAction(), auth.ActionWrite)
	}
}
