package state

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	rpc "github.com/telepresenceio/telepresence/rpc/v2/manager"
	"github.com/telepresenceio/telepresence/v2/pkg/tracing"
	"github.com/telepresenceio/telepresence/v2/pkg/tunnel"
)

const AgentSessionIDPrefix = "agent:"

type SessionState interface {
	Cancel()
	AwaitingBidiMapOwnerSessionID(stream tunnel.Stream) string
	Done() <-chan struct{}
	LastMarked() time.Time
	SetLastMarked(lastMarked time.Time)
	Dials() <-chan *rpc.DialRequest
	EstablishBidiPipe(context.Context, tunnel.Stream) (tunnel.Endpoint, error)
	OnConnect(context.Context, tunnel.Stream, *int32, *SessionConsumptionMetrics) (tunnel.Endpoint, error)
}

type awaitingBidiPipe struct {
	ctx        context.Context
	stream     tunnel.Stream
	bidiPipeCh chan tunnel.Endpoint
}

type sessionState struct {
	sync.Mutex
	doneCh              <-chan struct{}
	cancel              context.CancelFunc
	lastMarked          time.Time
	awaitingBidiPipeMap map[tunnel.ConnID]*awaitingBidiPipe
	dials               chan *rpc.DialRequest
}

// EstablishBidiPipe registers the given stream as waiting for a matching stream to arrive in a call
// to Tunnel, sends a DialRequest to the owner of this sessionState, and then waits. When the call
// arrives, a BidiPipe connecting the two streams is returned.
func (ss *sessionState) EstablishBidiPipe(ctx context.Context, stream tunnel.Stream) (te tunnel.Endpoint, err error) {
	ctx, span := otel.Tracer("").Start(ctx, "EstablishBidiPipe")
	defer tracing.EndAndRecord(span, err)
	// Dispatch directly to agent and let the dial happen there
	bidiPipeCh := make(chan tunnel.Endpoint)
	id := stream.ID()
	abp := &awaitingBidiPipe{ctx: ctx, stream: stream, bidiPipeCh: bidiPipeCh}

	ss.Lock()
	if ss.awaitingBidiPipeMap == nil {
		ss.awaitingBidiPipeMap = map[tunnel.ConnID]*awaitingBidiPipe{id: abp}
	} else {
		ss.awaitingBidiPipeMap[id] = abp
	}
	ss.Unlock()

	pCtx := ctx
	ctx, span = otel.Tracer("").Start(ctx, "EstablishBidiPipe.DialRequest")
	// Send dial request to the client/agent
	dr := &rpc.DialRequest{
		ConnId:           []byte(id),
		RoundtripLatency: int64(stream.RoundtripLatency()),
		DialTimeout:      int64(stream.DialTimeout()),
	}
	propagator := otel.GetTextMapPropagator()
	carrier := propagation.MapCarrier{}
	propagator.Inject(ctx, carrier)
	dr.TraceContext = carrier
	select {
	case <-ss.Done():
		span.End()
		return nil, status.Error(codes.Canceled, "session cancelled")
	case ss.dials <- dr:
	}
	span.End()

	ctx, span = otel.Tracer("").Start(pCtx, "EstablishBidiPipe.Wait")
	defer span.End()
	// Wait for the client/agent to connect. Allow extra time for the call
	ctx, cancel := context.WithTimeout(ctx, stream.DialTimeout()+stream.RoundtripLatency())
	defer cancel()
	select {
	case <-ctx.Done():
		return nil, status.Error(codes.DeadlineExceeded, "timeout while establishing bidipipe")
	case <-ss.Done():
		return nil, status.Error(codes.Canceled, "session cancelled")
	case bidi := <-bidiPipeCh:
		return bidi, nil
	}
}

func (ss *sessionState) AwaitingBidiMapOwnerSessionID(stream tunnel.Stream) string {
	ss.Lock()
	defer ss.Unlock()
	if abp, ok := ss.awaitingBidiPipeMap[stream.ID()]; ok {
		return abp.stream.SessionID()
	}
	return ""
}

