// Package paymentingestion is the source-neutral caller for incoming payment
// signals. Email and push adapters stop at InboundSignal; this package decides
// legitimacy, persistence, matching, escalation, confirmation, and release.
package paymentingestion

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"dexFiatController/internal/paymentevents"
)

const (
	ModeCapture = "capture"
	ModeShadow  = "shadow"
	ModeActive  = "active"
)

type Processor interface {
	Record(context.Context, paymentevents.PaymentEvent, string) (int64, bool, error)
	Match(context.Context, paymentevents.PaymentEvent, time.Time) (*paymentevents.Candidate, []paymentevents.Candidate, []paymentevents.Candidate, error)
	Complete(context.Context, paymentevents.PaymentEvent, int64, paymentevents.Candidate) (paymentevents.CompletionResult, error)
	Update(context.Context, int64, string, *uint, string) error
}

type Classifier interface {
	Match(app, normalized string) (allowed, denied bool)
	Add(app, normalized string, allow bool)
}

type Escalator interface {
	Enabled() bool
	SendClassify(context.Context, string, paymentevents.Presentation) error
	SendPick(context.Context, string, paymentevents.Presentation, []paymentevents.Candidate) error
}

type OutcomeKind string

const (
	OutcomeCaptured              OutcomeKind = "captured"
	OutcomeRejected              OutcomeKind = "rejected"
	OutcomeDenied                OutcomeKind = "denied"
	OutcomeUnmatched             OutcomeKind = "unmatched"
	OutcomeAwaitingClassify      OutcomeKind = "awaiting_classification"
	OutcomeAwaitingSelection     OutcomeKind = "awaiting_selection"
	OutcomeNoClassification      OutcomeKind = "unclassified_no_escalation"
	OutcomeNoSelection           OutcomeKind = "ambiguous_no_escalation"
	OutcomeListError             OutcomeKind = "list_error"
	OutcomeTelegramError         OutcomeKind = "telegram_error"
	OutcomeUserAllowed           OutcomeKind = "user_allowed"
	OutcomeUserDenied            OutcomeKind = "user_denied"
	OutcomePicked                OutcomeKind = "picked"
	OutcomeWaitingChannels       OutcomeKind = "waiting_channels"
	OutcomeDuplicateConfirmation OutcomeKind = "duplicate_confirmation"
	OutcomeReleased              OutcomeKind = "released"
	OutcomeConfirmationError     OutcomeKind = "confirmation_error"
	OutcomeReleaseError          OutcomeKind = "release_error"
	OutcomeShadowMatched         OutcomeKind = "shadow_matched"
	OutcomeShadowAmbiguous       OutcomeKind = "shadow_ambiguous"
	OutcomeShadowUnmatched       OutcomeKind = "shadow_unmatched"
)

type Outcome struct {
	Kind      OutcomeKind
	EventID   int64
	TxID      *uint
	PendingID string
	Required  []string
	Present   []string
	Reason    string
}

type Reporter interface {
	Report(context.Context, paymentevents.InboundSignal, Outcome)
}

type Receipt struct {
	Signal    paymentevents.InboundSignal
	EventID   int64
	Duplicate bool
	Done      bool
}

type pendingKind uint8

const (
	pendingClassification pendingKind = iota + 1
	pendingSelection
)

type pendingEntry struct {
	kind       pendingKind
	receipt    Receipt
	candidates []paymentevents.Candidate
}

type Coordinator struct {
	processor  Processor
	classifier Classifier
	escalator  Escalator
	reporter   Reporter
	now        func() time.Time

	pendingMu sync.Mutex
	pending   map[string]pendingEntry
}

func New(processor Processor, classifier Classifier, escalator Escalator, reporter Reporter) *Coordinator {
	return &Coordinator{
		processor: processor, classifier: classifier, escalator: escalator,
		reporter: reporter, now: func() time.Time { return time.Now().UTC() },
		pending: make(map[string]pendingEntry),
	}
}

// Accept persists and deduplicates a source output before any decision is made.
// Capture and invalid email signals terminate here after an audit status update.
func (c *Coordinator) Accept(ctx context.Context, signal paymentevents.InboundSignal) (Receipt, error) {
	status := "received"
	if signal.Event.Source == paymentevents.SourceEmail {
		status = "parsed"
		if signal.Rollout.Mode == ModeCapture {
			status = "captured"
		}
	}
	if signal.Rollout.Mode != ModeCapture && !signal.Validation.Accepted {
		status = "rejected"
	}
	eventID, duplicate, err := c.processor.Record(ctx, signal.Event, status)
	receipt := Receipt{Signal: signal, EventID: eventID, Duplicate: duplicate}
	if err != nil || duplicate {
		return receipt, err
	}
	if signal.Rollout.Mode == ModeCapture {
		receipt.Done = true
		if !signal.Validation.Accepted {
			_ = c.processor.Update(ctx, eventID, "parse_error", nil, signal.Validation.Reason)
		}
		c.report(ctx, signal, Outcome{Kind: OutcomeCaptured, EventID: eventID, Reason: signal.Validation.Reason})
		return receipt, nil
	}
	if !signal.Validation.Accepted {
		receipt.Done = true
		_ = c.processor.Update(ctx, eventID, "rejected", nil, signal.Validation.Reason)
		c.report(ctx, signal, Outcome{Kind: OutcomeRejected, EventID: eventID, Reason: signal.Validation.Reason})
	}
	return receipt, nil
}

