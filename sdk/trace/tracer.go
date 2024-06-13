// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package trace // import "go.opentelemetry.io/otel/sdk/trace"

import (
	"context"
	"time"
	"encoding/json"
	"fmt"
	"net"

	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/embedded"
	"go.opentelemetry.io/otel/baggage"
)

type tracer struct {
	embedded.Tracer

	provider             *TracerProvider
	instrumentationScope instrumentation.Scope
}

var _ trace.Tracer = &tracer{}

// docclabTracer includes logic for filtering tracepoints and maintaining trace continuity
type docclabTracer struct {
    tracer
    enabledTracepoints map[string]bool
}

type PacketData struct {
    Enabled   []string   `json:"Enabled"`
    Disabled  []string   `json:"Disabled"`
}

// Start starts a Span and returns it along with a context containing it.
//
// The Span is created with the provided name and as a child of any existing
// span context found in the passed context. The created Span will be
// configured appropriately by any SpanOption passed.
func (tr *tracer) Start(ctx context.Context, name string, options ...trace.SpanStartOption) (context.Context, trace.Span) {
	config := trace.NewSpanStartConfig(options...)

	if ctx == nil {
		// Prevent trace.ContextWithSpan from panicking.
		ctx = context.Background()
	}

	// For local spans created by this SDK, track child span count.
	if p := trace.SpanFromContext(ctx); p != nil {
		if sdkSpan, ok := p.(*recordingSpan); ok {
			sdkSpan.addChild()
		}
	}

	s := tr.newSpan(ctx, name, &config)
	if rw, ok := s.(ReadWriteSpan); ok && s.IsRecording() {
		sps := tr.provider.getSpanProcessors()
		for _, sp := range sps {
			sp.sp.OnStart(ctx, rw)
		}
	}
	if rtt, ok := s.(runtimeTracer); ok {
		ctx = rtt.runtimeTrace(ctx)
	}

	return trace.ContextWithSpan(ctx, s), s
}

func (dt *docclabTracer) docclabStart(ctx context.Context, serviceName string, options ...trace.SpanStartOption) (context.Context, trace.Span) {
	if dt.IsTracepointEnabled(serviceName) {
		// Tracepoint enabled; proceed with span creation and update baggage
		ctx, span := dt.Start(ctx, serviceName, options...)
		return dt.setLastUpstreamParent(ctx, span), span
	} else {
		// Tracepoint not enabled; attempt to maintain trace continuity if there's an enabled span upstream
		enabledSpan := dt.getLastUpstreamParent(ctx)
		if enabledSpan.IsValid() {
			// Use last upstream span to maintain trace continuity
			ctx = trace.ContextWithSpanContext(ctx, enabledSpan)
		}
		// Return context and no-op span, as this tracepoint isn't enabled
		return ctx, trace.NoopSpan{}
	}
}

// Add the most recent enabled span to the baggage
func (dt *docclabTracer) setLastUpstreamParent(ctx context.Context, span trace.Span) context.Context {
    spanContext := span.SpanContext()
    member, _ := baggage.NewMember("lastUpstreamParentKey", spanContext.SpanID().String())
    bag := baggage.FromContext(ctx).WithMember(member)
    return baggage.ContextWithBaggage(ctx, bag)
}

// Retrieve the most recent enabled span from the baggage
func (dt *docclabTracer) getLastUpstreamParent(ctx context.Context) trace.SpanContext {
    bag := baggage.FromContext(ctx)
    spanIDHex := bag.Member("lastUpstreamParentKey").Value()
    spanID, _ := trace.SpanIDFromHex(spanIDHex)
    traceID := trace.SpanContextFromContext(ctx).TraceID()
    if spanID.IsValid() {
        return trace.NewSpanContext(trace.SpanContextConfig{
            TraceID: traceID,
            SpanID:  spanID,
        })
    }
    return trace.SpanContext{}
}

//update enabled tracepoint list in docclabTracer struct
func (dt *docclabTracer) UpdateTracepoints(serverPath string) error {
    dt.mu.Lock()
    defer dt.mu.Unlock()

    // Read tracepoint decision from TCP connection
    data, err := dt.getTracepointList(serverPath)
    if err != nil {
        return fmt.Errorf("failed to get tracepoint list: %w", err)
    }

    // Update hashmap correspondingly
    for _, tp := range data.Enabled {
        dt.enabledTracepoints[tp] = true
    }
    for _, tp := range data.Disabled {
        dt.enabledTracepoints[tp] = false
    }

    return nil
}

