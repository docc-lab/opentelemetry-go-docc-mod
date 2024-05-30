package trace // import "go.opentelemetry.io/otel/trace"

import (
    "context"
    "fmt"
    "encoding/json"
    "net"
    "os"
    // "go.opentelemetry.io/otel/trace"
)

type lastUpstreamParentKey struct{}

// cactiTracer includes logic for filtering tracepoints and maintaining trace continuity
type cactiTracer struct {
    Tracer
    enabledTracepoints map[string]bool
}

type PacketData struct {
    Enabled   []string   `json:"Enabled"`
    Disabled  []string   `json:"Disabled"`
}


// Function to add the most recent enabled span to the context
func setLastUpstreamParent(ctx context.Context, span Span) context.Context {
    return context.WithValue(ctx, lastUpstreamParentKey{}, span)
}

// Function to retrieve the most recent enabled span from the context, if any
func getLastUpstreamParent(ctx context.Context) Span {
    if span, ok := ctx.Value(lastUpstreamParentKey{}).(Span); ok {
        return span
    }
    return nil
}

// Function to handle TCP packet, return to a channle of string
func handlePacket(conn net.Conn, payload chan<- PacketData, errChan chan<- error) {
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

// Function to receive tracepoint enabling list from remote TCP server
func (c *cactiTracer) getTracepointList(address string) (*PacketData, error) {
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

// Constructor for cactiTracer
func doccLabTracer(tracer Tracer, serverPath string) *cactiTracer {
    // Init hashmap in cactiTracer
    c := &cactiTracer{
        Tracer:             tracer,
        enabledTracepoints: make(map[string]bool),
    }

    // Read tracpoint decision from TCP connection
    data, err := c.getTracepointList(serverPath)
    if err != nil {
        fmt.Println("Failed to get tracepoint list: %s\n", err)
        os.Exit(1)
    }

    // Update hashmap correspondingly
    for _, tp := range data.Enabled {
        c.enabledTracepoints[tp] = true
    }
    for _, tp := range data.Disabled {
        c.enabledTracepoints[tp] = false
    }

    return c
}

// Start method override for cactiTracer
func (ct *cactiTracer) Start(ctx context.Context, serviceName string, opts ...SpanStartOption) (context.Context, Span) {
    if ct.enabledTracepoints[serviceName] {
        // Tracepoint enabled; proceed with span creation and update context
        ctx, span := ct.Tracer.Start(ctx, serviceName, opts...)
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
