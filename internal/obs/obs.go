// Package obs holds dexFiatController's domain Prometheus instruments.
//
// Instruments are package-level so call sites (pushpush pipeline, scraping
// monitor) can record without threading a registry through constructors; main
// registers them onto the commons service registry via Register.
package obs

import (
	"github.com/prometheus/client_golang/prometheus"
)

var (
	// FiatPushPipeline counts fiat push-notification pipeline transitions by
	// the fiat_tx_push_logs status they advance to (received, matched states,
	// released, error states, ...).
	FiatPushPipeline = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "dex_fiat_push_pipeline_total",
			Help: "Fiat push-notification pipeline transitions by resulting status.",
		},
		[]string{"status"},
	)

	// ScrapeLoginErrors counts failed login rounds of the scraping connectors.
	ScrapeLoginErrors = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "dex_scrape_login_errors_total",
			Help: "Scraping-connector login rounds that reported at least one failure.",
		},
	)

	// ScrapeTransactionsSeen counts transactions observed by the scraping monitor.
	ScrapeTransactionsSeen = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "dex_scrape_transactions_seen_total",
			Help: "Transactions observed by the scraping monitor, by connector.",
		},
		[]string{"service"},
	)
)

// Register attaches all package instruments to reg.
func Register(reg prometheus.Registerer) {
	reg.MustRegister(
		FiatPushPipeline,
		ScrapeLoginErrors,
		ScrapeTransactionsSeen,
	)
}