// Function to receive tracepoint enabling list from remote TCP server
func (dt *docclabTracer) getTracepointList(address string) (*PacketData, error) {
    // Setup listener for tcp server
    listener, err := net.Listen("tcp", address)
    if err != nil {
        fmt.Println("Error setting up TCP listener", err.Error())
        return nil, err
    }
    defer listener.Close()

    // Establish connection
    conn, err := listener.Accept()
    if err != nil {
        fmt.Println("Error accepting connection", err.Error())
        return nil, err
    }

    // Set channel and get a goroutine to asyncly handle packet processing
    payload := make(chan PacketData)
    errChan := make(chan error)
    go handlePacket(conn, payload, errChan)
    
    select {
    case data := <-payload:
        return &data, nil // Successfully received data
    case err := <-errChan:
        return nil, err // Received an error
    }
}

// Function to handle TCP packet, return to a channle of string
func (dt *docclabTracer) handlePacket(conn net.Conn, payload chan<- PacketData, errChan chan<- error) {
    defer conn.Close()

    // Read json from packet, and decode
    decoder := json.NewDecoder(conn)
    var data PacketData
    err := decoder.Decode(&data)
    if err != nil {
        fmt.Println("Error decoding JSON", err.Error())
        errChan <- err
        return
    }
    // Send the received message to the payload channel
    payload <- data

    // conn.Write([]byte("Tracepoint list received.\n"))
}

type runtimeTracer interface {
	// runtimeTrace starts a "runtime/trace".Task for the span and
	// returns a context containing the task.
	runtimeTrace(ctx context.Context) context.Context
}

// newSpan returns a new configured span.
func (tr *tracer) newSpan(ctx context.Context, name string, config *trace.SpanConfig) trace.Span {
	// If told explicitly to make this a new root use a zero value SpanContext
	// as a parent which contains an invalid trace ID and is not remote.
	var psc trace.SpanContext
	if config.NewRoot() {
		ctx = trace.ContextWithSpanContext(ctx, psc)
	} else {
		psc = trace.SpanContextFromContext(ctx)
	}

	// If there is a valid parent trace ID, use it to ensure the continuity of
	// the trace. Always generate a new span ID so other components can rely
	// on a unique span ID, even if the Span is non-recording.
	var tid trace.TraceID
	var sid trace.SpanID
	if !psc.TraceID().IsValid() {
		tid, sid = tr.provider.idGenerator.NewIDs(ctx)
	} else {
		tid = psc.TraceID()
		sid = tr.provider.idGenerator.NewSpanID(ctx, tid)
	}

	samplingResult := tr.provider.sampler.ShouldSample(SamplingParameters{
		ParentContext: ctx,
		TraceID:       tid,
		Name:          name,
		Kind:          config.SpanKind(),
		Attributes:    config.Attributes(),
		Links:         config.Links(),
	})

	scc := trace.SpanContextConfig{
		TraceID:    tid,
		SpanID:     sid,
		TraceState: samplingResult.Tracestate,
	}
	if isSampled(samplingResult) {
		scc.TraceFlags = psc.TraceFlags() | trace.FlagsSampled
	} else {
		scc.TraceFlags = psc.TraceFlags() &^ trace.FlagsSampled
	}
	sc := trace.NewSpanContext(scc)

	if !isRecording(samplingResult) {
		return tr.newNonRecordingSpan(sc)
	}
	return tr.newRecordingSpan(psc, sc, name, samplingResult, config)
}

// newRecordingSpan returns a new configured recordingSpan.
func (tr *tracer) newRecordingSpan(psc, sc trace.SpanContext, name string, sr SamplingResult, config *trace.SpanConfig) *recordingSpan {
	startTime := config.Timestamp()
	if startTime.IsZero() {
		startTime = time.Now()
	}

	s := &recordingSpan{
		// Do not pre-allocate the attributes slice here! Doing so will
		// allocate memory that is likely never going to be used, or if used,
		// will be over-sized. The default Go compiler has been tested to
		// dynamically allocate needed space very well. Benchmarking has shown
		// it to be more performant than what we can predetermine here,
		// especially for the common use case of few to no added
		// attributes.

		parent:      psc,
		spanContext: sc,
		spanKind:    trace.ValidateSpanKind(config.SpanKind()),
		name:        name,
		startTime:   startTime,
		events:      newEvictedQueue(tr.provider.spanLimits.EventCountLimit),
		links:       newEvictedQueue(tr.provider.spanLimits.LinkCountLimit),
		tracer:      tr,
	}

	for _, l := range config.Links() {
		s.addLink(l)
	}

	s.SetAttributes(sr.Attributes...)
	s.SetAttributes(config.Attributes()...)

	return s
}

// newNonRecordingSpan returns a new configured nonRecordingSpan.
func (tr *tracer) newNonRecordingSpan(sc trace.SpanContext) nonRecordingSpan {
	return nonRecordingSpan{tracer: tr, sc: sc}
}
