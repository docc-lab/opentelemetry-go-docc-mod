package trace // import "go.opentelemetry.io/otel/trace"

import (
    "context"
    "io/ioutil"
    "strings"
    // "go.opentelemetry.io/otel/trace"
)

type lastUpstreamParentKey struct{}

// cactiTracer includes logic for filtering tracepoints and maintaining trace continuity
type cactiTracer struct {
    Tracer
    enabledTracepoints map[string]bool
}

// Function to add the most recent enabled span to the context
func setLastUpstreamParent(ctx context.Context, span Span) context.Context {
    return context.WithValue(ctx, lastUpstreamParentKey{}, span)
}

// Function to retrieve the most recent enabled span from the context, if any
func getLastUpstreamParent(ctx context.Context) Span {
    span, ok := ctx.Value(lastUpstreamParentKey{}).(Span)
    if !ok {
        return nil // Or return a default no-op span
    }
    return span
}


// Constructor for cactiTracer
func NewCustomTracer(tracer Tracer, filePath string) *cactiTracer {
    // Read tracepoint list from file for now
    data, err := ioutil.ReadFile(filePath)
    if err != nil {
        panic(err)
    }

    enabled := make(map[string]bool)
    for _, line := range strings.Split(string(data), "\n") {
        if line != "" {
            enabled[line] = true
        }
    }

    return &cactiTracer{
        Tracer:            tracer,
        enabledTracepoints: enabled,
    }
}

// Start method override for cactiTracer
func (ct *cactiTracer) Start(ctx context.Context, spanName string, opts ...SpanStartOption) (context.Context, Span) {
    if ct.enabledTracepoints[spanName] {
        // Tracepoint enabled; proceed with span creation and update context
        ctx, span := ct.Tracer.Start(ctx, spanName, opts...)
        return setLastUpstreamParent(ctx, span), span
    } else {
        // Tracepoint not enabled; attempt to maintain trace continuity if there's an enabled span upstream
        enabledSpan := getLastUpstreamParent(ctx)
        if enabledSpan != nil {
            // use last upstream span to maintain trace continuity
            ctx = ContextWithSpan(ctx, enabledSpan)
        }
        // Return context and no-op span, as this tracepoint isn't enabled
        return ctx, noopSpan{}
    }
}
