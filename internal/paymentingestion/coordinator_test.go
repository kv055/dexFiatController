package paymentingestion

import (
	"context"
	"testing"
	"time"

	"dexFiatController/internal/paymentevents"
)

func TestRejectedEmailStopsBeforeMatching(t *testing.T) {
	processor := &fakeProcessor{}
	reporter := &fakeReporter{}
	coordinator := New(processor, &fakeClassifier{}, &fakeEscalator{enabled: true}, reporter)

	receipt, err := coordinator.Handle(context.Background(), emailSignal(false, ModeActive))
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.Done || processor.matchCalls != 0 || processor.completeCalls != 0 {
		t.Fatalf("receipt=%+v match=%d complete=%d", receipt, processor.matchCalls, processor.completeCalls)
	}
	if reporter.last().Kind != OutcomeRejected {
		t.Fatalf("outcome = %q", reporter.last().Kind)
	}
}

func TestCaptureAndShadowNeverCompleteOrEscalate(t *testing.T) {
	processor := &fakeProcessor{candidate: &paymentevents.Candidate{ID: 42}}
	escalator := &fakeEscalator{enabled: true}
	reporter := &fakeReporter{}
	coordinator := New(processor, &fakeClassifier{}, escalator, reporter)

	if _, err := coordinator.Handle(context.Background(), emailSignal(true, ModeCapture)); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Handle(context.Background(), emailSignal(true, ModeShadow)); err != nil {
		t.Fatal(err)
	}
	if processor.completeCalls != 0 || escalator.classifyCalls != 0 || escalator.pickCalls != 0 {
		t.Fatalf("complete=%d classify=%d pick=%d", processor.completeCalls, escalator.classifyCalls, escalator.pickCalls)
	}
	if reporter.last().Kind != OutcomeShadowMatched {
		t.Fatalf("last outcome = %q", reporter.last().Kind)
	}
}

func TestUnknownPushClassificationResumesSharedMatch(t *testing.T) {
	processor := &fakeProcessor{
		candidate:  &paymentevents.Candidate{ID: 7},
		completion: paymentevents.CompletionResult{State: paymentevents.CompletionReleased},
	}
	classifier := &fakeClassifier{}
	escalator := &fakeEscalator{enabled: true}
	reporter := &fakeReporter{}
	coordinator := New(processor, classifier, escalator, reporter)

	if _, err := coordinator.Handle(context.Background(), pushSignal()); err != nil {
		t.Fatal(err)
	}
	if escalator.classifyCalls != 1 || escalator.lastID == "" || processor.matchCalls != 0 {
		t.Fatalf("classification calls=%d pending=%q match=%d", escalator.classifyCalls, escalator.lastID, processor.matchCalls)
	}
	if err := coordinator.DecideClassification(context.Background(), escalator.lastID, true); err != nil {
		t.Fatal(err)
	}
	if !classifier.addedAllow || processor.matchCalls != 1 || processor.completeCalls != 1 {
		t.Fatalf("allow=%v match=%d complete=%d", classifier.addedAllow, processor.matchCalls, processor.completeCalls)
	}
	if reporter.last().Kind != OutcomeReleased {
		t.Fatalf("last outcome = %q", reporter.last().Kind)
	}
}

func TestAmbiguousSignalSelectionAndZeroMatchRouting(t *testing.T) {
	candidates := []paymentevents.Candidate{{ID: 10}, {ID: 11}}
	processor := &fakeProcessor{
		ambiguous:  candidates,
		completion: paymentevents.CompletionResult{State: paymentevents.CompletionWaiting},
	}
	escalator := &fakeEscalator{enabled: true}
	reporter := &fakeReporter{}
	coordinator := New(processor, &fakeClassifier{allowed: true}, escalator, reporter)

	if _, err := coordinator.Handle(context.Background(), emailSignal(true, ModeActive)); err != nil {
		t.Fatal(err)
	}
	if escalator.pickCalls != 1 {
		t.Fatalf("pick calls = %d", escalator.pickCalls)
	}
	if err := coordinator.DecideSelection(context.Background(), escalator.lastID, 11); err != nil {
		t.Fatal(err)
	}
	if processor.completedCandidate != 11 || reporter.last().Kind != OutcomeWaitingChannels {
		t.Fatalf("candidate=%d outcome=%q", processor.completedCandidate, reporter.last().Kind)
	}

	processor.candidate, processor.ambiguous = nil, nil
	before := escalator.pickCalls
	if _, err := coordinator.Handle(context.Background(), emailSignal(true, ModeActive)); err != nil {
		t.Fatal(err)
	}
	if escalator.pickCalls != before || reporter.last().Kind != OutcomeUnmatched {
		t.Fatalf("zero match escalated: calls=%d outcome=%q", escalator.pickCalls, reporter.last().Kind)
	}
}

func TestManualConfirmationIsDeliveredBeforeCompletion(t *testing.T) {
	request := &paymentevents.PaymentConfirmationRequest{
		ID: 99, TransactionID: 7, PaymentEventID: 1,
		MessageText: "confirm transaction",
	}
	processor := &fakeProcessor{
		candidate: &paymentevents.Candidate{ID: 7},
		completion: paymentevents.CompletionResult{
			State: paymentevents.CompletionManual,
			ConfirmationResult: paymentevents.ConfirmationResult{
				ManualRequired: true, Request: request,
			},
		},
	}
	escalator := &fakeEscalator{enabled: true}
	reporter := &fakeReporter{}
	coordinator := New(processor, &fakeClassifier{}, escalator, reporter)

	if _, err := coordinator.Handle(context.Background(), emailSignal(true, ModeActive)); err != nil {
		t.Fatal(err)
	}
	if escalator.confirmCalls != 1 || processor.deliveredRequest != 99 {
		t.Fatalf("confirm calls=%d delivered=%d", escalator.confirmCalls, processor.deliveredRequest)
	}
	if reporter.last().Kind != OutcomeAwaitingConfirmation {
		t.Fatalf("outcome=%q", reporter.last().Kind)
	}
}

