package v2proto

import (
	"fmt"
	"io"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/pktline"
)

// BundleAdvertisement is one entry in the bundle-uri response.
type BundleAdvertisement struct {
	ID          string // BundleEntry.ID
	URI         string // direct or proxied URL
	CreationTok string // unix-seconds string of GeneratedAt
}

// validBundleIDChar reports whether c is allowed in a bundle ID. The
// allowed set matches what maintenance writes today (alphanumerics,
// underscore, dash) and is deliberately narrower than what RFC-3986
// would accept, so a future schema change can't inject "." or "=" and
// fracture the `bundle.<id>.<key>=<value>` framing.
func validBundleIDChar(c byte) bool {
	return (c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') ||
		c == '_' || c == '-'
}

func validateAdvertisement(ad BundleAdvertisement) error {
	if ad.ID == "" {
		return fmt.Errorf("v2proto: bundle advertisement ID is empty")
	}
	if ad.URI == "" {
		return fmt.Errorf("v2proto: bundle advertisement URI is empty")
	}
	for i := 0; i < len(ad.ID); i++ {
		if !validBundleIDChar(ad.ID[i]) {
			return fmt.Errorf("v2proto: bundle advertisement ID contains invalid character %q at position %d", ad.ID[i], i)
		}
	}
	if strings.ContainsAny(ad.URI, "\r\n\x00") {
		return fmt.Errorf("v2proto: bundle advertisement URI contains forbidden control character")
	}
	if strings.ContainsAny(ad.CreationTok, "\r\n\x00") {
		return fmt.Errorf("v2proto: bundle advertisement CreationTok contains forbidden control character")
	}
	return nil
}

// EncodeBundleURIResponse writes the v2 bundle-uri response per Git's
// protocol-v2 bundle-uri.txt. When ads is non-empty the response begins
// with the two REQUIRED header keys (bundle.version=1, bundle.mode=all)
// followed by one or more `bundle.<id>.<key>=<value>` pkt-lines, then a
// flush-pkt. An empty ads list emits only the flush-pkt; clients fall
// through to standard fetch. Per Git's bundle-uri.adoc: "A client
// receiving a bundle list without a `bundle.mode` key SHOULD consider
// the entire bundle list invalid."
//
// Each advertisement is validated: ID must be non-empty and contain
// only [A-Za-z0-9_-]; URI must be non-empty and free of CR/LF/NUL;
// CreationTok must be free of CR/LF/NUL. The strict charset on ID
// prevents a bundle ID containing "." or "=" from fracturing the
// response framing.
func EncodeBundleURIResponse(w io.Writer, ads []BundleAdvertisement) error {
	for _, ad := range ads {
		if err := validateAdvertisement(ad); err != nil {
			return err
		}
	}
	pw := pktline.NewWriter(w)
	if len(ads) > 0 {
		if err := pw.WriteString("bundle.version=1\n"); err != nil {
			return err
		}
		if err := pw.WriteString("bundle.mode=all\n"); err != nil {
			return err
		}
	}
	for _, ad := range ads {
		if err := pw.WriteString(fmt.Sprintf("bundle.%s.uri=%s\n", ad.ID, ad.URI)); err != nil {
			return err
		}
		if ad.CreationTok != "" {
			if err := pw.WriteString(fmt.Sprintf("bundle.%s.creationToken=%s\n", ad.ID, ad.CreationTok)); err != nil {
				return err
			}
		}
	}
	return pw.WriteFlush()
}
