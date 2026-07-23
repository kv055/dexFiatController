package paymentevents

import "time"

// Presentation contains source-neutral text used for audit logging and human
// escalation. It deliberately excludes raw payloads and MIME bodies.
type Presentation struct {
	App        string
	Title      string
	Text       string
	Normalized string
}

// Validation carries evidence produced by a source adapter. Adapters perform
// source-specific parsing and verification; the caller decides whether that
// evidence is sufficient to enter matching.
type Validation struct {
	Accepted bool
	Reason   string
}

// Rollout controls whether an email signal is capture-only, shadow evaluated,
// or release-capable. Push signals leave this empty.
type Rollout struct {
	Mode           string
	UID            uint32
	ActivationUID  uint32
	ActivationTime time.Time
}

// InboundSignal is the output contract shared by source adapters and consumed
// by the neutral payment-ingestion coordinator.
type InboundSignal struct {
	Event        PaymentEvent
	Presentation Presentation
	Validation   Validation
	Rollout      Rollout
	AuditID      int64
}
