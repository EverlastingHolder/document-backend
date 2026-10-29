package httpapi

import (
	"errors"
	"net"
	"net/netip"
	"strconv"
)

var localClientRanges = [...]netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("fe80::/10"),
}

func normalizeAllowedClientNetworks(networks []netip.Prefix) ([]netip.Prefix, error) {
	if networks == nil {
		return nil, nil
	}
	normalized := make([]netip.Prefix, len(networks))
	for i, prefix := range networks {
		if !prefix.IsValid() || prefix.Addr().Is4In6() {
			return nil, errInvalidAllowedClientNetwork
		}
		prefix = prefix.Masked()
		if !prefixIsLocal(prefix) {
			return nil, errInvalidAllowedClientNetwork
		}
		normalized[i] = prefix
	}
	return normalized, nil
}

var errInvalidAllowedClientNetwork = errors.New("network is not fully within a local address range")

func prefixIsLocal(prefix netip.Prefix) bool {
	for _, localRange := range localClientRanges {
		if prefix.Bits() >= localRange.Bits() && localRange.Contains(prefix.Addr()) {
			return true
		}
	}
	return false
}

func remoteAddrAllowed(remoteAddr string, networks []netip.Prefix) bool {
	if len(networks) == 0 {
		return true
	}
	host, port, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return false
	}
	address, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	if address.Zone() != "" {
		if address.Is4In6() || !address.Is6() || !address.IsLinkLocalUnicast() {
			return false
		}
		address = address.WithZone("")
	}
	if address.Is4In6() {
		address = address.Unmap()
	}
	for _, network := range networks {
		if network.Contains(address) {
			return true
		}
	}
	return false
}