func emailSignal(valid bool, mode string) paymentevents.InboundSignal {
	now := time.Now().UTC()
	reason := ""
	if !valid {
		reason = "DKIM verification failed"
	}
	return paymentevents.InboundSignal{
		Event: paymentevents.PaymentEvent{
			Source: paymentevents.SourceEmail, Provider: "WISE",
			SourceEventID: now.String(), RawMessageHash: paymentevents.EventIDFromBytes([]byte(now.String())),
			AmountMinor: 1000, Currency: "EUR",
			ProviderTimestamp: now, ReceivedTimestamp: now,
		},
		Validation: paymentevents.Validation{Accepted: valid, Reason: reason},
		Rollout: paymentevents.Rollout{
			Mode: mode, UID: 10, ActivationUID: 1,
			ActivationTime: now.Add(-time.Hour),
		},
	}
}

func pushSignal() paymentevents.InboundSignal {
	now := time.Now().UTC()
	return paymentevents.InboundSignal{
		Event: paymentevents.PaymentEvent{
			Source: paymentevents.SourcePush, Provider: "WISE",
			SourceEventID: "push-1", RawMessageHash: paymentevents.EventIDFromBytes([]byte("push-1")),
			AmountMinor: 1000, Currency: "EUR",
			ProviderTimestamp: now, ReceivedTimestamp: now,
		},
		Presentation: paymentevents.Presentation{App: "Wise", Normalized: "payment received"},
		Validation:   paymentevents.Validation{Accepted: true},
	}
}

type fakeProcessor struct {
	nextID             int64
	matchCalls         int
	completeCalls      int
	completedCandidate uint
	candidate          *paymentevents.Candidate
	ambiguous          []paymentevents.Candidate
	candidates         []paymentevents.Candidate
	completion         paymentevents.CompletionResult
	decisionResult     paymentevents.CompletionResult
	decisionRequest    paymentevents.PaymentConfirmationRequest
	deliveredRequest   int64
	deliveryError      string
}

func (f *fakeProcessor) Record(context.Context, paymentevents.PaymentEvent, string) (int64, bool, error) {
	f.nextID++
	return f.nextID, false, nil
}

func (f *fakeProcessor) Match(context.Context, paymentevents.PaymentEvent, time.Time) (*paymentevents.Candidate, []paymentevents.Candidate, []paymentevents.Candidate, error) {
	f.matchCalls++
	return f.candidate, f.ambiguous, f.candidates, nil
}

func (f *fakeProcessor) Complete(_ context.Context, _ paymentevents.PaymentEvent, _ int64, candidate paymentevents.Candidate) (paymentevents.CompletionResult, error) {
	f.completeCalls++
	f.completedCandidate = candidate.ID
	return f.completion, nil
}

func (f *fakeProcessor) CompleteSelected(_ context.Context, _ paymentevents.PaymentEvent, _ int64, candidate paymentevents.Candidate) (paymentevents.CompletionResult, error) {
	f.completeCalls++
	f.completedCandidate = candidate.ID
	return f.completion, nil
}

func (f *fakeProcessor) Update(context.Context, int64, string, *uint, string) error {
	return nil
}

func (f *fakeProcessor) MarkConfirmationDelivered(_ context.Context, requestID int64, _ string, _ int64) error {
	f.deliveredRequest = requestID
	return nil
}

func (f *fakeProcessor) MarkConfirmationDeliveryError(_ context.Context, _ int64, text string) error {
	f.deliveryError = text
	return nil
}

func (f *fakeProcessor) PendingConfirmationRequests(context.Context) ([]paymentevents.PaymentConfirmationRequest, error) {
	return nil, nil
}

func (f *fakeProcessor) DecideConfirmation(context.Context, int64, string, bool) (paymentevents.CompletionResult, paymentevents.PaymentConfirmationRequest, error) {
	return f.decisionResult, f.decisionRequest, nil
}

type fakeClassifier struct {
	allowed    bool
	denied     bool
	addedAllow bool
}

func (f *fakeClassifier) Match(string, string) (bool, bool)  { return f.allowed, f.denied }
func (f *fakeClassifier) Add(_ string, _ string, allow bool) { f.addedAllow = allow }

type fakeEscalator struct {
	enabled       bool
	classifyCalls int
	pickCalls     int
	confirmCalls  int
	lastID        string
}

func (f *fakeEscalator) Enabled() bool { return f.enabled }
func (f *fakeEscalator) SendClassify(_ context.Context, id string, _ paymentevents.Presentation) error {
	f.classifyCalls++
	f.lastID = id
	return nil
}
func (f *fakeEscalator) SendPick(_ context.Context, id string, _ paymentevents.Presentation, _ []paymentevents.Candidate) error {
	f.pickCalls++
	f.lastID = id
	return nil
}
func (f *fakeEscalator) SendPaymentConfirmation(_ context.Context, request paymentevents.PaymentConfirmationRequest) (string, int64, error) {
	f.confirmCalls++
	f.lastID = request.MessageText
	return "123", 456, nil
}

type fakeReporter struct {
	outcomes []Outcome
}

func (f *fakeReporter) Report(_ context.Context, _ paymentevents.InboundSignal, outcome Outcome) {
	f.outcomes = append(f.outcomes, outcome)
}

func (f *fakeReporter) last() Outcome {
	return f.outcomes[len(f.outcomes)-1]
}
