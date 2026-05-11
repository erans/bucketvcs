package uploadpack

import (
	"bytes"
	"sort"

	"github.com/bucketvcs/bucketvcs/internal/pack"
)

// ShippingPlan is the set of commits and ref tips the server will
// stream to the client. Produced by Negotiate; consumed by Deliver.
type ShippingPlan struct {
	Commits []pack.OID
	Refs    map[string]pack.OID
}

// Equal reports order-independent equality.
func (p ShippingPlan) Equal(q ShippingPlan) bool {
	if len(p.Commits) != len(q.Commits) || len(p.Refs) != len(q.Refs) {
		return false
	}
	ps := append([]pack.OID(nil), p.Commits...)
	qs := append([]pack.OID(nil), q.Commits...)
	sort.Slice(ps, func(i, j int) bool { return bytes.Compare(ps[i][:], ps[j][:]) < 0 })
	sort.Slice(qs, func(i, j int) bool { return bytes.Compare(qs[i][:], qs[j][:]) < 0 })
	for i := range ps {
		if ps[i] != qs[i] {
			return false
		}
	}
	for k, v := range p.Refs {
		if q.Refs[k] != v {
			return false
		}
	}
	return true
}
