package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/netip"
	"regexp"
	"strings"

	"heckel.io/ntfy/v2/util"
)

var (
	mimeDecoder mime.WordDecoder

	// priorityHeaderIgnoreRegex matches specific patterns of the "Priority" header (RFC 9218), so that it can be ignored
	priorityHeaderIgnoreRegex = regexp.MustCompile(`^u=\d,\s*(i|\d)$|^u=\d$`)

	// forwardedHeaderRegex parses IPv4 and IPv6 addresses from the "Forwarded" header (RFC 7239)
	// IPv6 addresses in Forwarded header are enclosed in square brackets. The port is optional.
	//
	// Examples:
	//  for="1.2.3.4"
	//  for="[2001:db8::1]"; for=1.2.3.4:8080, by=phil
	//  for="1.2.3.4:8080"
	forwardedHeaderRegex = regexp.MustCompile(`(?i)\bfor="?(\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}|\[[0-9a-f:]+])(?::\d+)?"?`)
)

func readBoolParam(r *http.Request, defaultValue bool, names ...string) bool {
	value := strings.ToLower(readParam(r, names...))
	if value == "" {
		return defaultValue
	}
	return toBool(value)
}

func isBoolValue(value string) bool {
	return value == "1" || value == "yes" || value == "true" || value == "0" || value == "no" || value == "false"
}

func toBool(value string) bool {
	return value == "1" || value == "yes" || value == "true"
}

func readCommaSeparatedParam(r *http.Request, names ...string) []string {
	if paramStr := readParam(r, names...); paramStr != "" {
		return util.Map(util.SplitNoEmpty(paramStr, ","), strings.TrimSpace)
	}
	return []string{}
}

func readParam(r *http.Request, names ...string) string {
	value := readHeaderParam(r, names...)
	if value != "" {
		return value
	}
	return readQueryParam(r, names...)
}

func readHeaderParam(r *http.Request, names ...string) string {
	for _, name := range names {
		value := strings.TrimSpace(maybeDecodeHeader(name, r.Header.Get(name)))
		if value != "" {
			return value
		}
	}
	return ""
}

func readQueryParam(r *http.Request, names ...string) string {
	for _, name := range names {
		value := r.URL.Query().Get(strings.ToLower(name))
		if value != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// extractIPAddress extracts the IP address of the visitor from the request,
// either from the TCP socket or from a proxy header.
func extractIPAddress(r *http.Request, behindProxy bool, proxyForwardedHeader string, proxyTrustedPrefixes []netip.Prefix) netip.Addr {
	if behindProxy && proxyForwardedHeader != "" {
		if addr, err := extractIPAddressFromHeader(r, proxyForwardedHeader, proxyTrustedPrefixes); err == nil {
			return addr
		}
		// Fall back to the remote address if the header is not found or invalid
	}
	addrPort, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		logr(r).Err(err).Warn("unable to parse IP (%s), new visitor with unspecified IP (0.0.0.0) created", r.RemoteAddr)
		return netip.IPv4Unspecified()
	}
	return addrPort.Addr()
}

// extractIPAddressFromHeader extracts the last IP address from the specified header.
//
// It supports multiple formats:
// - single IP address
// - comma-separated list
// - RFC 7239-style list (Forwarded header)
//
// If there are multiple addresses, we first remove the trusted IP addresses from the list, and
// then take the right-most address in the list (as this is the one added by our proxy server).
// See https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/X-Forwarded-For for details.
func extractIPAddressFromHeader(r *http.Request, forwardedHeader string, trustedPrefixes []netip.Prefix) (netip.Addr, error) {
	value := strings.TrimSpace(strings.ToLower(r.Header.Get(forwardedHeader)))
	if value == "" {
		return netip.IPv4Unspecified(), fmt.Errorf("no %s header found", forwardedHeader)
	}
	// Extract valid addresses
	addrsStrs := util.Map(util.SplitNoEmpty(value, ","), strings.TrimSpace)
	var validAddrs []netip.Addr
	for _, addrStr := range addrsStrs {
		// Handle Forwarded header with for="[IPv6]" or for="IPv4"
		if m := forwardedHeaderRegex.FindStringSubmatch(addrStr); len(m) == 2 {
			addrRaw := m[1]
			if strings.HasPrefix(addrRaw, "[") && strings.HasSuffix(addrRaw, "]") {
				addrRaw = addrRaw[1 : len(addrRaw)-1]
			}
			if addr, err := netip.ParseAddr(addrRaw); err == nil {
				validAddrs = append(validAddrs, addr)
			}
		} else if addr, err := netip.ParseAddr(addrStr); err == nil {
			validAddrs = append(validAddrs, addr)
		}
	}
	// Filter out proxy addresses
	clientAddrs := util.Filter(validAddrs, func(addr netip.Addr) bool {
		for _, prefix := range trustedPrefixes {
			if prefix.Contains(addr) {
				return false // Address is in the trusted range, ignore it
			}
		}
		return true
	})
	if len(clientAddrs) == 0 {
		return netip.IPv4Unspecified(), fmt.Errorf("no client IP address found in %s header: %s", forwardedHeader, value)
	}
	return clientAddrs[len(clientAddrs)-1], nil
}

func readJSONWithLimit[T any](r io.ReadCloser, limit int, allowEmpty bool) (*T, error) {
	obj, err := util.UnmarshalJSONWithLimit[T](r, limit, allowEmpty)
	if errors.Is(err, util.ErrUnmarshalJSON) {
		return nil, errHTTPBadRequestJSONInvalid
	} else if errors.Is(err, util.ErrTooLargeJSON) {
		return nil, errHTTPEntityTooLargeJSONBody
	} else if err != nil {
		return nil, err
	}
	return obj, nil
}

func withContext(r *http.Request, ctx map[contextKey]any) *http.Request {
	c := r.Context()
	for k, v := range ctx {
		c = context.WithValue(c, k, v)
	}
	return r.WithContext(c)
}

func fromContext[T any](r *http.Request, key contextKey) (T, error) {
	t, ok := r.Context().Value(key).(T)
	if !ok {
		return t, fmt.Errorf("cannot find key %v in request context", key)
	}
	return t, nil
}

// maybeDecodeHeader decodes the given header value if it is MIME encoded, e.g. "=?utf-8?q?Hello_World?=",
// or returns the original header value if it is not MIME encoded. It also calls maybeIgnoreSpecialHeader
// to ignore the new HTTP "Priority" header.
func maybeDecodeHeader(name, value string) string {
	decoded, err := mimeDecoder.DecodeHeader(value)
	if err != nil {
		return maybeIgnoreSpecialHeader(name, value)
	}
	return maybeIgnoreSpecialHeader(name, decoded)
}

// maybeIgnoreSpecialHeader ignores the new HTTP "Priority" header (RFC 9218, see https://datatracker.ietf.org/doc/html/rfc9218)
//
// Cloudflare (and potentially other providers) add this to requests when forwarding to the backend (ntfy),
// so we just ignore it. If the "Priority" header is set to "u=*, i" or "u=*" (by Cloudflare), the header will be ignored.
// Returning an empty string will allow the rest of the logic to continue searching for another header (x-priority, prio, p),
// or in the Query parameters.
func maybeIgnoreSpecialHeader(name, value string) string {
	if strings.ToLower(name) == "priority" && priorityHeaderIgnoreRegex.MatchString(strings.TrimSpace(value)) {
		return ""
	}
	return value
}
