// Package reachability implements the M10 reachability index: a Set
// view that combines a base (.bvcg v2 + .bvom) with a chain of .bvrd
// deltas (one per push), used by upload-pack to answer want/have
// negotiation without materializing the on-disk mirror.
//
// Producers:
//
//	internal/gitproto/receivepack writes one .bvrd per push.
//	internal/maintenance compacts the chain back to an empty list.
//
// Consumers:
//
//	internal/gitproto/uploadpack reads Set during negotiation.
//	cmd/bucketvcs/negotiate is a debug CLI over the same path.
package reachability
