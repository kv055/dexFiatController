package pushpush

import (
	"strconv"
	"sync"
	"time"

	"dexFiatController/internal/paymentevents"
)

// pendingKind distinguishes the two escalation flows.
type pendingKind string

const (
	pendingClassify pendingKind = "classify" // yes/no: is this a payment?
	pendingPick     pendingKind = "pick"     // pick which transaction this payment is for
)

// PendingEntry is a notification awaiting a Telegram decision.
type PendingEntry struct {
	Notification Notification
	Event        PaymentEvent
	EventID      int64
	Normalized   string
	Candidates   []paymentevents.Candidate
	Kind         pendingKind
	LogID        int64 // fiat_tx_push_logs row this escalation belongs to (0 if none)
}

// PendingStore holds notifications awaiting a human decision via Telegram. Entries are
// consumed on first Get.
type PendingStore struct {
	mu   sync.Mutex
	data map[string]PendingEntry
}

// NewPendingStore creates an empty pending store.
func NewPendingStore() *PendingStore {
	return &PendingStore{data: map[string]PendingEntry{}}
}

// Add records an entry and returns a short id (base36 nanos) safe for Telegram
// callback_data (which is capped at 64 bytes).
func (p *PendingStore) Add(e PendingEntry) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	id := strconv.FormatInt(time.Now().UnixNano(), 36)
	p.data[id] = e
	return id
}

// Get returns and removes the entry for id.
func (p *PendingStore) Get(id string) (PendingEntry, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.data[id]
	if ok {
		delete(p.data, id)
	}
	return e, ok
}
