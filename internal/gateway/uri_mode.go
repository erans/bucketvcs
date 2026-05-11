package gateway

// URIMode controls how the gateway delivers bundle and pack URIs to clients.
type URIMode int

const (
	URIModeAuto    URIMode = iota // try direct (signed); fall back to proxied
	URIModeDirect                 // direct only; error if adapter cannot sign
	URIModeProxied                // gateway-proxied only
	URIModeOff                    // do not advertise the URI capability
)

func ParseURIMode(s string) (URIMode, bool) {
	switch s {
	case "auto":
		return URIModeAuto, true
	case "direct":
		return URIModeDirect, true
	case "proxied":
		return URIModeProxied, true
	case "off":
		return URIModeOff, true
	}
	return URIModeAuto, false
}

func (m URIMode) String() string {
	switch m {
	case URIModeAuto:
		return "auto"
	case URIModeDirect:
		return "direct"
	case URIModeProxied:
		return "proxied"
	case URIModeOff:
		return "off"
	}
	return "unknown"
}
