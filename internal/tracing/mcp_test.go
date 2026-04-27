// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package tracing

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/contrib/propagators/autoprop"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"
)

func TestTracer_StartSpanAndInjectMeta(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := trace.NewTracerProvider(trace.WithSyncer(exporter))

	tracer := newMCPTracer(tp.Tracer("test"), autoprop.NewTextMapPropagator(),
		map[string]string{
			"x-tracing-enrichment-user-region": "user.region",
			"agent-session-id":                 "session.id",
			"CustomAttr":                       "custom.attr",
		})

	headers := make(http.Header)
	headers.Add("X-Tracing-Enrichment-User-Region", "us-east-1")
	headers.Add("Agent-Session-Id", "123") // should be ignored as the value in the metadata takes precedence

	reqID, _ := jsonrpc.MakeID("id")
	r := &jsonrpc.Request{ID: reqID, Method: "initialize"}
	p := &mcp.InitializeParams{Meta: map[string]any{
		"Agent-Session-Id": "sess-4567", // alphabetical order wins when multiple values match case-insensitively
		"agent-session-id": "sess-1234",
		"customattr":       "custom-value1", // exact match should win over case-insensitive match
		"CustomAttr":       "custom-value2",
	}}
	span := tracer.StartSpanAndInjectMeta(t.Context(), r, p, headers)

	require.NotNil(t, span)
	meta := p.GetMeta()
	require.NotNil(t, meta)
	require.NotNil(t, meta["traceparent"])

	// End the span to export it
	span.EndSpan()
	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	actualSpan := spans[0]
	require.Contains(t, actualSpan.Attributes, attribute.String("user.region", "us-east-1"))
	require.Contains(t, actualSpan.Attributes, attribute.String("session.id", "sess-4567"))
	require.Contains(t, actualSpan.Attributes, attribute.String("custom.attr", "custom-value2"))
	require.NotContains(t, actualSpan.Attributes, attribute.String("session.id", "123"))
	require.NotContains(t, actualSpan.Attributes, attribute.String("custom.attr", "custom-value1"))
}

func TestTracer_StartSpanAndInjectMeta_MetaAndHeaderFallback(t *testing.T) {
	cases := []struct {
		name     string
		meta     map[string]any
		headers  http.Header
		expected string
	}{
		{
			name:     "meta only",
			meta:     map[string]any{"agent-session-id": "meta-session"},
			expected: "meta-session",
		},
		{
			name:     "header fallback",
			headers:  http.Header{"Agent-Session-Id": []string{"header-session"}},
			expected: "header-session",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exporter := tracetest.NewInMemoryExporter()
			tp := trace.NewTracerProvider(trace.WithSyncer(exporter))
			tracer := newMCPTracer(tp.Tracer("test"), autoprop.NewTextMapPropagator(),
				map[string]string{"agent-session-id": "session.id"})

			reqID, _ := jsonrpc.MakeID("id")
			r := &jsonrpc.Request{ID: reqID, Method: "initialize"}
			p := &mcp.InitializeParams{Meta: tc.meta}
			span := tracer.StartSpanAndInjectMeta(t.Context(), r, p, tc.headers)
			require.NotNil(t, span)
			span.EndSpan()

			spans := exporter.GetSpans()
			require.Len(t, spans, 1)
			require.Contains(t, spans[0].Attributes, attribute.String("session.id", tc.expected))
		})
	}
}

// TestTracer_StartSpanAndInjectMeta_W3CHeaderContext verifies that a W3C
// traceparent header on the inbound HTTP request seeds the parent span
// context. This is the baseline client contract: clients set traceparent
// over HTTP, the gateway span chains under it.
func TestTracer_StartSpanAndInjectMeta_W3CHeaderContext(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := trace.NewTracerProvider(trace.WithSyncer(exporter))
	tracer := newMCPTracer(tp.Tracer("test"), autoprop.NewTextMapPropagator(), nil)

	const (
		traceID = "0af7651916cd43dd8448eb211c80319c"
		spanID  = "b7ad6b7169203331"
	)
	headers := http.Header{}
	headers.Set("traceparent", "00-"+traceID+"-"+spanID+"-01")

	reqID, _ := jsonrpc.MakeID("id")
	r := &jsonrpc.Request{ID: reqID, Method: "tools/call"}
	p := &mcp.CallToolParams{Name: "fake-tool"}

	span := tracer.StartSpanAndInjectMeta(context.Background(), r, p, headers)
	require.NotNil(t, span)
	span.EndSpan()

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	// Span must chain under the traceparent the client sent, not a fresh trace.
	require.Equal(t, traceID, spans[0].SpanContext.TraceID().String())
	require.Equal(t, spanID, spans[0].Parent.SpanID().String())

	// Gateway-mutated meta must also carry the (new) trace context so the
	// MCP-protocol carrier is usable by downstream MCP hops.
	meta := p.GetMeta()
	require.NotNil(t, meta)
	require.NotEmpty(t, meta["traceparent"])
}

