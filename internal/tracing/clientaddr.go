// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package tracing

import (
	"context"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
)

// MCPTrustedProxyCIDRsEnv is an optional env var holding a
// comma-separated list of CIDR ranges that the gateway will trust to
// set the X-Forwarded-For (XFF) header.
//
// When the immediate TCP peer (as seen on r.RemoteAddr) falls inside
// one of these CIDRs, [PickClientAddr] takes the *leftmost* entry of
// XFF as the caller's IP. Otherwise we always fall back to the
// immediate peer IP, since an untrusted peer could spoof XFF.
//
// Default (env unset or empty) is the empty set: XFF is never trusted
// and the immediate peer is always used. The gateway today has no
// reverse proxy in front of it on `10.113.24.33`, so leaving this
// unset is correct in production. Operators that put the gateway
// behind an L7 proxy (NGINX / Envoy / Cloudflare) MUST set this to the
// proxy's CIDR or XFF will be ignored.
//
// Phase B TODO: this knob, the entire [PickClientAddr] helper, and the
// `client.address` / `auth.kind` attribute emission in
// [mcpTracer.StartSpanAndInjectMeta] all become advisory / audit-only
// once Okta-verified identity is wired in. See
// `docs/telemetry-rollout-tracker.md` "Phase A vs Phase B" for the
// migration plan.
const MCPTrustedProxyCIDRsEnv = "MCP_TRUSTED_PROXY_CIDRS"

// trustedProxyCIDRs caches the parsed CIDRs from
// [MCPTrustedProxyCIDRsEnv]. Resolved once at first use so per-request
// work stays cheap; env-var changes take effect at process restart,
// the same lifecycle as the rest of the OTel config that
// [NewTracingFromEnv] reads. Tests may reset this via
// [resetTrustedProxyCIDRsForTest].
var (
	trustedProxyCIDRsOnce sync.Once
	trustedProxyCIDRsVal  []*net.IPNet
)

func loadTrustedProxyCIDRs() []*net.IPNet {
	trustedProxyCIDRsOnce.Do(func() {
		trustedProxyCIDRsVal = parseTrustedProxyCIDRs(os.Getenv(MCPTrustedProxyCIDRsEnv))
	})
	return trustedProxyCIDRsVal
}

func parseTrustedProxyCIDRs(raw string) []*net.IPNet {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var out []*net.IPNet
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		// Accept bare IPs as `/32` (IPv4) or `/128` (IPv6) shortcuts so
		// operators can paste a single proxy address without remembering
		// the CIDR suffix.
		if !strings.Contains(entry, "/") {
			if ip := net.ParseIP(entry); ip != nil {
				if ip.To4() != nil {
					entry += "/32"
				} else {
					entry += "/128"
				}
			}
		}
		_, block, err := net.ParseCIDR(entry)
		if err != nil {
			continue
		}
		out = append(out, block)
	}
	return out
}

// PickClientAddr returns a best-effort caller IP for r and a "kind"
// label describing how the value was derived. The kind is recorded
// alongside the address as the `client.address.kind` span attribute in
// [mcpTracer.StartSpanAndInjectMeta] so dashboards can tell trusted-
// proxy XFF and direct-peer addresses apart.
//
// Resolution priority:
//
//  1. If the immediate peer (host portion of r.RemoteAddr) is contained
//     in [MCPTrustedProxyCIDRsEnv] AND X-Forwarded-For is non-empty,
//     return the *leftmost* XFF entry. Returned kind: "xff".
//  2. Else return the host portion of r.RemoteAddr. Returned kind:
//     "remote_addr".
//  3. If neither yields a parseable IP, return ("", "anonymous").
//
// The returned address is the canonical IP-only form: no port, no
// brackets, no IPv6 zone identifier.
//
// Phase B TODO: this helper exists only because the gateway has no
// verified-identity layer yet. Once Okta/JWT validation is wired in,
// prefer the verified principal as `user.id` and demote this address
// to an audit-only attribute -- it should no longer feed `user.id` as
// a fallback. The helper itself can stay (audit value) but the
// `auth.kind=ip` branch in `StartSpanAndInjectMeta` is removed.
// See `docs/telemetry-rollout-tracker.md`.
func PickClientAddr(r *http.Request) (addr, kind string) {
	if r == nil {
		return "", "anonymous"
	}
	peer := remoteAddrIP(r.RemoteAddr)

	if peer != "" && peerIsTrustedProxy(peer) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if leftmost := normalizeIP(firstCSV(xff)); leftmost != "" {
				return leftmost, "xff"
			}
		}
	}
	if peer != "" {
		return peer, "remote_addr"
	}
	return "", "anonymous"
}

func firstCSV(s string) string {
	if i := strings.IndexByte(s, ','); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// remoteAddrIP extracts the IP portion of a host:port string like
// "1.2.3.4:5678" or "[::1]:5678". Returns "" if the input is empty or
// cannot be parsed as an IP. r.RemoteAddr is documented to be in
// host:port form, but in unit tests / unusual transports it may be a
// bare IP, so we tolerate that case as well.
func remoteAddrIP(s string) string {
	if s == "" {
		return ""
	}
	host, _, err := net.SplitHostPort(s)
	if err != nil {
		host = s
	}
	return normalizeIP(host)
}

// normalizeIP returns the canonical IP-only form of v, or "" if v is
// not a valid IP. IPv6 zone identifiers (everything after `%`) and
// stray brackets are stripped.
func normalizeIP(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "[")
	v = strings.TrimSuffix(v, "]")
	if i := strings.IndexByte(v, '%'); i >= 0 {
		v = v[:i]
	}
	if v == "" {
		return ""
	}
	if ip := net.ParseIP(v); ip != nil {
		return ip.String()
	}
	return ""
}

func peerIsTrustedProxy(ipStr string) bool {
	cidrs := loadTrustedProxyCIDRs()
	if len(cidrs) == 0 {
		return false
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, n := range cidrs {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// clientAddrCtxKey is the context key for the captured client address.
// Unexported; access goes through [WithClientAddr] /
// [clientAddrFromContext]. Defined as an empty struct so it cannot
// collide with any other ctx key in the program.
type clientAddrCtxKey struct{}

type clientAddrValue struct {
	addr string
	kind string
}

// WithClientAddr stores the captured client address and its kind on
// ctx so the tracer can read them in
// [mcpTracer.StartSpanAndInjectMeta] without re-parsing the inbound
// HTTP request.
//
// The handler layer (mcpproxy.servePOST) calls this once at the top of
// the request lifecycle. addr "" with kind "anonymous" is a valid
// input and signals "no usable client identity"; in that case the
// tracer emits `auth.kind=anonymous` and skips `client.address`.
//
// Phase B TODO: removed once Okta-derived principal is the trusted
// identity source and `user.id` no longer falls back to "ip:<addr>".
func WithClientAddr(ctx context.Context, addr, kind string) context.Context {
	if ctx == nil {
		return ctx
	}
	if kind == "" {
		kind = "anonymous"
	}
	return context.WithValue(ctx, clientAddrCtxKey{}, clientAddrValue{addr: addr, kind: kind})
}

// clientAddrFromContext returns the captured client address and kind
// for ctx. ok reports whether [WithClientAddr] was ever called on ctx
// (or one of its parents).
func clientAddrFromContext(ctx context.Context) (addr, kind string, ok bool) {
	if ctx == nil {
		return "", "", false
	}
	v, present := ctx.Value(clientAddrCtxKey{}).(clientAddrValue)
	if !present {
		return "", "", false
	}
	return v.addr, v.kind, true
}
