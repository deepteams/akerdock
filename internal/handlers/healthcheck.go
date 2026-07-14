package handlers

import (
	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/store"
)

// healthCheckParams maps the API health check config to the store row,
// applying the OpenAPI defaults (§5.3). A Dockerfile HEALTHCHECK stays
// authoritative over this configuration.
func healthCheckParams(resourceID int64, hc api.HealthCheckConfig) store.UpsertHealthCheckParams {
	p := store.UpsertHealthCheckParams{
		ResourceID:         resourceID,
		Enabled:            hc.Enabled != nil && *hc.Enabled,
		Method:             "GET",
		Path:               "/",
		IntervalSeconds:    30,
		TimeoutSeconds:     5,
		Retries:            3,
		StartPeriodSeconds: 5,
	}
	if hc.Method != nil && *hc.Method != "" {
		p.Method = *hc.Method
	}
	if hc.Path != nil && *hc.Path != "" {
		p.Path = *hc.Path
	}
	if hc.Port != nil {
		p.Port = ptr(int32(*hc.Port))
	}
	if hc.IntervalSeconds != nil && *hc.IntervalSeconds > 0 {
		p.IntervalSeconds = int32(*hc.IntervalSeconds)
	}
	if hc.TimeoutSeconds != nil && *hc.TimeoutSeconds > 0 {
		p.TimeoutSeconds = int32(*hc.TimeoutSeconds)
	}
	if hc.Retries != nil && *hc.Retries > 0 {
		p.Retries = int32(*hc.Retries)
	}
	if hc.StartPeriodSeconds != nil && *hc.StartPeriodSeconds >= 0 {
		p.StartPeriodSeconds = int32(*hc.StartPeriodSeconds)
	}
	return p
}