// TestTracer_StartSpanAndInjectMeta_Baggage verifies that well-known W3C
// baggage keys set by clients over HTTP are (a) promoted to span attributes
// under the `mcp.client.*` namespace and (b) re-injected into both the
// MCP _meta map and the HTTP headers on the way out so they reach upstream
// services.
func TestTracer_StartSpanAndInjectMeta_Baggage(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := trace.NewTracerProvider(trace.WithSyncer(exporter))
	tracer := newMCPTracer(tp.Tracer("test"), autoprop.NewTextMapPropagator(), nil)

	headers := http.Header{}
	headers.Set("baggage", "ticket=TICK-42,user=alice%40example.com,role=oncall,opaque=keep-me")

	reqID, _ := jsonrpc.MakeID("id")
	r := &jsonrpc.Request{ID: reqID, Method: "tools/list"}
	p := &mcp.ListToolsParams{}

	span := tracer.StartSpanAndInjectMeta(context.Background(), r, p, headers)
	require.NotNil(t, span)
	span.EndSpan()

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	attrs := spans[0].Attributes
	require.Contains(t, attrs, attribute.String("mcp.client.ticket", "TICK-42"))
	require.Contains(t, attrs, attribute.String("mcp.client.user", "alice@example.com"))
	require.Contains(t, attrs, attribute.String("mcp.client.role", "oncall"))
	// Opaque keys should NOT leak onto span attributes.
	for _, a := range attrs {
		require.NotEqual(t, attribute.Key("mcp.client.opaque"), a.Key)
	}

	// Baggage is propagated back onto headers so the downstream MCP hop
	// carries it. The header must still include the opaque key.
	outHeader := headers.Get("baggage")
	require.Contains(t, outHeader, "ticket=TICK-42")
	require.Contains(t, outHeader, "role=oncall")
	require.Contains(t, outHeader, "opaque=keep-me")
}

// TestTracer_BaggageTruncatedOnSpan verifies that an abusively large
// baggage value is truncated before being copied onto a span attribute,
// bounding OTLP payload size. The full value still propagates on the
// baggage header unchanged because propagation is independent of attribute
// promotion.
func TestTracer_BaggageTruncatedOnSpan(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := trace.NewTracerProvider(trace.WithSyncer(exporter))
	tracer := newMCPTracer(tp.Tracer("test"), autoprop.NewTextMapPropagator(), nil)

	// 512-byte ticket value, 4x the attribute cap. Using an ASCII run so
	// the test asserts on exact byte lengths independent of UTF-8 width.
	big := strings.Repeat("A", 512)
	headers := http.Header{}
	headers.Set("baggage", "ticket="+big)

	reqID, _ := jsonrpc.MakeID("id")
	r := &jsonrpc.Request{ID: reqID, Method: "tools/list"}
	p := &mcp.ListToolsParams{}

	span := tracer.StartSpanAndInjectMeta(context.Background(), r, p, headers)
	require.NotNil(t, span)
	span.EndSpan()

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)

	var got string
	var found bool
	for _, a := range spans[0].Attributes {
		if a.Key == "mcp.client.ticket" {
			got = a.Value.AsString()
			found = true
			break
		}
	}
	require.True(t, found, "expected mcp.client.ticket attribute")
	require.LessOrEqual(t, len(got), maxBaggageAttrValueBytes+len("..."),
		"span attribute value must be capped to bound OTLP payload size")
	require.True(t, strings.HasSuffix(got, "..."),
		"truncated values must carry an ellipsis marker so operators see they were clipped")

	// Propagation is unchanged: the full baggage value still leaves on
	// the outbound baggage header so downstream services see the raw
	// client input.
	require.Contains(t, headers.Get("baggage"), big,
		"outbound baggage header must carry the full client value verbatim")
}

