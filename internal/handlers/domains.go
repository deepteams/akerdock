package handlers

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/store"
)

// fqdnFormat mirrors the domains table CHECK (§23.3).
var fqdnFormat = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)

type domainSpec struct {
	FQDN       string
	Path       string
	TargetPort *int32
}

// parseDomain accepts the §4.2 element formats: fqdn, fqdn:port (internal
// target port) and fqdn/path — an optional https:// scheme is tolerated.
func parseDomain(raw string) (domainSpec, error) {
	spec := domainSpec{Path: "/"}
	s := strings.TrimSpace(strings.ToLower(raw))
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	if host, path, ok := strings.Cut(s, "/"); ok {
		s = host
		spec.Path = "/" + strings.TrimSuffix(path, "/")
		if spec.Path == "" {
			spec.Path = "/"
		}
	}
	if host, port, ok := strings.Cut(s, ":"); ok {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return spec, fmt.Errorf("invalid port in domain %q", raw)
		}
		spec.TargetPort = ptr(int32(n))
		s = host
	}
	if !fqdnFormat.MatchString(s) {
		return spec, fmt.Errorf("invalid domain %q", raw)
	}
	spec.FQDN = s
	return spec, nil
}

// formatDomain renders a domains row back to its API element form.
func formatDomain(d store.Domain) string {
	out := d.Fqdn
	if d.TargetPort != nil {
		out += ":" + strconv.Itoa(int(*d.TargetPort))
	}
	if d.Path != "/" {
		out += d.Path
	}
	return out
}

// withDomains attaches the domains and tags to a single-application
// response.
func (a *API) withDomains(r *http.Request, app api.Application, resourceID int64) api.Application {
	if rows, err := a.Store.ListDomainsForApplication(r.Context(), ptr(resourceID)); err == nil {
		list := make([]string, 0, len(rows))
		for _, d := range rows {
			list = append(list, formatDomain(d))
		}
		app.Domains = &list
	}
	if tags, err := a.Store.ListTagsForResource(r.Context(), resourceID); err == nil {
		list := make([]string, 0, len(tags))
		list = append(list, tags...)
		app.Tags = &list
	}
	return app
}
