package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// infoCmd is the detail view of ONE resource. `list` answers "what do I have";
// nothing answered "what is this one doing" (ADR-070 §2), which is the question
// asked between a deployment and a page that will not load: desired against
// observed status, the health check that gates routing, the per-container
// breakdown, the URL, and what the last deployment did.
func infoCmd(k resourceKind) *cobra.Command {
	example := "  akerdock " + k.group + " info " + exampleName(k)
	if k.dirDefault {
		example += "\n  akerdock " + k.group + " info   # the application named by the .akerdock file"
	}
	example += "\n  akerdock " + k.group + " info " + exampleName(k) + " -o json"
	return &cobra.Command{
		Use:   "info [NAME]",
		Short: "Show one " + k.label + " in detail: statuses, components, last deployment",
		Long: "Everything the platform knows about one " + k.label + ", in the order it is " +
			"read when something is wrong: what was ASKED of it (desired status) against what " +
			"is OBSERVED of it, then the details that explain a gap between the two.\n\n" +
			"`-o json` prints the API objects untouched — the resource, its components and " +
			"its last deployment — for anything that needs a field this view does not show.",
		Example: example,
		Args:    targetArgs(k),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(flags.context)
			if err != nil {
				return err
			}
			res, err := c.target(cmd.Context(), k, args)
			if err != nil {
				return err
			}
			return c.showInfo(cmd.Context(), k, res.Uuid)
		},
	}
}

// resourceDetail is the projection of Application, Database and Service that
// this view renders. One struct rather than three, because the view is one: the
// fields the three schemas do not share simply stay at their zero value and
// their row is left out. It is deliberately NOT what `-o json` prints.
type resourceDetail struct {
	Uuid           string      `json:"uuid"`
	Name           string      `json:"name"`
	SourceType     string      `json:"source_type"`
	BuildPack      string      `json:"build_pack"`
	Engine         string      `json:"engine"`
	DesiredStatus  string      `json:"desired_status"`
	ObservedStatus string      `json:"observed_status"`
	ObservedAt     time.Time   `json:"observed_at"`
	Domains        []string    `json:"domains"`
	HealthCheck    healthCheck `json:"health_check"`
	// scale_asleep separates "stopped on purpose, will wake on the next request"
	// from "down" (ADR-037). Both observe as `exited`, and only this field tells
	// the reader which one they are looking at.
	ScaleAsleep bool `json:"scale_asleep"`
	// Database only: the flag that says a configuration change is waiting for
	// exactly the command sitting next to this one, `db restart`.
	RestartRequired    bool      `json:"restart_required"`
	IsPublic           bool      `json:"is_public"`
	PublicPort         int       `json:"public_port"`
	LastDeploymentUuid string    `json:"last_deployment_uuid"`
	LastDeploymentAt   time.Time `json:"last_deployment_at"`
}

// healthCheck is HealthCheckConfig (§5.3) — the check that conditions routing
// and zero-downtime, and therefore the first thing to read when a deployment
// switched traffic onto something that does not answer.
type healthCheck struct {
	Enabled         bool   `json:"enabled"`
	Path            string `json:"path"`
	Port            int    `json:"port"`
	Method          string `json:"method"`
	IntervalSeconds int    `json:"interval_seconds"`
}

// component is ServiceComponent: one sub-container of a compose stack, with its
// own observed status (data dictionary §9.2).
type component struct {
	Name           string `json:"name"`
	ObservedStatus string `json:"observed_status"`
	// exclude_from_hc marks a one-shot job (compose-spec §7.3). An `exited`
	// migration container is a success, not an outage, and without this the
	// breakdown reads as a stack half down.
	ExcludeFromHC bool `json:"exclude_from_hc"`
}

