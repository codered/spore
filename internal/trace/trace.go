// Package trace wires spore to OpenTelemetry using OpenInference semantic
// conventions, so Phoenix renders LLM, tool and retriever spans natively.
// Attribute names live here and nowhere else.
package trace

import (
	"context"
	"sync/atomic"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/provider"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
)

type Span = oteltrace.Span

// OpenInference attribute keys.
const (
	attrSpanKind         = "openinference.span.kind"
	attrModelName        = "llm.model_name"
	attrTokensIn         = "llm.token_count.prompt"
	attrTokensOut        = "llm.token_count.completion"
	attrInput            = "input.value"
	attrOutput           = "output.value"
	attrToolName         = "tool.name"
	attrToolParams       = "tool.parameters"
	attrCallSite         = "spore.call_site"
	attrCostUSD          = "spore.cost_usd"
	attrSessionID        = "session.id"
	attrClient           = "spore.client"
	attrPolicyDecision   = "spore.policy.decision"
	attrPolicyRule       = "spore.policy.rule"
	attrToolIsError      = "spore.tool.is_error"
	attrToolResultLen    = "spore.tool.result_bytes"
	attrRetrievalBackend = "spore.recall.backend"
	attrRetrievalK       = "spore.recall.k"
	attrRetrievalHits    = "spore.recall.hits"
)

var redact atomic.Bool

func SetRedact(on bool) { redact.Store(on) }

func tracer() oteltrace.Tracer { return otel.Tracer("github.com/codered/spore") }

// Init installs the global tracer provider. When tracing is disabled it
// returns a no-op shutdown, so callers never branch on configuration.
func Init(ctx context.Context, cfg config.TraceConfig) (func(context.Context) error, error) {
	SetRedact(cfg.Redact)
	if !cfg.Enabled {
		return func(context.Context) error { return nil }, nil
	}
	exp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(cfg.Endpoint))
	if err != nil {
		return nil, err
	}
	res, err := resource.Merge(resource.Default(),
		resource.NewWithAttributes("", attribute.String("service.name", "spore")))
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.TraceIDRatioBased(cfg.SampleRate)),
	)
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

func StartTurn(ctx context.Context, sessionID, client string) (context.Context, Span) {
	return tracer().Start(ctx, "turn", oteltrace.WithAttributes(
		attribute.String(attrSpanKind, "CHAIN"),
		attribute.String(attrSessionID, sessionID),
		attribute.String(attrClient, client),
	))
}

func StartLLM(ctx context.Context, callSite, modelRef string) (context.Context, Span) {
	return tracer().Start(ctx, "llm", oteltrace.WithAttributes(
		attribute.String(attrSpanKind, "LLM"),
		attribute.String(attrModelName, modelRef),
		attribute.String(attrCallSite, callSite),
	))
}

// EndLLM records usage and, unless redacting, the prompt and completion.
func EndLLM(span Span, prompt, completion string, u provider.Usage, cost float64) {
	span.SetAttributes(
		attribute.Int(attrTokensIn, u.InputTokens),
		attribute.Int(attrTokensOut, u.OutputTokens),
		attribute.Float64(attrCostUSD, cost),
	)
	if !redact.Load() {
		span.SetAttributes(
			attribute.String(attrInput, prompt),
			attribute.String(attrOutput, completion),
		)
	}
	span.End()
}

func StartTool(ctx context.Context, name string, args []byte) (context.Context, Span) {
	kv := []attribute.KeyValue{
		attribute.String(attrSpanKind, "TOOL"),
		attribute.String(attrToolName, name),
	}
	if !redact.Load() {
		kv = append(kv, attribute.String(attrToolParams, string(args)))
	}
	return tracer().Start(ctx, "tool "+name, oteltrace.WithAttributes(kv...))
}

// RecordPolicy annotates the current tool span with the decision that let the
// call through — or stopped it. Called from internal/policy, which has no
// other dependency on tracing.
func RecordPolicy(ctx context.Context, decision, rule string) {
	span := oteltrace.SpanFromContext(ctx)
	span.SetAttributes(
		attribute.String(attrPolicyDecision, decision),
		attribute.String(attrPolicyRule, rule),
	)
}

// RecordToolResult records the shape of a tool result. The content itself is
// dropped when redacting, but its size and error flag are always kept.
func RecordToolResult(span Span, content string, isErr, truncated bool) {
	span.SetAttributes(
		attribute.Int(attrToolResultLen, len(content)),
		attribute.Bool(attrToolIsError, isErr),
		attribute.Bool("spore.tool.truncated", truncated),
	)
	if !redact.Load() {
		span.SetAttributes(attribute.String(attrOutput, content))
	}
}

// StartRetriever opens the retriever span Phoenix renders natively. The query
// is prompt-shaped text, so it is dropped when redacting; the shape of the
// search is kept either way.
func StartRetriever(ctx context.Context, backend, query string, k int) (context.Context, Span) {
	kv := []attribute.KeyValue{
		attribute.String(attrSpanKind, "RETRIEVER"),
		attribute.String(attrRetrievalBackend, backend),
		attribute.Int(attrRetrievalK, k),
	}
	if !redact.Load() {
		kv = append(kv, attribute.String(attrInput, query))
	}
	return tracer().Start(ctx, "recall.search", oteltrace.WithAttributes(kv...))
}

// EndRetriever records which documents came back. Ids and scores are index
// metadata rather than content, so they survive redaction.
func EndRetriever(span Span, ids []string, scores []float64) {
	span.SetAttributes(
		attribute.Int(attrRetrievalHits, len(ids)),
		attribute.StringSlice("retrieval.documents.ids", ids),
		attribute.Float64Slice("retrieval.documents.scores", scores),
	)
	span.End()
}
