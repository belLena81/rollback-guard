package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/adapters/kubernetes"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/resilience"
	"github.com/belLena81/rollback-guard/internal/ports"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// These tests run the RollbackExecutor against a real MCP rollback server
// over HTTP and break the execute_approved_rollback exchange after the server
// has read the request. Each fault runs twice: once where the server never
// handled the call, and once where it applied the rollback and only the
// response was lost. In neither case may the adapter report a plain failure,
// which the application would record as a failed execution.

type transportFault string

const (
	faultEOF         transportFault = "connection closed without a response"
	faultReset       transportFault = "connection reset"
	faultTimeout     transportFault = "no response before the call deadline"
	faultServerError transportFault = "HTTP 502"
	faultInternal    transportFault = "HTTP 500"
	faultUnavailable transportFault = "HTTP 503"
	faultSession     transportFault = "session lost (HTTP 404)"
	faultGarbled     transportFault = "undecodable 200 response"
	// faultUnstructured is a valid tool result with isError and no
	// structured content: what the SDK sends when a handler fails or its
	// output cannot be encoded, possibly after it mutated.
	faultUnstructured transportFault = "tool error without structured result"
	faultRPCError     transportFault = "JSON-RPC internal error"
)

// rollbackServerState is the fake cluster behind both MCP servers: the
// rollback tool applies the mutation once per key, the read tools observe it.
type rollbackServerState struct {
	proposal     domain.RollbackProposal
	mu           sync.Mutex
	applied      bool
	mutations    int
	executeCalls int
}

func (s *rollbackServerState) execute(key string) ExecuteRollbackOutput {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.executeCalls++
	already := s.applied
	if !s.applied {
		s.applied = true
		s.mutations++
	}
	now := time.Now().UTC()
	return ExecuteRollbackOutput{ExecutionID: "mutation-" + key, FromRevision: string(s.proposal.CurrentRevision()), ToRevision: string(s.proposal.TargetRevision()), StartedAt: now, CompletedAt: now, AlreadyApplied: already}
}

func (s *rollbackServerState) snapshot() (applied bool, mutations, executeCalls int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applied, s.mutations, s.executeCalls
}

// readonly answers the read tools from the fake cluster in process; only the
// rollback exchange is under test.
func (s *rollbackServerState) readonly() Caller {
	return callerFunc(func(_ context.Context, params *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
		applied, _, _ := s.snapshot()
		revision := s.proposal.CurrentRevision()
		if applied {
			revision = s.proposal.TargetRevision()
		}
		if params.Name == ToolWatchRollout {
			return successfulToolResult(RolloutOutput{Revision: string(revision), DesiredReplicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1, ObservedGeneration: 2, Generation: 2, Complete: applied}), nil
		}
		return successfulToolResult(deploymentOutput(s.proposal, revision, applied, time.Now().UTC())), nil
	})
}

func newRollbackMCPServer(state *rollbackServerState) *mcpsdk.Server {
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "rollback-fault-test", Version: "v0"}, nil)
	mcpsdk.AddTool(server, &mcpsdk.Tool{Name: ToolPrepareRollback, Description: "test"}, func(_ context.Context, _ *mcpsdk.CallToolRequest, _ PrepareRollbackInput) (*mcpsdk.CallToolResult, Response[PrepareRollbackOutput], error) {
		return nil, Success(PrepareRollbackOutput{ProposalHash: string(state.proposal.Hash()), TargetRevision: string(state.proposal.TargetRevision())}), nil
	})
	mcpsdk.AddTool(server, &mcpsdk.Tool{Name: ToolExecuteApprovedRollback, Description: "test"}, func(_ context.Context, _ *mcpsdk.CallToolRequest, input ExecuteRollbackInput) (*mcpsdk.CallToolResult, Response[ExecuteRollbackOutput], error) {
		return nil, Success(state.execute(input.IdempotencyKey)), nil
	})
	return server
}

