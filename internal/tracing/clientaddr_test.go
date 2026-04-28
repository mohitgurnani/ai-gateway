// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package tracing

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// resetTrustedProxyCIDRsForTest re-arms [trustedProxyCIDRsOnce] so that
// the next call to [loadTrustedProxyCIDRs] re-reads
// [MCPTrustedProxyCIDRsEnv] from the environment. Tests that exercise
// trusted-proxy behaviour must call this before and after they mutate
// the env var so neither this package's other tests nor the package
// itself observe stale state.
func resetTrustedProxyCIDRsForTest() {
	trustedProxyCIDRsOnce = sync.Once{}
	trustedProxyCIDRsVal = nil
}

func TestParseTrustedProxyCIDRs(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int // expected number of parsed entries
	}{
		{name: "empty", raw: "", want: 0},
		{name: "whitespace only", raw: "   ", want: 0},
		{name: "single CIDR", raw: "10.0.0.0/8", want: 1},
		{name: "single bare IPv4 -> /32 shortcut", raw: "10.113.24.33", want: 1},
		{name: "single bare IPv6 -> /128 shortcut", raw: "::1", want: 1},
		{name: "multiple", raw: "10.0.0.0/8, 192.168.0.0/16,  172.16.0.0/12", want: 3},
		{name: "garbage entries skipped", raw: "10.0.0.0/8,not-a-cidr,192.168.0.0/16", want: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseTrustedProxyCIDRs(tc.raw)
			require.Len(t, got, tc.want)
		})
	}
}