// showInfo gathers the documents the view needs and renders them.
func (c *Client) showInfo(ctx context.Context, k resourceKind, uuid string) error {
	// The resource is kept raw so `-o json` can pass the API object through byte
	// for byte, and decoded a second time into the projection the table reads.
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, k.path+"/"+uuid, nil, nil, &raw); err != nil {
		return err
	}
	var d resourceDetail
	if err := json.Unmarshal(raw, &d); err != nil {
		return err
	}
	components, err := c.listComponents(ctx, k, uuid)
	if err != nil {
		return err
	}
	lastRaw := c.lastDeployment(ctx, d.LastDeploymentUuid)

	if flags.output == "json" {
		// The envelope names the three documents this command gathered; it does
		// not rewrite any of them. A script that wants `config_version` or
		// `error_message` finds them where the API put them.
		return printJSON(struct {
			Resource       json.RawMessage   `json:"resource"`
			Components     []json.RawMessage `json:"components,omitempty"`
			LastDeployment json.RawMessage   `json:"last_deployment,omitempty"`
		}{Resource: raw, Components: components, LastDeployment: lastRaw})
	}

	rows, err := infoRows(k, d, components, lastRaw)
	if err != nil {
		return err
	}
	// A key/value block, not a one-row table: a row of fifteen columns is
	// unreadable at any terminal width, and half of these fields are absent on
	// half the kinds — a column that is empty for a database would still cost
	// its header. `table` with no header gives the two aligned columns for free.
	table(nil, rows)
	return nil
}

// infoRows lays the view out in the order it is read: identity, then the two
// statuses side by side, then what explains a gap between them.
func infoRows(k resourceKind, d resourceDetail, components []json.RawMessage, lastRaw json.RawMessage) ([][]string, error) {
	rows := [][]string{
		{"NAME", d.Name},
		{"UUID", d.Uuid},
		{"TYPE", detailType(k, d)},
		{"DESIRED", firstNonEmpty(d.DesiredStatus, "-")},
		{"OBSERVED", observedText(d)},
	}
	if k == kindApp {
		// Printed even when disabled: an application that switches traffic
		// without waiting for anything to answer is the explanation of half the
		// "the deployment succeeded and the site is down" reports, and its
		// absence shows up nowhere else in the CLI.
		rows = append(rows, []string{"HEALTH CHECK", healthCheckText(d.HealthCheck)})
	}
	if k == kindDB && d.RestartRequired {
		rows = append(rows, []string{"PENDING", "a configuration change awaits `akerdock db restart " + d.Name + "`"})
	}
	if url := infoURL(k, d); url != "" {
		rows = append(rows, []string{"URL", url})
	}
	if k == kindDB && d.IsPublic {
		// The connection URL is deliberately not printed even when the token can
		// read it: `external_url` embeds the password (INV-003), and `info` is a
		// view people paste into tickets. `akerdock db console` connects without
		// anyone reading a credential.
		port := "the configured port"
		if d.PublicPort > 0 {
			port = fmt.Sprintf("port %d", d.PublicPort)
		}
		rows = append(rows, []string{"PUBLIC", "reachable from outside on " + port})
	}
	if last, ok := lastDeploymentText(d, lastRaw); ok {
		rows = append(rows, []string{"LAST DEPLOY", last})
	}
	breakdown, err := componentRows(components)
	if err != nil {
		return nil, err
	}
	return append(rows, breakdown...), nil
}

// detailType names the kind precisely: `source_type` alone does not separate a
// nixpacks application from a compose stack of eight containers, and the build
// pack is what decides whether the COMPONENTS block below will hold anything.
func detailType(k resourceKind, d resourceDetail) string {
	switch {
	case d.SourceType != "" && d.BuildPack != "":
		return d.SourceType + " · " + d.BuildPack
	case d.SourceType != "":
		return d.SourceType
	case d.Engine != "":
		return d.Engine
	}
	return k.label
}

// observedText folds the observation into the one line that answers "is it up".
// `unknown` is the API's word for an observation gone stale (§21.2), so the
// timestamp travels with the status rather than in a row of its own — a status
// nobody dates is a status nobody can trust.
func observedText(d resourceDetail) string {
	s := firstNonEmpty(d.ObservedStatus, "unknown")
	if d.ScaleAsleep {
		s += " (asleep — scale-to-zero, wakes on the next request)"
	}
	if !d.ObservedAt.IsZero() {
		s += "   seen " + d.ObservedAt.Local().Format("2006-01-02 15:04")
	}
	return s
}

