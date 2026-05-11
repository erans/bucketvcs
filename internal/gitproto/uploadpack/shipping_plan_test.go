package uploadpack

import (
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/pack"
)

func TestShippingPlan_Equal_OrderIndependent(t *testing.T) {
	a := ShippingPlan{Commits: []pack.OID{oid(1), oid(2)}, Refs: map[string]pack.OID{"main": oid(2)}}
	b := ShippingPlan{Commits: []pack.OID{oid(2), oid(1)}, Refs: map[string]pack.OID{"main": oid(2)}}
	if !a.Equal(b) {
		t.Fatalf("Equal should ignore commit order")
	}
}

func TestShippingPlan_Equal_DifferentRefs(t *testing.T) {
	a := ShippingPlan{Refs: map[string]pack.OID{"main": oid(2)}}
	b := ShippingPlan{Refs: map[string]pack.OID{"main": oid(3)}}
	if a.Equal(b) {
		t.Fatalf("Equal should detect ref diff")
	}
}

func oid(b byte) pack.OID {
	var o pack.OID
	o[0] = b
	return o
}
