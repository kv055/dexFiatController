package paymentevents

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

type Releaser interface {
	Release(context.Context, uint) error
}

// Processor is the source-neutral entry point shared by push and email
// adapters. It owns persistence, deduplication, candidate matching and channel
// correlation, including the final idempotent release request. Source packages
// only decode, classify and present outcomes.
type Processor struct {
	store    *EventStore
	matcher  *Matcher
	releaser Releaser
}

func NewProcessor(store *EventStore, releaser ...Releaser) *Processor {
	processor := &Processor{store: store, matcher: NewMatcher()}
	if len(releaser) > 0 {
		processor.releaser = releaser[0]
	}
	return processor
}

func (p *Processor) Record(ctx context.Context, event PaymentEvent, status string) (int64, bool, error) {
	return p.store.Insert(ctx, event, status)
}

func (p *Processor) Match(ctx context.Context, event PaymentEvent, now time.Time) (*Candidate, []Candidate, []Candidate, error) {
	candidates, err := p.store.ListPending(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	candidate, ambiguous := p.matcher.Match(event, candidates, now)
	return candidate, ambiguous, candidates, nil
}

func (p *Processor) Confirm(ctx context.Context, event PaymentEvent, eventID int64, candidate Candidate, evidence json.RawMessage) (ConfirmationResult, error) {
	return p.store.Confirm(ctx, event, eventID, candidate.ID, evidence)
}

func (p *Processor) Update(ctx context.Context, eventID int64, status string, txID *uint, errText string) error {
	return p.store.UpdateStatus(ctx, eventID, status, txID, errText)
}

type CompletionState string

const (
	CompletionWaiting   CompletionState = "waiting"
	CompletionDuplicate CompletionState = "duplicate"
	CompletionReleased  CompletionState = "released"
	CompletionFailed    CompletionState = "failed"
)

type CompletionResult struct {
	ConfirmationResult
	State CompletionState
}

// Complete records a source confirmation, evaluates the account's selected
// channels and requests release exactly when this call atomically advances the
// transaction. Both push and email adapters enter this method.
func (p *Processor) Complete(ctx context.Context, event PaymentEvent, eventID int64, candidate Candidate) (CompletionResult, error) {
	txID := candidate.ID
	confirmation, err := p.Confirm(ctx, event, eventID, candidate, buildEvidence(event, candidate))
	if err != nil {
		_ = p.Update(ctx, eventID, "rejected", &txID, err.Error())
		return CompletionResult{ConfirmationResult: confirmation, State: CompletionFailed}, err
	}
	result := CompletionResult{ConfirmationResult: confirmation}
	if !confirmation.Ready {
		result.State = CompletionWaiting
		_ = p.Update(ctx, eventID, "confirmed_waiting", &txID, "")
		return result, nil
	}
	if !confirmation.Advanced {
		result.State = CompletionDuplicate
		_ = p.Update(ctx, eventID, "duplicate_confirmation", &txID, "")
		return result, nil
	}
	if p.releaser == nil {
		err = fmt.Errorf("payment events: release client not configured")
	} else {
		err = p.releaser.Release(ctx, txID)
	}
	if err != nil {
		result.State = CompletionFailed
		_ = p.Update(ctx, eventID, "release_error", &txID, err.Error())
		return result, err
	}
	result.State = CompletionReleased
	_ = p.Update(ctx, eventID, "released", &txID, "")
	return result, nil
}

func buildEvidence(event PaymentEvent, candidate Candidate) json.RawMessage {
	document := map[string]any{
		"source": event.Source, "provider": event.Provider,
		"source_event_id": event.SourceEventID, "sender": event.Sender,
		"amount_minor": event.AmountMinor, "currency": event.Currency,
		"description": event.Description, "provider_timestamp": event.ProviderTimestamp,
		"received_at": event.ReceivedTimestamp, "raw_message_hash": event.RawMessageHash,
		"matched_tx": map[string]any{
			"id": candidate.ID, "payment_method": candidate.PaymentMethod,
		},
	}
	raw, err := json.Marshal(document)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}