// TestTracer_BaggageEmptyValueSkipped verifies that an empty-valued
// baggage key does NOT create an empty span attribute. Empty attributes
// are impossible to filter on in backends and just pollute the span.
func TestTracer_BaggageEmptyValueSkipped(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := trace.NewTracerProvider(trace.WithSyncer(exporter))
	tracer := newMCPTracer(tp.Tracer("test"), autoprop.NewTextMapPropagator(), nil)

	headers := http.Header{}
	// Client-sent baggage with an empty ticket -- legal per the W3C
	// grammar but useless as a span attribute.
	headers.Set("baggage", "ticket=,role=oncall")

	reqID, _ := jsonrpc.MakeID("id")
	r := &jsonrpc.Request{ID: reqID, Method: "tools/list"}
	p := &mcp.ListToolsParams{}

	span := tracer.StartSpanAndInjectMeta(context.Background(), r, p, headers)
	require.NotNil(t, span)
	span.EndSpan()

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	for _, a := range spans[0].Attributes {
		require.NotEqual(t, attribute.Key("mcp.client.ticket"), a.Key,
			"empty-valued baggage keys must not be promoted to span attributes")
	}
	require.Contains(t, spans[0].Attributes,
		attribute.String("mcp.client.role", "oncall"),
		"non-empty baggage values in the same header must still be promoted")
}

// TestBaggageAttrsFromEnv_OptIn verifies the operator-level opt-in
// behavior of MCP_TRACE_BAGGAGE_ATTRS.
func TestBaggageAttrsFromEnv_OptIn(t *testing.T) {
	t.Run("unset returns defaults", func(t *testing.T) {
		t.Setenv(mcpTraceBaggageAttrsEnv, "")
		_ = os.Unsetenv(mcpTraceBaggageAttrsEnv)
		m := baggageAttrsFromEnv()
		require.Equal(t, "mcp.client.ticket", m["ticket"])
		require.Equal(t, "mcp.client.user", m["user"])
		require.Equal(t, "mcp.client.role", m["role"])
	})

	t.Run("custom keys override defaults", func(t *testing.T) {
		t.Setenv(mcpTraceBaggageAttrsEnv, "ticket, tenant ,ignored-empty= ")
		m := baggageAttrsFromEnv()
		require.Equal(t, "mcp.client.ticket", m["ticket"])
		require.Equal(t, "mcp.client.tenant", m["tenant"])
		// Default keys that were not listed in the override MUST NOT
		// leak through -- the env var is a replace, not a merge, so
		// operators can narrow the surface.
		_, hasUser := m["user"]
		require.False(t, hasUser, "env override must narrow the allow-list, not merge with defaults")
		_, hasRole := m["role"]
		require.False(t, hasRole)
	})

	t.Run("empty string promotes nothing", func(t *testing.T) {
		t.Setenv(mcpTraceBaggageAttrsEnv, "")
		m := baggageAttrsFromEnv()
		require.Empty(t, m, "explicit empty MCP_TRACE_BAGGAGE_ATTRS must disable baggage promotion entirely")
	})
}

// TestTruncateBaggageAttrValue verifies UTF-8-aware truncation so a cap
// that lands inside a multi-byte rune does not produce invalid UTF-8.
func TestTruncateBaggageAttrValue(t *testing.T) {
	t.Run("short value unchanged", func(t *testing.T) {
		require.Equal(t, "hello", truncateBaggageAttrValue("hello"))
	})
	t.Run("oversize ascii truncated with marker", func(t *testing.T) {
		in := strings.Repeat("x", maxBaggageAttrValueBytes+10)
		out := truncateBaggageAttrValue(in)
		require.Equal(t, strings.Repeat("x", maxBaggageAttrValueBytes)+"...", out)
	})
	t.Run("multi-byte runes are not split", func(t *testing.T) {
		// Each "猫" is 3 bytes in UTF-8. 45 runes = 135 bytes, over the
		// 128-byte cap and guaranteed to land mid-rune at byte 128.
		in := strings.Repeat("猫", 45)
		out := truncateBaggageAttrValue(in)
		require.True(t, utf8.ValidString(out),
			"truncation must not split a rune: got %q", out)
		require.True(t, strings.HasSuffix(out, "..."))
	})
}

