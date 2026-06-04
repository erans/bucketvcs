// Package doctor is the check framework behind `bucketvcs doctor`: a list of
// named read-only checks executed sequentially, reported one line per check
// (human or NDJSON). Checks themselves live in cmd/bucketvcs/doctor.go —
// they close over CLI flags and the cmd package's open helpers.
package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Status is a check outcome. Only StatusFail affects the exit code.
type Status string

const (
	StatusOK   Status = "ok"
	StatusWarn Status = "warn" // suspicious but not fatal (e.g. unsafe-no-sandbox)
	StatusFail Status = "fail"
	StatusSkip Status = "skip" // not applicable under this configuration
)

// Result is what a check returns.
type Result struct {
	Status Status
	Detail string
}

// Check is one named diagnostic.
type Check struct {
	Name string
	Run  func(ctx context.Context) Result
}

// jsonLine is the NDJSON shape (house style: one object per line).
type jsonLine struct {
	Check  string `json:"check"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// Run executes checks in order, writing one line per check to w. Returns the
// number of failed checks (callers exit 1 when > 0).
func Run(ctx context.Context, w io.Writer, jsonOut bool, checks []Check) int {
	failed := 0
	enc := json.NewEncoder(w)
	for _, c := range checks {
		res := c.Run(ctx)
		if res.Status == StatusFail {
			failed++
		}
		if jsonOut {
			_ = enc.Encode(jsonLine{Check: c.Name, Status: string(res.Status), Detail: res.Detail})
			continue
		}
		fmt.Fprintf(w, "%-5s %-24s %s\n", strings.ToUpper(string(res.Status)), c.Name, res.Detail)
	}
	return failed
}