// OnConnect checks if a stream is waiting for the given stream to arrive in order to create a BidiPipe.
// If that's the case, the BidiPipe is created, started, and returned by both this method and the EstablishBidiPipe
// method that registered the waiting stream. Otherwise, this method returns nil.
func (ss *sessionState) OnConnect(
	ctx context.Context,
	stream tunnel.Stream,
	counter *int32,
	consumptionMetrics *SessionConsumptionMetrics,
) (te tunnel.Endpoint, err error) {
	ctx, span := otel.Tracer("").Start(ctx, "OnConnect")
	defer tracing.EndAndRecord(span, err)
	id := stream.ID()
	id.SpanRecord(span)
	ss.Lock()
	// abp is a session corresponding to an end user machine
	abp, ok := ss.awaitingBidiPipeMap[id]
	if ok {
		delete(ss.awaitingBidiPipeMap, id)
	}
	ss.Unlock()

	if !ok {
		return nil, nil
	}
	name := fmt.Sprintf("%s: session %s -> %s", id, abp.stream.SessionID(), stream.SessionID())
	tunnelProbes := &tunnel.BidiPipeProbes{}
	if consumptionMetrics != nil {
		tunnelProbes.BytesProbeA = consumptionMetrics.FromClientBytes
		tunnelProbes.BytesProbeB = consumptionMetrics.ToClientBytes
	}

	link := trace.LinkFromContext(ctx)
	abp.ctx, span = otel.Tracer("").Start(abp.ctx, "OnConnect.bidiPipe.Start", trace.WithLinks(link))
	defer span.End()

	bidiPipe := tunnel.NewBidiPipe(abp.stream, stream, name, counter, tunnelProbes)
	bidiPipe.Start(abp.ctx)

	defer close(abp.bidiPipeCh)
	select {
	case <-ss.Done():
		return nil, status.Error(codes.Canceled, "session cancelled")
	case abp.bidiPipeCh <- bidiPipe:
		return bidiPipe, nil
	}
}

func (ss *sessionState) Cancel() {
	ss.cancel()
	close(ss.dials)
}

func (ss *sessionState) Dials() <-chan *rpc.DialRequest {
	return ss.dials
}

func (ss *sessionState) Done() <-chan struct{} {
	return ss.doneCh
}

func (ss *sessionState) LastMarked() time.Time {
	return ss.lastMarked
}

func (ss *sessionState) SetLastMarked(lastMarked time.Time) {
	ss.lastMarked = lastMarked
}

func newSessionState(ctx context.Context, now time.Time) sessionState {
	ctx, cancel := context.WithCancel(ctx)
	return sessionState{
		doneCh:     ctx.Done(),
		cancel:     cancel,
		lastMarked: now,
		dials:      make(chan *rpc.DialRequest),
	}
}

type clientSessionState struct {
	sessionState

	consumptionMetrics *SessionConsumptionMetrics
}

func (css *clientSessionState) ConsumptionMetrics() *SessionConsumptionMetrics {
	return css.consumptionMetrics
}

func newClientSessionState(ctx context.Context, ts time.Time) *clientSessionState {
	return &clientSessionState{
		sessionState: newSessionState(ctx, ts),

		consumptionMetrics: NewSessionConsumptionMetrics(),
	}
}

type agentSessionState struct {
	sessionState
	dnsRequests  chan *rpc.DNSRequest
	dnsResponses map[string]chan *rpc.DNSResponse
}

func newAgentSessionState(ctx context.Context, ts time.Time) *agentSessionState {
	return &agentSessionState{
		sessionState: newSessionState(ctx, ts),
		dnsRequests:  make(chan *rpc.DNSRequest),
		dnsResponses: make(map[string]chan *rpc.DNSResponse),
	}
}

func (ss *agentSessionState) Cancel() {
	close(ss.dnsRequests)
	for k, lr := range ss.dnsResponses {
		delete(ss.dnsResponses, k)
		close(lr)
	}
	ss.sessionState.Cancel()
}