func TestPickClientAddr(t *testing.T) {
	t.Run("nil request -> anonymous", func(t *testing.T) {
		addr, kind := PickClientAddr(nil)
		require.Empty(t, addr)
		require.Equal(t, "anonymous", kind)
	})

	t.Run("RemoteAddr only -> remote_addr", func(t *testing.T) {
		r := &http.Request{
			RemoteAddr: "10.113.24.55:51234",
			Header:     http.Header{},
		}
		addr, kind := PickClientAddr(r)
		require.Equal(t, "10.113.24.55", addr)
		require.Equal(t, "remote_addr", kind)
	})

	t.Run("IPv6 RemoteAddr -> remote_addr", func(t *testing.T) {
		r := &http.Request{
			RemoteAddr: "[2001:db8::1]:51234",
			Header:     http.Header{},
		}
		addr, kind := PickClientAddr(r)
		require.Equal(t, "2001:db8::1", addr)
		require.Equal(t, "remote_addr", kind)
	})

	t.Run("RemoteAddr without port (test transport) is tolerated", func(t *testing.T) {
		r := &http.Request{
			RemoteAddr: "10.113.24.55",
			Header:     http.Header{},
		}
		addr, kind := PickClientAddr(r)
		require.Equal(t, "10.113.24.55", addr)
		require.Equal(t, "remote_addr", kind)
	})

	t.Run("XFF is ignored when peer is not in trusted-proxy set", func(t *testing.T) {
		// MCP_TRUSTED_PROXY_CIDRS is unset -> empty trust list ->
		// XFF must be ignored.
		resetTrustedProxyCIDRsForTest()
		t.Cleanup(resetTrustedProxyCIDRsForTest)
		t.Setenv(MCPTrustedProxyCIDRsEnv, "")

		r := &http.Request{
			RemoteAddr: "10.113.24.55:51234",
			Header: http.Header{
				"X-Forwarded-For": []string{"203.0.113.7"}, // a fake spoof
			},
		}
		addr, kind := PickClientAddr(r)
		require.Equal(t, "10.113.24.55", addr)
		require.Equal(t, "remote_addr", kind)
	})

	t.Run("XFF leftmost wins when peer is in trusted-proxy set", func(t *testing.T) {
		resetTrustedProxyCIDRsForTest()
		t.Cleanup(resetTrustedProxyCIDRsForTest)
		t.Setenv(MCPTrustedProxyCIDRsEnv, "10.113.24.0/24")

		r := &http.Request{
			RemoteAddr: "10.113.24.55:51234", // inside the trusted CIDR
			Header: http.Header{
				"X-Forwarded-For": []string{"198.51.100.7, 10.113.24.55"},
			},
		}
		addr, kind := PickClientAddr(r)
		require.Equal(t, "198.51.100.7", addr)
		require.Equal(t, "xff", kind)
	})

	t.Run("trusted peer with empty XFF falls back to remote_addr", func(t *testing.T) {
		resetTrustedProxyCIDRsForTest()
		t.Cleanup(resetTrustedProxyCIDRsForTest)
		t.Setenv(MCPTrustedProxyCIDRsEnv, "10.113.24.0/24")

		r := &http.Request{
			RemoteAddr: "10.113.24.55:51234",
			Header:     http.Header{},
		}
		addr, kind := PickClientAddr(r)
		require.Equal(t, "10.113.24.55", addr)
		require.Equal(t, "remote_addr", kind)
	})

	t.Run("bare-IP entry in env is treated as /32", func(t *testing.T) {
		resetTrustedProxyCIDRsForTest()
		t.Cleanup(resetTrustedProxyCIDRsForTest)
		t.Setenv(MCPTrustedProxyCIDRsEnv, "10.113.24.55")

		r := &http.Request{
			RemoteAddr: "10.113.24.55:51234",
			Header: http.Header{
				"X-Forwarded-For": []string{"198.51.100.7"},
			},
		}
		addr, kind := PickClientAddr(r)
		require.Equal(t, "198.51.100.7", addr)
		require.Equal(t, "xff", kind)
	})

	t.Run("XFF with garbage leftmost falls back to remote_addr", func(t *testing.T) {
		resetTrustedProxyCIDRsForTest()
		t.Cleanup(resetTrustedProxyCIDRsForTest)
		t.Setenv(MCPTrustedProxyCIDRsEnv, "10.113.24.0/24")

		r := &http.Request{
			RemoteAddr: "10.113.24.55:51234",
			Header: http.Header{
				"X-Forwarded-For": []string{"not-an-ip, 10.113.24.55"},
			},
		}
		addr, kind := PickClientAddr(r)
		require.Equal(t, "10.113.24.55", addr)
		require.Equal(t, "remote_addr", kind)
	})

	t.Run("XEA wins over XFF when peer is trusted", func(t *testing.T) {
		// Envoy populates X-Envoy-External-Address as a single
		// trusted IP; we prefer it over XFF (which can be a chain
		// from an arbitrary upstream).
		resetTrustedProxyCIDRsForTest()
		t.Cleanup(resetTrustedProxyCIDRsForTest)
		t.Setenv(MCPTrustedProxyCIDRsEnv, "127.0.0.1/32")

		r := &http.Request{
			RemoteAddr: "127.0.0.1:51234",
			Header: http.Header{
				"X-Envoy-External-Address": []string{"10.150.20.30"},
				"X-Forwarded-For":          []string{"203.0.113.7, 10.150.20.30"},
			},
		}
		addr, kind := PickClientAddr(r)
		require.Equal(t, "10.150.20.30", addr)
		require.Equal(t, "xea", kind)
	})

	t.Run("XEA is ignored when peer is NOT trusted", func(t *testing.T) {
		// Even with XEA set, an untrusted peer must not be allowed
		// to spoof the client IP.
		resetTrustedProxyCIDRsForTest()
		t.Cleanup(resetTrustedProxyCIDRsForTest)
		t.Setenv(MCPTrustedProxyCIDRsEnv, "")

		r := &http.Request{
			RemoteAddr: "10.113.24.55:51234",
			Header: http.Header{
				"X-Envoy-External-Address": []string{"10.150.20.30"},
			},
		}
		addr, kind := PickClientAddr(r)
		require.Equal(t, "10.113.24.55", addr)
		require.Equal(t, "remote_addr", kind)
	})

	t.Run("garbage XEA falls through to XFF leftmost", func(t *testing.T) {
		// A malformed XEA value should not block XFF parsing; the
		// resolver moves on to the next-priority source.
		resetTrustedProxyCIDRsForTest()
		t.Cleanup(resetTrustedProxyCIDRsForTest)
		t.Setenv(MCPTrustedProxyCIDRsEnv, "127.0.0.1/32")

		r := &http.Request{
			RemoteAddr: "127.0.0.1:51234",
			Header: http.Header{
				"X-Envoy-External-Address": []string{"not-an-ip"},
				"X-Forwarded-For":          []string{"10.150.20.30, 127.0.0.1"},
			},
		}
		addr, kind := PickClientAddr(r)
		require.Equal(t, "10.150.20.30", addr)
		require.Equal(t, "xff", kind)
	})
}

func TestWithClientAddrRoundtrip(t *testing.T) {
	ctx := WithClientAddr(context.Background(), "10.0.0.1", "remote_addr")
	addr, kind, ok := clientAddrFromContext(ctx)
	require.True(t, ok)
	require.Equal(t, "10.0.0.1", addr)
	require.Equal(t, "remote_addr", kind)
}

func TestWithClientAddr_DefaultsKindToAnonymous(t *testing.T) {
	ctx := WithClientAddr(context.Background(), "", "")
	addr, kind, ok := clientAddrFromContext(ctx)
	require.True(t, ok)
	require.Empty(t, addr)
	require.Equal(t, "anonymous", kind)
}

func TestClientAddrFromContext_Unset(t *testing.T) {
	addr, kind, ok := clientAddrFromContext(context.Background())
	require.False(t, ok)
	require.Empty(t, addr)
	require.Empty(t, kind)
}