func Test_getMCPAttributes(t *testing.T) {
	cases := []struct {
		p        mcp.Params
		expected []attribute.KeyValue
	}{
		{
			p: &mcp.InitializeParams{},
		},
		{
			p: &mcp.ListToolsParams{},
		},
		{
			p: &mcp.CallToolParams{
				Name: "fake-tool",
			},
			expected: []attribute.KeyValue{
				attribute.String("mcp.tool.name", "fake-tool"),
				attribute.String("tool.name", "fake-tool"),
			},
		},
		{
			p: &mcp.ListPromptsParams{},
		},
		{
			p: &mcp.GetPromptParams{
				Name: "fake-prompt",
			},
			expected: []attribute.KeyValue{
				attribute.String("mcp.prompt.name", "fake-prompt"),
			},
		},
		{
			p: &mcp.SetLoggingLevelParams{
				Level: "info",
			},
			expected: []attribute.KeyValue{
				attribute.String("mcp.logging.level", "info"),
			},
		},
		{
			p: &mcp.ListResourcesParams{},
		},
		{
			p: &mcp.ReadResourceParams{
				URI: "fake-uri",
			},
			expected: []attribute.KeyValue{
				attribute.String("mcp.resource.uri", "fake-uri"),
				// ReadResource encodes the URI as the request input
				// under the Langfuse / OpenInference dual keys so the
				// resource being read shows up in the trace's "Input"
				// panel. See getMCPParamsAsAttributes.
				attribute.String("langfuse.observation.input", `{"uri":"fake-uri"}`),
				attribute.String("input.value", `{"uri":"fake-uri"}`),
				attribute.String("input.mime_type", "application/json"),
			},
		},
		{
			p: &mcp.ListResourceTemplatesParams{},
		},
		{
			p: &mcp.SubscribeParams{
				URI: "fake-uri",
			},
			expected: []attribute.KeyValue{
				attribute.String("mcp.resource.uri", "fake-uri"),
			},
		},
		{
			p: &mcp.UnsubscribeParams{
				URI: "fake-uri",
			},
			expected: []attribute.KeyValue{
				attribute.String("mcp.resource.uri", "fake-uri"),
			},
		},
		{
			p: &mcp.ProgressNotificationParams{
				Message:       "fake-message",
				Progress:      100,
				ProgressToken: "fake-token",
			},
			expected: []attribute.KeyValue{
				attribute.Float64("mcp.notifications.progress", 100),
				attribute.String("mcp.notifications.progress.token", "fake-token"),
				attribute.String("mcp.notifications.progress.message", "fake-message"),
			},
		},
		{
			p: &mcp.CompleteParams{
				Argument: mcp.CompleteParamsArgument{
					Name:  "fake-name",
					Value: "fake-value",
				},
			},
			expected: []attribute.KeyValue{
				attribute.String("mcp.complete.argument.name", "fake-name"),
				attribute.String("mcp.complete.argument.value", "fake-value"),
			},
		},
		// CallTool with arguments: arguments must be promoted to the
		// Langfuse / OpenInference input keys so the trace UI's Input
		// panel renders the actual arguments instead of being empty.
		// json.Marshal on a *RawMessage emits the underlying JSON, so
		// we use a RawMessage-shaped argument map for the canonical
		// JSON ordering the proxy will see at runtime.
		{
			p: &mcp.CallToolParams{
				Name:      "fake-tool",
				Arguments: map[string]any{"a": 1, "b": "x"},
			},
			expected: []attribute.KeyValue{
				attribute.String("mcp.tool.name", "fake-tool"),
				attribute.String("tool.name", "fake-tool"),
				attribute.String("langfuse.observation.input", `{"a":1,"b":"x"}`),
				attribute.String("input.value", `{"a":1,"b":"x"}`),
				attribute.String("input.mime_type", "application/json"),
			},
		},
		// GetPrompt with arguments: same dual-emit contract as CallTool.
		{
			p: &mcp.GetPromptParams{
				Name:      "fake-prompt",
				Arguments: map[string]string{"k": "v"},
			},
			expected: []attribute.KeyValue{
				attribute.String("mcp.prompt.name", "fake-prompt"),
				attribute.String("langfuse.observation.input", `{"k":"v"}`),
				attribute.String("input.value", `{"k":"v"}`),
				attribute.String("input.mime_type", "application/json"),
			},
		},
	}

	for _, tc := range cases {
		t.Run("", func(t *testing.T) {
			require.Equal(t, tc.expected, getMCPParamsAsAttributes(tc.p))
		})
	}
}

