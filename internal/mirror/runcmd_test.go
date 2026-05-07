package mirror

import (
	"bytes"
	"os/exec"
)

func runCmd(dir, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.Bytes(), err
}

func mustCmd(t testingT, args ...string)               { execHelper(t, "", args...) }
func mustCmdIn(t testingT, dir string, args ...string) { execHelper(t, dir, args...) }
func execHelper(t testingT, dir string, args ...string) {
	t.Helper()
	out, err := runCmd(dir, args[0], args[1:]...)
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
}

// mustCmdCapture runs a command in dir and returns its combined output,
// failing the test on non-zero exit. Useful when the test needs to read
// the command's stdout (e.g. `git rev-parse HEAD`).
func mustCmdCapture(t testingT, dir string, args ...string) []byte {
	t.Helper()
	out, err := runCmd(dir, args[0], args[1:]...)
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
	return out
}

// Tiny interface so test helpers can call t.Helper / t.Fatalf without
// requiring a *testing.T import in the helper file.
type testingT interface {
	Helper()
	Fatalf(format string, args ...interface{})
}
