package gateway

import (
	"encoding/json"
	"net/http"

	"github.com/bucketvcs/bucketvcs/internal/replica"
)

// replicaGateCheck consults the replica freshness gate for read entry
// points. Returns false after writing the 503 when the replica is
// unhealthy for ref advertisement (bounded-stale past its lag budget).
func (s *Server) replicaGateCheck(w http.ResponseWriter, r *http.Request, tenant, repoID string) bool {
	if s.opts.Replica == nil || s.opts.Replica.Gate == nil {
		return true
	}
	if err := s.opts.Replica.Gate.CheckAdvertise(r.Context(), tenant, repoID); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return false
	}
	return true
}

// replicaRefuseWrite writes the read-only refusal for write endpoints.
func (s *Server) replicaRefuseWrite(w http.ResponseWriter) {
	http.Error(w, replica.RefusalMessage(s.opts.Replica.WriteRegionURL), http.StatusForbidden)
}

// handleHealthzReplica serves the replica health snapshot. Mounted only
// when Options.Replica is set; /healthz stays a plain liveness "ok" for
// load balancers.
func (s *Server) handleHealthzReplica(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var snap replica.HealthSnapshot
	if s.opts.Replica.Health != nil {
		snap = s.opts.Replica.Health()
	} else {
		snap = replica.HealthSnapshot{Role: "replica"}
	}
	_ = json.NewEncoder(w).Encode(snap)
}

// replicaWriteURL is a nil-safe accessor used when building LFS deps.
func replicaWriteURL(r *replica.GatewayConfig) string {
	if r == nil {
		return ""
	}
	return r.WriteRegionURL
}