// faultInjector breaks the first execute_approved_rollback POST after reading
// it completely. With apply set, the real handler runs first (its response is
// discarded), so the server has mutated before the fault.
type faultInjector struct {
	t        *testing.T
	next     http.Handler
	fault    transportFault
	apply    bool
	injected atomic.Bool
	// executePosts counts execute_approved_rollback requests on the wire.
	executePosts atomic.Int32
}

func (f *faultInjector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Errorf("read request: %v", err)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	isExecute := r.Method == http.MethodPost && bytes.Contains(body, []byte(`"tools/call"`)) && bytes.Contains(body, []byte(`"`+ToolExecuteApprovedRollback+`"`))
	if isExecute {
		f.executePosts.Add(1)
	}
	if !isExecute || !f.injected.CompareAndSwap(false, true) {
		f.next.ServeHTTP(w, r)
		return
	}
	if f.apply {
		f.next.ServeHTTP(httptest.NewRecorder(), r.Clone(context.WithoutCancel(r.Context())))
	}
	switch f.fault {
	case faultEOF, faultReset:
		conn := f.hijack(w)
		if conn == nil {
			return
		}
		if tcp, ok := conn.(*net.TCPConn); ok && f.fault == faultReset {
			if err := tcp.SetLinger(0); err != nil { // close sends RST
				f.t.Errorf("set linger: %v", err)
			}
		}
		if err := conn.Close(); err != nil {
			f.t.Logf("close hijacked connection: %v", err)
		}
	case faultTimeout:
		<-r.Context().Done()
	case faultServerError:
		http.Error(w, "bad gateway", http.StatusBadGateway)
	case faultInternal:
		http.Error(w, "internal error", http.StatusInternalServerError)
	case faultUnavailable:
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	case faultUnstructured:
		f.writeRPC(w, body, `"result":{"content":[{"type":"text","text":"handler failed"}],"isError":true}`)
	case faultRPCError:
		f.writeRPC(w, body, `"error":{"code":-32603,"message":"internal error"}`)
	case faultSession:
		http.Error(w, "session not found", http.StatusNotFound)
	case faultGarbled:
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"jsonrpc":"2.0","id":`)); err != nil {
			f.t.Logf("write garbled response: %v", err)
		}
	default:
		f.t.Errorf("unknown fault %q", f.fault)
	}
}

// writeRPC answers the request in body with a well-formed JSON-RPC response
// whose payload member is member.
func (f *faultInjector) writeRPC(w http.ResponseWriter, body []byte, member string) {
	var request struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(body, &request); err != nil || len(request.ID) == 0 {
		f.t.Errorf("request id: %v", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(request.ID) + `,` + member + `}`)); err != nil {
		f.t.Logf("write response: %v", err)
	}
}

func (f *faultInjector) hijack(w http.ResponseWriter) net.Conn {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		f.t.Error("test server cannot hijack")
		return nil
	}
	conn, _, err := hijacker.Hijack()
	if err != nil {
		f.t.Errorf("hijack: %v", err)
		return nil
	}
	return conn
}

type appliedOutcome uint8

const (
	reconciled appliedOutcome = iota + 1
	unknown
	reconciledOrUnknown
)

const faultCallTimeout = 750 * time.Millisecond

func connectRollbackCaller(t *testing.T, handler http.Handler) (Caller, *httptest.Server) {
	t.Helper()
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	client, err := Connect(context.Background(), httpServer.URL+"/mcp", &http.Client{Timeout: 5 * time.Second}, ClientOrchestrator)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Log("close client:", err)
		}
	})
	caller, err := BoundCalls(client, faultCallTimeout)
	if err != nil {
		t.Fatal(err)
	}
	return caller, httpServer
}

func TestRollbackExecutorMapsEveryPostSendTransportFailureToReconcileOrUnknown(t *testing.T) {
	// whenApplied: what the adapter answers after the server applied the
	// rollback and the response was lost. Its replay through the same session
	// normally reconciles to success. After an undecodable response the SDK
	// closes the session, so the replay cannot run and the answer stays
	// unknown; after a 404 it closes the session asynchronously, so either
	// answer occurs. Unknown is the safe outcome: the application keeps the
	// attempt pending and a retry of the same approval reconciles it.
	whenApplied := map[transportFault]appliedOutcome{
		faultEOF: reconciled, faultReset: reconciled, faultTimeout: reconciled, faultServerError: reconciled,
		faultInternal: reconciled, faultUnavailable: reconciled, faultUnstructured: reconciled, faultRPCError: reconciled,
		faultSession: reconciledOrUnknown, faultGarbled: unknown,
	}
	for fault, wantApplied := range whenApplied {
		for _, applied := range []bool{false, true} {
			name := string(fault) + "/server did not apply"
			if applied {
				name = string(fault) + "/server applied, response lost"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				approved := testApprovedRollback(t, time.Now().UTC())
				state := &rollbackServerState{proposal: approved.Proposal()}
				injector := &faultInjector{t: t, next: HTTPHandler(newRollbackMCPServer(state), nil, 0, time.Minute, nil), fault: fault, apply: applied}
				rollback, _ := connectRollbackCaller(t, injector)
				// The watch/reconcile budget has wide headroom over the
				// injected call deadline, so a slow parallel run cannot turn
				// a reconciled case into an unknown one.
				adapter, err := NewRollbackExecutor(rollback, state.readonly(), 10*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				executor, err := resilience.NewExecutor(adapter, fastRetrier(t))
				if err != nil {
					t.Fatal(err)
				}

				execution, err := executor.ExecuteApprovedRollback(context.Background(), approved, "fault-key")

				if !injector.injected.Load() {
					t.Fatal("the fault was never injected")
				}
				gotApplied, _, handled := state.snapshot()
				posts := int(injector.executePosts.Load())
				switch {
				case !applied && (posts != 1 || handled != 0):
					// Not at the target revision: no replay, and the decorator
					// must not resend a request that reached the server.
					t.Fatalf("execute posts = %d, handled = %d, want 1/0", posts, handled)
				case applied && err == nil && (posts != 2 || handled != 2):
					// One faulted call that the server handled, one replay
					// answered from the ledger.
					t.Fatalf("execute posts = %d, handled = %d, want 2/2", posts, handled)
				case applied && posts > 2:
					t.Fatalf("execute posts = %d, want at most the call and one replay", posts)
				}
				switch {
				case err == nil:
					if !applied || !gotApplied || execution.Outcome() != domain.ExecutionSucceeded {
						t.Fatalf("execution = %v applied=%v, want success only after the server applied it", execution.Outcome(), gotApplied)
					}
				case !errors.Is(err, ports.ErrRollbackOutcomeUnknown):
					t.Fatalf("error = %v (%T), want ErrRollbackOutcomeUnknown or a reconciled success; a plain error would record a failed execution", err, err)
				case resilience.RequestNotSent(err):
					t.Fatalf("error = %v is classified as not sent", err)
				}
				if !applied && !errors.Is(err, ports.ErrRollbackOutcomeUnknown) {
					t.Fatalf("error = %v, want unknown outcome when the deployment did not reach the target", err)
				}
				if applied && wantApplied == reconciled && err != nil {
					t.Fatalf("error = %v, want the applied rollback reconciled through the same session", err)
				}
				if applied && wantApplied == unknown && !errors.Is(err, ports.ErrRollbackOutcomeUnknown) {
					t.Fatalf("error = %v, want unknown outcome", err)
				}
			})
		}
	}
}

// The other direction: a dial failure proves the request never left, so the
// adapter returns it unchanged and the resilience decorator may repeat it. No
// call reaches the server, and the outcome is not reported as unknown.
func TestRollbackExecutorLeavesPreSendDialFailureRetryable(t *testing.T) {
	approved := testApprovedRollback(t, time.Now().UTC())
	state := &rollbackServerState{proposal: approved.Proposal()}
	rollback, httpServer := connectRollbackCaller(t, HTTPHandler(newRollbackMCPServer(state), nil, 0, time.Minute, nil))
	executeAttempts := 0
	closing := callerFunc(func(ctx context.Context, params *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
		if params.Name == ToolExecuteApprovedRollback {
			executeAttempts++
			httpServer.Close() // nothing listens any more: the dial is refused
		}
		return rollback.CallTool(ctx, params)
	})
	adapter, err := NewRollbackExecutor(closing, state.readonly(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := resilience.NewExecutor(adapter, fastRetrier(t))
	if err != nil {
		t.Fatal(err)
	}

	_, err = executor.ExecuteApprovedRollback(context.Background(), approved, "dial-key")

	if err == nil || errors.Is(err, ports.ErrRollbackOutcomeUnknown) || !resilience.RequestNotSent(err) {
		t.Fatalf("error = %v, want the unchanged, retryable dial failure", err)
	}
	if _, mutations, calls := state.snapshot(); mutations != 0 || calls != 0 {
		t.Fatalf("server saw %d execute calls and %d mutations, want none", calls, mutations)
	}
	if executeAttempts != 1 {
		t.Fatalf("execute attempts = %d, want 1 (the retried attempt fails earlier, at prepare)", executeAttempts)
	}
}

// Resuming an attempt whose rollback an earlier call already applied: the
// adapter's own inspection fails, so it cannot tell, and prepare_rollback
// answers revision drift because the Deployment is already at the target.
// That must not be reported as a definitive failure.
func TestRollbackExecutorPrepareRejectionAfterFailedInspectionIsUnknown(t *testing.T) {
	t.Parallel()
	approved := testApprovedRollback(t, time.Now().UTC())
	tests := []struct {
		name        string
		inspectErr  error
		prepareErr  error
		wantUnknown bool
	}{
		{name: "inspect reset, prepare drift", inspectErr: io.ErrUnexpectedEOF, prepareErr: nil, wantUnknown: true},
		{name: "inspect timeout, prepare drift", inspectErr: context.DeadlineExceeded, prepareErr: nil, wantUnknown: true},
		{name: "inspect ok, prepare drift is definitive", inspectErr: nil, prepareErr: nil, wantUnknown: false},
		{name: "inspect dial failure, prepare dial failure stays retryable", inspectErr: dialFailure(), prepareErr: dialFailure(), wantUnknown: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			proposal := approved.Proposal()
			executes := 0
			readonly := callerFunc(func(context.Context, *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
				if test.inspectErr != nil {
					return nil, test.inspectErr
				}
				return successfulToolResult(deploymentOutput(proposal, proposal.CurrentRevision(), false, time.Now().UTC())), nil
			})
			rollback := callerFunc(func(_ context.Context, params *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
				if params.Name == ToolExecuteApprovedRollback {
					executes++
				}
				if test.prepareErr != nil {
					return nil, test.prepareErr
				}
				result, _ := Failure[PrepareRollbackOutput](kubernetes.ErrRevisionDrift)
				result.StructuredContent = Response[PrepareRollbackOutput]{OK: false, Error: &ToolError{Code: CodeRevisionDrift, Message: "deployment revision changed"}}
				return result, nil
			})
			executor, err := NewRollbackExecutor(rollback, readonly, time.Second)
			if err != nil {
				t.Fatal(err)
			}

			_, err = executor.ExecuteApprovedRollback(context.Background(), approved, "resume-key")

			if err == nil || executes != 0 {
				t.Fatalf("err = %v, executes = %d, want a failure before any execute", err, executes)
			}
			if got := errors.Is(err, ports.ErrRollbackOutcomeUnknown); got != test.wantUnknown {
				t.Fatalf("err = %v, unknown = %v, want %v", err, got, test.wantUnknown)
			}
			if !test.wantUnknown && test.prepareErr == nil && !errors.Is(err, domain.ErrApprovalMismatch) {
				t.Fatalf("err = %v, want the definitive drift rejection", err)
			}
		})
	}
}

// A server that applied the rollback but could not record it answers with
// internal, never with the rejection code of the recording error.
func TestClassifyErrorReportsUnrecordedMutationAsInternal(t *testing.T) {
	t.Parallel()
	for name, cause := range map[string]error{
		"idempotency conflict": &domain.IdempotencyConflictError{Key: "key"},
		"validation":           &domain.ValidationError{Field: "rollback_mutation", Reason: "invalid"},
		"lease not held":       ports.ErrRollbackMutationLeaseNotHeld,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := classifyError(errors.Join(kubernetes.ErrMutationNotRecorded, cause))
			if got.Code != CodeInternal {
				t.Fatalf("code = %s, want %s", got.Code, CodeInternal)
			}
			if mutationFailure(errorFromTool(got)) != mutationAmbiguous {
				t.Fatal("client does not reconcile an unrecorded mutation")
			}
		})
	}
}

func TestMutationFailureClassification(t *testing.T) {
	t.Parallel()
	dial := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	tests := []struct {
		name string
		err  error
		want mutationFailureKind
	}{
		{name: "dial refused", err: dial, want: mutationNotSent},
		{name: "wrapped dial", err: errors.Join(errors.New("sending tools/call"), dial), want: mutationNotSent},
		{name: "read reset", err: &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}, want: mutationAmbiguous},
		{name: "EOF", err: io.EOF, want: mutationAmbiguous},
		{name: "unexpected EOF", err: io.ErrUnexpectedEOF, want: mutationAmbiguous},
		{name: "deadline", err: context.DeadlineExceeded, want: mutationAmbiguous},
		{name: "canceled", err: context.Canceled, want: mutationAmbiguous},
		{name: "plain transport text", err: errors.New("broken pipe"), want: mutationAmbiguous},
		{name: "internal tool error", err: errorFromTool(ToolError{Code: CodeInternal}), want: mutationAmbiguous},
		{name: "dependency unavailable", err: errorFromTool(ToolError{Code: CodeDependencyUnavailable, Retryable: true}), want: mutationAmbiguous},
		{name: "unknown code", err: errorFromTool(ToolError{Code: "new_code"}), want: mutationAmbiguous},
		{name: "server timeout code", err: errorFromTool(ToolError{Code: CodeTimeout}), want: mutationAmbiguous},
		{name: "already unknown", err: ports.ErrRollbackOutcomeUnknown, want: mutationAmbiguous},
		{name: "revision drift", err: errorFromTool(ToolError{Code: CodeRevisionDrift}), want: mutationRejected},
		{name: "approval mismatch", err: errorFromTool(ToolError{Code: CodeApprovalMismatch}), want: mutationRejected},
		{name: "policy denied", err: errorFromTool(ToolError{Code: CodePolicyDenied}), want: mutationRejected},
		{name: "expired", err: errorFromTool(ToolError{Code: CodeExpired}), want: mutationRejected},
		{name: "idempotency conflict", err: errorFromTool(ToolError{Code: CodeIdempotencyConflict}), want: mutationRejected},
		{name: "invalid argument", err: errorFromTool(ToolError{Code: CodeInvalidArgument}), want: mutationRejected},
		{name: "target forbidden", err: errorFromTool(ToolError{Code: CodeTargetForbidden}), want: mutationRejected},
		{name: "not found", err: errorFromTool(ToolError{Code: CodeNotFound}), want: mutationRejected},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := mutationFailure(test.err); got != test.want {
				t.Fatalf("mutationFailure(%v) = %d, want %d", test.err, got, test.want)
			}
		})
	}
}