func (c *Coordinator) Handle(ctx context.Context, signal paymentevents.InboundSignal) (Receipt, error) {
	receipt, err := c.Accept(ctx, signal)
	if err != nil || receipt.Duplicate || receipt.Done {
		return receipt, err
	}
	return receipt, c.Process(ctx, receipt)
}

// Process owns all legitimacy and routing decisions after source decoding.
func (c *Coordinator) Process(ctx context.Context, receipt Receipt) error {
	signal := receipt.Signal
	if signal.Event.Source == paymentevents.SourceEmail {
		if signal.Rollout.Mode == ModeActive &&
			(signal.Rollout.UID <= signal.Rollout.ActivationUID ||
				signal.Event.ProviderTimestamp.IsZero() ||
				signal.Event.ProviderTimestamp.Before(signal.Rollout.ActivationTime)) {
			reason := "message is before the active UID/time watermark"
			_ = c.processor.Update(ctx, receipt.EventID, "pre_activation", nil, reason)
			c.report(ctx, signal, Outcome{Kind: OutcomeRejected, EventID: receipt.EventID, Reason: reason})
			return nil
		}
	}

	if signal.Event.Source == paymentevents.SourcePush {
		allowed, denied := c.classifier.Match(signal.Presentation.App, signal.Presentation.Normalized)
		switch {
		case denied:
			_ = c.processor.Update(ctx, receipt.EventID, "denied", nil, "")
			c.report(ctx, signal, Outcome{Kind: OutcomeDenied, EventID: receipt.EventID})
			return nil
		case !allowed:
			return c.escalateClassification(ctx, receipt)
		}
	}
	return c.match(ctx, receipt)
}

func (c *Coordinator) match(ctx context.Context, receipt Receipt) error {
	signal := receipt.Signal
	candidate, ambiguous, candidates, err := c.processor.Match(ctx, signal.Event, c.now())
	if err != nil {
		_ = c.processor.Update(ctx, receipt.EventID, "list_error", nil, err.Error())
		c.report(ctx, signal, Outcome{Kind: OutcomeListError, EventID: receipt.EventID, Reason: err.Error()})
		return err
	}
	if signal.Event.Source == paymentevents.SourceEmail && signal.Rollout.Mode == ModeShadow {
		outcome := Outcome{EventID: receipt.EventID}
		switch {
		case candidate != nil:
			outcome.Kind, outcome.TxID = OutcomeShadowMatched, &candidate.ID
			_ = c.processor.Update(ctx, receipt.EventID, "shadow_matched", &candidate.ID, "")
		case len(ambiguous) > 0:
			outcome.Kind = OutcomeShadowAmbiguous
			_ = c.processor.Update(ctx, receipt.EventID, "shadow_ambiguous", nil, "")
		default:
			outcome.Kind = OutcomeShadowUnmatched
			_ = c.processor.Update(ctx, receipt.EventID, "shadow_unmatched", nil, "")
		}
		c.report(ctx, signal, outcome)
		return nil
	}
	switch {
	case candidate != nil:
		return c.complete(ctx, receipt, *candidate)
	case len(ambiguous) > 0:
		return c.escalateSelection(ctx, receipt, ambiguous)
	default:
		status, reason := paymentevents.EligibilityStatus(signal.Event, candidates, c.now())
		_ = c.processor.Update(ctx, receipt.EventID, status, nil, reason)
		kind := OutcomeUnmatched
		if status == "rejected" {
			kind = OutcomeRejected
		}
		c.report(ctx, signal, Outcome{Kind: kind, EventID: receipt.EventID, Reason: reason})
		return nil
	}
}

func (c *Coordinator) complete(ctx context.Context, receipt Receipt, candidate paymentevents.Candidate) error {
	result, err := c.processor.Complete(ctx, receipt.Signal.Event, receipt.EventID, candidate)
	outcome := Outcome{
		EventID: receipt.EventID, TxID: &candidate.ID,
		Required: result.Required, Present: result.Present,
	}
	if err != nil {
		outcome.Kind = OutcomeConfirmationError
		if result.Advanced {
			outcome.Kind = OutcomeReleaseError
		}
		outcome.Reason = err.Error()
		c.report(ctx, receipt.Signal, outcome)
		return err
	}
	switch result.State {
	case paymentevents.CompletionWaiting:
		outcome.Kind = OutcomeWaitingChannels
	case paymentevents.CompletionDuplicate:
		outcome.Kind = OutcomeDuplicateConfirmation
	default:
		outcome.Kind = OutcomeReleased
	}
	c.report(ctx, receipt.Signal, outcome)
	return nil
}