func Test_getSpanName(t *testing.T) {
	tests := []struct {
		method   string
		expected string
	}{
		{method: "initialize", expected: "Initialize"},
		{method: "tools/list", expected: "ListTools"},
		{method: "tools/call", expected: "CallTool"},
		{method: "prompts/list", expected: "ListPrompts"},
		{method: "prompts/get", expected: "GetPrompt"},
		{method: "resources/list", expected: "ListResources"},
		{method: "resources/read", expected: "ReadResource"},
		{method: "resources/subscribe", expected: "Subscribe"},
		{method: "resources/unsubscribe", expected: "Unsubscribe"},
		{method: "resources/templates/list", expected: "ListResourceTemplates"},
		{method: "logging/setLevel", expected: "SetLoggingLevel"},
		{method: "completion/complete", expected: "Complete"},
		{method: "ping", expected: "Ping"},
	}

	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			actual := getSpanName(tt.method)
			require.Equal(t, tt.expected, actual)
		})
	}
}

func TestMCPTracer_SpanName(t *testing.T) {
	tests := []struct {
		name             string
		method           string
		params           mcp.Params
		expectedSpanName string
	}{
		{
			name:             "tools/list",
			method:           "tools/list",
			params:           &mcp.ListToolsParams{},
			expectedSpanName: "ListTools",
		},
		{
			name:             "tools/call",
			method:           "tools/call",
			params:           &mcp.CallToolParams{Name: "test-tool"},
			expectedSpanName: "CallTool",
		},
		{
			name:             "prompts/list",
			method:           "prompts/list",
			params:           &mcp.ListPromptsParams{},
			expectedSpanName: "ListPrompts",
		},
		{
			name:             "prompts/get",
			method:           "prompts/get",
			params:           &mcp.GetPromptParams{Name: "test-prompt"},
			expectedSpanName: "GetPrompt",
		},
		{
			name:             "resources/list",
			method:           "resources/list",
			params:           &mcp.ListResourcesParams{},
			expectedSpanName: "ListResources",
		},
		{
			name:             "resources/read",
			method:           "resources/read",
			params:           &mcp.ReadResourceParams{URI: "test://uri"},
			expectedSpanName: "ReadResource",
		},
		{
			name:             "initialize",
			method:           "initialize",
			params:           &mcp.InitializeParams{},
			expectedSpanName: "Initialize",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exporter := tracetest.NewInMemoryExporter()
			tp := trace.NewTracerProvider(trace.WithSyncer(exporter))

			tracer := newMCPTracer(tp.Tracer("test"), autoprop.NewTextMapPropagator(), nil)

			reqID, _ := jsonrpc.MakeID("test-id")
			req := &jsonrpc.Request{ID: reqID, Method: tt.method}

			span := tracer.StartSpanAndInjectMeta(context.Background(), req, tt.params, nil)
			require.NotNil(t, span)
			span.EndSpan()

			spans := exporter.GetSpans()
			require.Len(t, spans, 1)
			actualSpan := spans[0]

			require.Equal(t, tt.expectedSpanName, actualSpan.Name)
			require.Equal(t, oteltrace.SpanKindClient, actualSpan.SpanKind)
		})
	}
}

// TestTracer_StartSpanAndInjectMeta_PromotesUserToLangfuse asserts that a
// `user` baggage member is surfaced both under the existing `mcp.client.user`
// attribute (back-compat for non-Langfuse consumers) and under the
// Langfuse-native `user.id` attribute, which Langfuse indexes as the
// dashboard "User ID" dimension. Both attributes must carry the same value.
func TestTracer_StartSpanAndInjectMeta_PromotesUserToLangfuse(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := trace.NewTracerProvider(trace.WithSyncer(exporter))
	tracer := newMCPTracer(tp.Tracer("test"), autoprop.NewTextMapPropagator(), nil)

	headers := http.Header{}
	headers.Set("baggage", "user=alice%40example.com")

	reqID, _ := jsonrpc.MakeID("id")
	r := &jsonrpc.Request{ID: reqID, Method: "tools/list"}
	p := &mcp.ListToolsParams{}

	span := tracer.StartSpanAndInjectMeta(context.Background(), r, p, headers)
	require.NotNil(t, span)
	span.EndSpan()

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	attrs := spans[0].Attributes
	require.Contains(t, attrs, attribute.String("mcp.client.user", "alice@example.com"))
	require.Contains(t, attrs, attribute.String("user.id", "alice@example.com"))
}