// healthCheckText renders the check as the request it performs.
func healthCheckText(h healthCheck) string {
	if !h.Enabled {
		return "disabled — traffic switches over without waiting for an answer"
	}
	target := firstNonEmpty(h.Path, "/")
	if h.Port > 0 {
		target = fmt.Sprintf(":%d%s", h.Port, target)
	}
	s := firstNonEmpty(h.Method, "GET") + " " + target
	if h.IntervalSeconds > 0 {
		s += fmt.Sprintf(" every %ds", h.IntervalSeconds)
	}
	return s
}

// infoURL returns the public address, and nothing when there is none to state.
//
// An application's `domains` is authoritative only when it is non-empty: an
// empty list means "generate one from the server's wildcard" (§4.2), and the
// generated FQDN lives on the proxy's view of the server, GET
// /servers/{uuid}/domains — behind `servers:read`, which `applications:read`
// does not imply. So the row is left out rather than filled with a guess, or
// with a request that would turn `info` into a 403 for exactly the readers
// ADR-059 gave the read path to. A compose stack routes per component from the
// file itself and carries no domain field at all.
func infoURL(k resourceKind, d resourceDetail) string {
	if k != kindApp || len(d.Domains) == 0 {
		return ""
	}
	return strings.Join(d.Domains, ", ")
}

// lastDeploymentText describes what the last deployment did. The enrichment is
// optional (see lastDeployment), so the timestamp the resource itself carries
// is the fallback: something is always better than a row that disappears
// depending on the caller's permissions.
func lastDeploymentText(d resourceDetail, lastRaw json.RawMessage) (string, bool) {
	if d.LastDeploymentUuid == "" && d.LastDeploymentAt.IsZero() {
		return "", false
	}
	when := "-"
	if !d.LastDeploymentAt.IsZero() {
		when = d.LastDeploymentAt.Local().Format("2006-01-02 15:04")
	}
	if len(lastRaw) > 0 {
		var dep deployment
		if err := json.Unmarshal(lastRaw, &dep); err == nil {
			return strings.Join([]string{dep.Status, deployTrigger(dep), deploySource(dep), when, dep.Uuid}, " · "), true
		}
	}
	return when + " · " + d.LastDeploymentUuid, true
}

// componentRows renders the per-container breakdown. Three cells rather than
// two: the component name is a column of its own, so twelve services of a stack
// read as a list instead of as twelve sentences.
func componentRows(components []json.RawMessage) ([][]string, error) {
	rows := make([][]string, 0, len(components))
	for _, item := range components {
		var comp component
		if err := json.Unmarshal(item, &comp); err != nil {
			return nil, err
		}
		status := firstNonEmpty(comp.ObservedStatus, "unknown")
		if comp.ExcludeFromHC {
			status += " (one-shot job)"
		}
		key := ""
		if len(rows) == 0 {
			key = "COMPONENTS"
		}
		rows = append(rows, []string{key, comp.Name, status})
	}
	return rows, nil
}

// listComponents reads the per-container breakdown of the kinds that have one.
//
// A database has NO components endpoint — it is one container — so none is
// called: a request invented to keep the code symmetric would turn a 404 into
// the whole view's failure. An application answers with an empty list on every
// build pack but `compose`, which is why an empty answer is a normal one here
// and not a reason to say anything.
func (c *Client) listComponents(ctx context.Context, k resourceKind, uuid string) ([]json.RawMessage, error) {
	if k != kindApp && k != kindSvc {
		return nil, nil
	}
	var page struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, k.path+"/"+uuid+"/components", nil, nil, &page); err != nil {
		return nil, err
	}
	return page.Data, nil
}

// lastDeployment fetches the deployment the resource points at, and swallows
// the failure on purpose.
//
// The detail sits behind `deployments:read`, a permission separate from the
// `applications:read` that got the caller this far — ADR-059's reviewer is
// exactly such a caller. The deployment is an ENRICHMENT of this view, not its
// subject: losing the status of the last build must not lose the statuses the
// command was run for. What survives the loss is the timestamp the resource
// carries itself (lastDeploymentText).
func (c *Client) lastDeployment(ctx context.Context, uuid string) json.RawMessage {
	if uuid == "" {
		return nil
	}
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, "/deployments/"+uuid, nil, nil, &raw); err != nil {
		return nil
	}
	return raw
}
