package scheduler

import (
	"context"
	"time"

	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/store"
)

// expiryThresholds are the windows an expiring certificate is announced at
// (§4.3), NARROWEST FIRST. The order matters: a certificate is announced once,
// at the closest threshold it has already crossed — a certificate expiring in
// 5 days has crossed both J-7 and J-30, and deserves one alert, not two.
// Marking it at J-7 also silences the wider window, since the row records the
// smallest threshold already announced.
var expiryThresholds = []int32{7, 30}

// alertExpiringCertificates publishes certificate.expiring.v1 for the
// certificates entering a threshold. The event goes through the outbox like
// any other, so the notification rules — severity, routing, quiet hours —
// apply to it without a single line of special-casing (ADR-019).
//
// The threshold reached is recorded on the row: without it, a 30-day window
// would re-announce the same certificate on every scheduler pass.
func (s *Scheduler) alertExpiringCertificates(ctx context.Context) {
	// Narrowest threshold first: a certificate expiring in 5 days has crossed
	// BOTH J-30 and J-7, and announcing it twice in the same pass is noise. It
	// is announced once, at the closest threshold it has reached.
	for _, threshold := range expiryThresholds {
		certs, err := s.Store.ListCertificatesToAlert(ctx, threshold)
		if err != nil {
			s.Logger.Warn("certificate expiry scan failed", "error", err)
			return
		}
		for _, cert := range certs {
			if ctx.Err() != nil {
				return
			}
			daysLeft := int(time.Until(cert.NotAfter.Time).Hours() / 24)
			s.Audit.Outbox(ctx, s.Store, "certificate.expiring.v1",
				cert.TeamUuid, cert.Uuid, "certificate:"+pguuid.String(cert.Uuid),
				map[string]any{
					"certificate_uuid": pguuid.String(cert.Uuid),
					"server_uuid":      pguuid.String(cert.ServerUuid),
					"main_domain":      cert.MainDomain,
					"not_after":        cert.NotAfter.Time.UTC(),
					"days_left":        daysLeft,
					"threshold_days":   threshold,
				})
			if err := s.Store.MarkCertificateAlerted(ctx, store.MarkCertificateAlertedParams{
				ID: cert.ID, ExpiryAlertedThreshold: &threshold,
			}); err != nil {
				s.Logger.Warn("could not mark a certificate as alerted", "id", cert.ID, "error", err)
				continue
			}
			s.Logger.Warn("certificate expiring", "domain", cert.MainDomain, "days_left", daysLeft)
		}
	}
}