// TestTracer_StartSpanAndInjectMeta_PromotesTicketAndRoleToTags asserts
// that `ticket` and `role` baggage members are surfaced as entries in the
// Langfuse-native `langfuse.tags` slice attribute (Langfuse indexes that
// list as the dashboard "Tags" dimension, which supports group-by). The
// existing `mcp.client.*` attributes must remain so non-Langfuse queries
// keep working.
func TestTracer_StartSpanAndInjectMeta_PromotesTicketAndRoleToTags(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := trace.NewTracerProvider(trace.WithSyncer(exporter))
	tracer := newMCPTracer(tp.Tracer("test"), autoprop.NewTextMapPropagator(), nil)

	headers := http.Header{}
	headers.Set("baggage", "ticket=ENG-1,role=oncall,user=alice")

	reqID, _ := jsonrpc.MakeID("id")
	r := &jsonrpc.Request{ID: reqID, Method: "tools/list"}
	p := &mcp.ListToolsParams{}

	span := tracer.StartSpanAndInjectMeta(context.Background(), r, p, headers)
	require.NotNil(t, span)
	span.EndSpan()

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	attrs := spans[0].Attributes

	// mcp.client.* contract preserved.
	require.Contains(t, attrs, attribute.String("mcp.client.ticket", "ENG-1"))
	require.Contains(t, attrs, attribute.String("mcp.client.role", "oncall"))

	// Find langfuse.tags and assert ticket: and role: entries are present.
	// Order-insensitive because we iterate over the baggage map.
	var tags []string
	var found bool
	for _, a := range attrs {
		if a.Key == "langfuse.tags" {
			tags = a.Value.AsStringSlice()
			found = true
			break
		}
	}
	require.True(t, found, "expected langfuse.tags attribute on span")
	require.Contains(t, tags, "ticket:ENG-1")
	require.Contains(t, tags, "role:oncall")
	// `user` must NOT leak into tags -- it has its own native dimension.
	for _, tag := range tags {
		require.False(t, strings.HasPrefix(tag, "user:"),
			"user baggage must be promoted to user.id, not langfuse.tags")
	}
}

// TestTracer_StartSpanAndInjectMeta_TruncatesOversizeBeforePromotion
// asserts that the [maxBaggageAttrValueBytes] cap applies BEFORE the
// Langfuse-native promotions, so a misbehaving client cannot inflate the
// `user.id` or `langfuse.tags` payloads any more than `mcp.client.*`.
func TestTracer_StartSpanAndInjectMeta_TruncatesOversizeBeforePromotion(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := trace.NewTracerProvider(trace.WithSyncer(exporter))
	tracer := newMCPTracer(tp.Tracer("test"), autoprop.NewTextMapPropagator(), nil)

	big := strings.Repeat("u", 512)
	headers := http.Header{}
	headers.Set("baggage", "user="+big+",ticket="+big)

	reqID, _ := jsonrpc.MakeID("id")
	r := &jsonrpc.Request{ID: reqID, Method: "tools/list"}
	p := &mcp.ListToolsParams{}

	span := tracer.StartSpanAndInjectMeta(context.Background(), r, p, headers)
	require.NotNil(t, span)
	span.EndSpan()

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	attrs := spans[0].Attributes

	maxLen := maxBaggageAttrValueBytes + len("...")
	for _, a := range attrs {
		switch a.Key {
		case "user.id", "mcp.client.user", "mcp.client.ticket":
			require.LessOrEqual(t, len(a.Value.AsString()), maxLen,
				"%s must be truncated to %d bytes", a.Key, maxLen)
			require.True(t, strings.HasSuffix(a.Value.AsString(), "..."),
				"%s must carry an ellipsis marker after truncation", a.Key)
		case "langfuse.tags":
			for _, tag := range a.Value.AsStringSlice() {
				if strings.HasPrefix(tag, "ticket:") {
					require.LessOrEqual(t, len(tag), maxLen+len("ticket:"),
						"langfuse.tags ticket entry must use the truncated value")
					require.True(t, strings.HasSuffix(tag, "..."),
						"truncated ticket tag must carry the ellipsis marker")
				}
			}
		}
	}
}

