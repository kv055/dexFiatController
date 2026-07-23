package paymentemail

import (
	"context"
	"fmt"
	"time"

	"dexFiatController/internal/paymentevents"
)

// MatchCallbacks are source-presentation hooks only. The neutral processor has
// already persisted, deduplicated and matched the event before either is called.
type MatchCallbacks interface {
	ConfirmEmail(context.Context, paymentevents.PaymentEvent, int64, paymentevents.Candidate, string)
	AmbiguousEmail(context.Context, paymentevents.PaymentEvent, int64, []paymentevents.Candidate, string)
}

type Ingestor struct {
	cfg       PaymentEmailConfig
	processor *paymentevents.Processor
	callbacks MatchCallbacks
}

func NewIngestor(cfg PaymentEmailConfig, processor *paymentevents.Processor, callbacks MatchCallbacks) *Ingestor {
	if cfg.Mode == "" {
		cfg.Mode = Capture
	}
	return &Ingestor{cfg: cfg, processor: processor, callbacks: callbacks}
}

// Handle is the payment-email adapter entry point used by the IMAP reader.
// Capture and shadow terminate here; active events enter the exact same neutral
// matcher and confirmation path as PushPush events.
func (i *Ingestor) Handle(ctx context.Context, uid uint32, raw []byte) error {
	received := time.Now().UTC()
	if i.cfg.Mode == Capture {
		event, decodeErr := CaptureMessage(raw, uid, received)
		status := "captured"
		if decodeErr != nil {
			status = "parse_error"
		}
		eventID, duplicate, err := i.processor.Record(ctx, event, status)
		if err != nil || duplicate {
			return err
		}
		if decodeErr != nil {
			_ = i.processor.Update(ctx, eventID, status, nil, decodeErr.Error())
		}
		return decodeErr
	}

	message, parseErr := ParseMessage(raw, received)
	status := "parsed"
	if parseErr != nil {
		status = "rejected"
	}
	eventID, duplicate, err := i.processor.Record(ctx, message.Event, status)
	if err != nil || duplicate {
		return err
	}
	if parseErr != nil {
		_ = i.processor.Update(ctx, eventID, status, nil, parseErr.Error())
		return parseErr
	}
	if i.cfg.Mode == Active &&
		(uid <= i.cfg.ActivationUID ||
			message.Event.ProviderTimestamp.IsZero() ||
			message.Event.ProviderTimestamp.Before(i.cfg.ActivationTime)) {
		err := fmt.Errorf("message is before the active UID/time watermark")
		_ = i.processor.Update(ctx, eventID, "pre_activation", nil, err.Error())
		return err
	}

	candidate, ambiguous, candidates, err := i.processor.Match(ctx, message.Event, received)
	if err != nil {
		_ = i.processor.Update(ctx, eventID, "list_error", nil, err.Error())
		return err
	}
	if i.cfg.Mode == Shadow {
		switch {
		case candidate != nil:
			_ = i.processor.Update(ctx, eventID, "shadow_matched", &candidate.ID, "")
		case len(ambiguous) > 0:
			_ = i.processor.Update(ctx, eventID, "shadow_ambiguous", nil, "")
		default:
			_ = i.processor.Update(ctx, eventID, "shadow_unmatched", nil, "")
		}
		return nil
	}
	switch {
	case candidate != nil:
		i.callbacks.ConfirmEmail(ctx, message.Event, eventID, *candidate, message.Subject)
	case len(ambiguous) > 0:
		i.callbacks.AmbiguousEmail(ctx, message.Event, eventID, ambiguous, message.Subject)
	default:
		status, reason := paymentevents.EligibilityStatus(message.Event, candidates, received)
		_ = i.processor.Update(ctx, eventID, status, nil, reason)
	}
	return nil
}
