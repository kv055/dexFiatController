package paymentevents

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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
	return p.store.Confirm(ctx, event, eventID, candidate.ID, evidence, 0, false)
}

func (p *Processor) Update(ctx context.Context, eventID int64, status string, txID *uint, errText string) error {
	return p.store.UpdateStatus(ctx, eventID, status, txID, errText)
}

type CompletionState string

const (
	CompletionWaiting   CompletionState = "waiting"
	CompletionManual    CompletionState = "manual_confirmation"
	CompletionRejected  CompletionState = "rejected"
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
	return p.complete(ctx, event, eventID, candidate, 0, false)
}

func (p *Processor) CompleteSelected(ctx context.Context, event PaymentEvent, eventID int64, candidate Candidate) (CompletionResult, error) {
	return p.complete(ctx, event, eventID, candidate, 0, true)
}

func (p *Processor) complete(ctx context.Context, event PaymentEvent, eventID int64, candidate Candidate, approvalRequestID int64, forceManual bool) (CompletionResult, error) {
	txID := candidate.ID
	confirmation, err := p.store.Confirm(ctx, event, eventID, candidate.ID, buildEvidence(event, candidate), approvalRequestID, forceManual)
	if err != nil {
		if approvalRequestID == 0 {
			_ = p.Update(ctx, eventID, "rejected", &txID, err.Error())
		}
		return CompletionResult{ConfirmationResult: confirmation, State: CompletionFailed}, err
	}
	result := CompletionResult{ConfirmationResult: confirmation}
	if confirmation.ManualRequired {
		result.State = CompletionManual
		_ = p.Update(ctx, eventID, "awaiting_user_confirmation", &txID, "")
		return result, nil
	}
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

func (p *Processor) MarkConfirmationDelivered(ctx context.Context, requestID int64, chatID string, messageID int64) error {
	return p.store.MarkConfirmationDelivered(ctx, requestID, chatID, messageID)
}

func (p *Processor) MarkConfirmationDeliveryError(ctx context.Context, requestID int64, text string) error {
	return p.store.MarkConfirmationDeliveryError(ctx, requestID, text)
}

func (p *Processor) PendingConfirmationRequests(ctx context.Context) ([]PaymentConfirmationRequest, error) {
	return p.store.PendingConfirmationRequests(ctx)
}

func (p *Processor) DecideConfirmation(ctx context.Context, requestID int64, chatID string, allow bool) (CompletionResult, PaymentConfirmationRequest, error) {
	request, err := p.store.LoadConfirmationRequest(ctx, requestID)
	if err != nil {
		return CompletionResult{}, request, err
	}
	if request.DecisionStatus != confirmationDecisionPending {
		return CompletionResult{}, request, fmt.Errorf("payment events: confirmation request is already handled")
	}
	if request.TransactionStatus != statusDexPositionTaken {
		expired, expireErr := p.store.RejectConfirmation(ctx, requestID)
		if expireErr == nil {
			request = expired
		}
		return CompletionResult{}, request, fmt.Errorf("payment events: confirmation request expired because the transaction is no longer pending")
	}
	if request.TelegramChatID != "" && strings.TrimSpace(chatID) != request.TelegramChatID {
		return CompletionResult{}, request, fmt.Errorf("payment events: confirmation callback came from the wrong Telegram chat")
	}
	if !allow {
		request, err = p.store.RejectConfirmation(ctx, requestID)
		if err != nil {
			return CompletionResult{}, request, err
		}
		return CompletionResult{State: CompletionRejected}, request, nil
	}
	result, err := p.complete(
		ctx, request.Event, request.PaymentEventID, request.Candidate, requestID, false,
	)
	return result, request, err
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