// TestTracer_StartSpanAndInjectMeta_AuthKindBaggage asserts that when a
// client sets baggage `user`, the span carries `auth.kind=baggage` and
// does NOT emit a `client.address` (the request had no peer address in
// this unit test) or an `ip:*` user.id fallback. This is the
// "trusted-identity" case.
//
// Phase B TODO: this test stays valid after Okta rollout, but
// `auth.kind=baggage` becomes the legacy fallback once `auth.kind=okta`
// becomes the trusted-identity case. See
// `docs/telemetry-rollout-tracker.md`.
func TestTracer_StartSpanAndInjectMeta_AuthKindBaggage(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := trace.NewTracerProvider(trace.WithSyncer(exporter))
	tracer := newMCPTracer(tp.Tracer("test"), autoprop.NewTextMapPropagator(), nil)

	headers := http.Header{}
	headers.Set("baggage", "user=alice%40example.com")

	reqID, _ := jsonrpc.MakeID("id")
	r := &jsonrpc.Request{ID: reqID, Method: "tools/list"}
	p := &mcp.ListToolsParams{}

	span := tracer.StartSpanAndInjectMeta(context.Background(), r, p, headers)
	require.NotNil(t, span)
	span.EndSpan()

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	attrs := spans[0].Attributes
	require.Contains(t, attrs, attribute.String("auth.kind", "baggage"))
	require.Contains(t, attrs, attribute.String("user.id", "alice@example.com"))
	for _, a := range attrs {
		require.NotEqual(t, attribute.Key("client.address"), a.Key,
			"client.address must not be set when context has no client addr")
		// user.id must be exactly the baggage value, never the
		// "ip:<addr>" fallback.
		if a.Key == "user.id" {
			require.False(t, strings.HasPrefix(a.Value.AsString(), "ip:"),
				"user.id must come from baggage, not the IP fallback")
		}
	}
}

// TestTracer_StartSpanAndInjectMeta_AuthKindIP asserts that when no
// baggage `user` is set but the request context carries a captured
// client address, the span:
//   - carries `auth.kind=ip`,
//   - falls user.id back to "ip:<addr>" so per-user dashboards still
//     bucket the call,
//   - emits `client.address` and `client.address.kind`.
//
// Phase B TODO: deleted once Okta-derived identity is the trusted source
// for user.id. See `docs/telemetry-rollout-tracker.md` "Phase A vs Phase B".
func TestTracer_StartSpanAndInjectMeta_AuthKindIP(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := trace.NewTracerProvider(trace.WithSyncer(exporter))
	tracer := newMCPTracer(tp.Tracer("test"), autoprop.NewTextMapPropagator(), nil)

	ctx := WithClientAddr(context.Background(), "10.113.24.55", "remote_addr")

	reqID, _ := jsonrpc.MakeID("id")
	r := &jsonrpc.Request{ID: reqID, Method: "tools/list"}
	p := &mcp.ListToolsParams{}

	span := tracer.StartSpanAndInjectMeta(ctx, r, p, http.Header{})
	require.NotNil(t, span)
	span.EndSpan()

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	attrs := spans[0].Attributes
	require.Contains(t, attrs, attribute.String("auth.kind", "ip"))
	require.Contains(t, attrs, attribute.String("user.id", "ip:10.113.24.55"))
	require.Contains(t, attrs, attribute.String("client.address", "10.113.24.55"))
	require.Contains(t, attrs, attribute.String("client.address.kind", "remote_addr"))
}

// TestTracer_StartSpanAndInjectMeta_AuthKindAnonymous asserts that when
// neither baggage `user` nor a captured client address is available, the
// span carries `auth.kind=anonymous`, no `user.id`, and no
// `client.address`. Anonymous spans must still be exportable -- this
// test guards against regressions where a missing identity throws or
// emits empty-string attributes.
func TestTracer_StartSpanAndInjectMeta_AuthKindAnonymous(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := trace.NewTracerProvider(trace.WithSyncer(exporter))
	tracer := newMCPTracer(tp.Tracer("test"), autoprop.NewTextMapPropagator(), nil)

	reqID, _ := jsonrpc.MakeID("id")
	r := &jsonrpc.Request{ID: reqID, Method: "tools/list"}
	p := &mcp.ListToolsParams{}

	span := tracer.StartSpanAndInjectMeta(context.Background(), r, p, http.Header{})
	require.NotNil(t, span)
	span.EndSpan()

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	attrs := spans[0].Attributes
	require.Contains(t, attrs, attribute.String("auth.kind", "anonymous"))
	for _, a := range attrs {
		require.NotEqual(t, attribute.Key("user.id"), a.Key,
			"anonymous spans must not emit user.id at all")
		require.NotEqual(t, attribute.Key("client.address"), a.Key)
		require.NotEqual(t, attribute.Key("client.address.kind"), a.Key)
	}
}