func (c *Coordinator) escalateClassification(ctx context.Context, receipt Receipt) error {
	if c.escalator == nil || !c.escalator.Enabled() {
		_ = c.processor.Update(ctx, receipt.EventID, "unclassified", nil, "")
		c.report(ctx, receipt.Signal, Outcome{Kind: OutcomeNoClassification, EventID: receipt.EventID})
		return nil
	}
	id := c.addPending(pendingEntry{kind: pendingClassification, receipt: receipt})
	_ = c.processor.Update(ctx, receipt.EventID, "awaiting_classification", nil, "")
	c.report(ctx, receipt.Signal, Outcome{Kind: OutcomeAwaitingClassify, EventID: receipt.EventID, PendingID: id})
	if err := c.escalator.SendClassify(ctx, id, receipt.Signal.Presentation); err != nil {
		_ = c.processor.Update(ctx, receipt.EventID, "telegram_error", nil, err.Error())
		c.report(ctx, receipt.Signal, Outcome{Kind: OutcomeTelegramError, EventID: receipt.EventID, PendingID: id, Reason: err.Error()})
		return err
	}
	return nil
}

func (c *Coordinator) escalateSelection(ctx context.Context, receipt Receipt, candidates []paymentevents.Candidate) error {
	if c.escalator == nil || !c.escalator.Enabled() {
		_ = c.processor.Update(ctx, receipt.EventID, "ambiguous", nil, "")
		c.report(ctx, receipt.Signal, Outcome{Kind: OutcomeNoSelection, EventID: receipt.EventID})
		return nil
	}
	id := c.addPending(pendingEntry{kind: pendingSelection, receipt: receipt, candidates: candidates})
	_ = c.processor.Update(ctx, receipt.EventID, "awaiting_selection", nil, "")
	c.report(ctx, receipt.Signal, Outcome{Kind: OutcomeAwaitingSelection, EventID: receipt.EventID, PendingID: id})
	if err := c.escalator.SendPick(ctx, id, receipt.Signal.Presentation, candidates); err != nil {
		_ = c.processor.Update(ctx, receipt.EventID, "telegram_error", nil, err.Error())
		c.report(ctx, receipt.Signal, Outcome{Kind: OutcomeTelegramError, EventID: receipt.EventID, PendingID: id, Reason: err.Error()})
		return err
	}
	return nil
}

func (c *Coordinator) DecideClassification(ctx context.Context, id string, allow bool) error {
	entry, ok := c.takePending(id, pendingClassification)
	if !ok {
		return fmt.Errorf("pending classification not found")
	}
	signal := entry.receipt.Signal
	c.classifier.Add(signal.Presentation.App, signal.Presentation.Normalized, allow)
	if !allow {
		_ = c.processor.Update(ctx, entry.receipt.EventID, "denied", nil, "")
		c.report(ctx, signal, Outcome{Kind: OutcomeUserDenied, EventID: entry.receipt.EventID})
		return nil
	}
	c.report(ctx, signal, Outcome{Kind: OutcomeUserAllowed, EventID: entry.receipt.EventID})
	return c.match(ctx, entry.receipt)
}

func (c *Coordinator) DecideSelection(ctx context.Context, id string, txID uint) error {
	entry, ok := c.takePending(id, pendingSelection)
	if !ok {
		return fmt.Errorf("pending selection not found")
	}
	for _, candidate := range entry.candidates {
		if candidate.ID == txID {
			c.report(ctx, entry.receipt.Signal, Outcome{Kind: OutcomePicked, EventID: entry.receipt.EventID, TxID: &candidate.ID})
			return c.complete(ctx, entry.receipt, candidate)
		}
	}
	return fmt.Errorf("candidate not found")
}

func (c *Coordinator) addPending(entry pendingEntry) string {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	id := strconv.FormatInt(time.Now().UnixNano(), 36)
	c.pending[id] = entry
	return id
}

func (c *Coordinator) takePending(id string, kind pendingKind) (pendingEntry, bool) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	entry, ok := c.pending[id]
	if !ok || entry.kind != kind {
		return pendingEntry{}, false
	}
	delete(c.pending, id)
	return entry, true
}

func (c *Coordinator) report(ctx context.Context, signal paymentevents.InboundSignal, outcome Outcome) {
	if c.reporter != nil {
		c.reporter.Report(ctx, signal, outcome)
	}
}