// TestTracer_StartSpanAndInjectMeta_BaggageBeatsIP asserts the precedence
// rule: if both baggage `user` AND a captured client address are
// present, the span's `auth.kind` is "baggage" (the trusted source) and
// `user.id` is the baggage value, not the IP. The IP is still recorded
// as `client.address` for audit so dashboards can break down a single
// baggage user across source IPs.
func TestTracer_StartSpanAndInjectMeta_BaggageBeatsIP(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := trace.NewTracerProvider(trace.WithSyncer(exporter))
	tracer := newMCPTracer(tp.Tracer("test"), autoprop.NewTextMapPropagator(), nil)

	ctx := WithClientAddr(context.Background(), "10.113.24.55", "remote_addr")
	headers := http.Header{}
	headers.Set("baggage", "user=alice%40example.com")

	reqID, _ := jsonrpc.MakeID("id")
	r := &jsonrpc.Request{ID: reqID, Method: "tools/list"}
	p := &mcp.ListToolsParams{}

	span := tracer.StartSpanAndInjectMeta(ctx, r, p, headers)
	require.NotNil(t, span)
	span.EndSpan()

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	attrs := spans[0].Attributes
	require.Contains(t, attrs, attribute.String("auth.kind", "baggage"))
	require.Contains(t, attrs, attribute.String("user.id", "alice@example.com"))
	// client.address still recorded for audit, even when baggage wins.
	require.Contains(t, attrs, attribute.String("client.address", "10.113.24.55"))

	// Specifically, user.id must NOT have been overwritten with the IP
	// fallback. Walking attrs because there can only be one user.id.
	for _, a := range attrs {
		if a.Key == "user.id" {
			require.Equal(t, "alice@example.com", a.Value.AsString(),
				"baggage user must win over IP fallback for user.id")
		}
	}
}

// TestRecordRouteToBackend_SetsBackendAttribute asserts that the
// backend name written by [mcpSpan.RecordRouteToBackend] is surfaced as a
// queryable span attribute (`mcp.backend.name`) and as a `langfuse.tags`
// entry (`backend:<name>`) -- not just as a span event. Span events are
// not indexed by Langfuse dashboards, so without this promotion the
// backend would be invisible to dashboard group-bys.
func TestRecordRouteToBackend_SetsBackendAttribute(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := trace.NewTracerProvider(trace.WithSyncer(exporter))
	tracer := newMCPTracer(tp.Tracer("test"), autoprop.NewTextMapPropagator(), nil)

	headers := http.Header{}
	headers.Set("baggage", "ticket=ENG-1")

	reqID, _ := jsonrpc.MakeID("id")
	r := &jsonrpc.Request{ID: reqID, Method: "tools/call"}
	p := &mcp.CallToolParams{Name: "nurag.query"}

	span := tracer.StartSpanAndInjectMeta(context.Background(), r, p, headers)
	require.NotNil(t, span)
	span.RecordRouteToBackend("nurag", "sess-abc", true)
	span.EndSpan()

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	attrs := spans[0].Attributes

	// Backend appears as a top-level attribute (filterable in any OTLP
	// backend) and inside langfuse.tags (groupable in Langfuse dashboards).
	require.Contains(t, attrs, attribute.String("mcp.backend.name", "nurag"))

	var tags []string
	for _, a := range attrs {
		if a.Key == "langfuse.tags" {
			tags = a.Value.AsStringSlice()
			break
		}
	}
	require.Contains(t, tags, "backend:nurag")
	// Earlier tags from baggage promotion must NOT be lost when
	// RecordRouteToBackend re-sets langfuse.tags. This catches the
	// "SetAttributes overwrites a key" footgun.
	require.Contains(t, tags, "ticket:ENG-1")

	// The existing span event contract is preserved for legacy consumers.
	require.Len(t, spans[0].Events, 1)
	require.Equal(t, "route to backend", spans[0].Events[0].Name)
}
