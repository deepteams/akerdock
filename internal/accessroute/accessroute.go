// Package accessroute validates and renders the deliberately small language
// used to publish selected paths through an otherwise protected resource.
package accessroute

import (
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
)

// Match is the path matching mode of a public route.
type Match string

// The three matching modes of ADR-049: an exact path, a template with
// {placeholders}, or a prefix subtree.
const (
	MatchExact    Match = "exact"
	MatchTemplate Match = "template"
	MatchPrefix   Match = "prefix"
)

const maxPathBytes = 2048

var (
	parameterName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	methodToken   = regexp.MustCompile(`^[A-Z][A-Z0-9!#$%&'*+.^_` + "`" + `|~-]*$`)
	segmentValue  = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)
)

// Route is one unauthenticated path exception. Parameters is only meaningful
// for template matches and optionally restricts a :name placeholder.
type Route struct {
	Path       string              `json:"path"`
	Match      Match               `json:"match"`
	Methods    []string            `json:"methods"`
	Parameters map[string][]string `json:"parameters,omitempty"`
}

// Validate returns a deterministic, normalized route or an error suitable for
// API/Compose validation. It never accepts a user-supplied regular expression.
func Validate(in Route) (Route, error) {
	out := in
	if out.Match == "" {
		out.Match = MatchExact
	}
	switch out.Match {
	case MatchExact, MatchTemplate, MatchPrefix:
	default:
		return Route{}, fmt.Errorf("match must be exact, template or prefix")
	}
	if err := validatePath(out.Path); err != nil {
		return Route{}, err
	}
	if len(out.Methods) == 0 {
		return Route{}, fmt.Errorf("methods must contain at least one HTTP method")
	}
	seenMethods := map[string]struct{}{}
	out.Methods = make([]string, 0, len(in.Methods))
	for _, raw := range in.Methods {
		method := strings.ToUpper(strings.TrimSpace(raw))
		if method == "" || !methodToken.MatchString(method) {
			return Route{}, fmt.Errorf("invalid HTTP method %q", raw)
		}
		if _, ok := seenMethods[method]; ok {
			continue
		}
		seenMethods[method] = struct{}{}
		out.Methods = append(out.Methods, method)
	}
	sort.Strings(out.Methods)

	placeholders, err := validateMatchPath(out.Path, out.Match)
	if err != nil {
		return Route{}, err
	}
	if len(out.Parameters) > 0 && out.Match != MatchTemplate {
		return Route{}, fmt.Errorf("parameters are only valid with template matching")
	}
	if len(out.Parameters) > 0 {
		normalized := make(map[string][]string, len(out.Parameters))
		for name, values := range out.Parameters {
			if _, ok := placeholders[name]; !ok {
				return Route{}, fmt.Errorf("parameter %q has no matching :%s segment", name, name)
			}
			if len(values) == 0 {
				return Route{}, fmt.Errorf("parameter %q must allow at least one value", name)
			}
			seen := map[string]struct{}{}
			for _, value := range values {
				if value == "." || value == ".." || !segmentValue.MatchString(value) {
					return Route{}, fmt.Errorf("parameter %q has invalid URL-segment value %q", name, value)
				}
				seen[value] = struct{}{}
			}
			normalized[name] = make([]string, 0, len(seen))
			for value := range seen {
				normalized[name] = append(normalized[name], value)
			}
			sort.Strings(normalized[name])
		}
		out.Parameters = normalized
	}
	return out, nil
}

func validatePath(value string) error {
	switch {
	case value == "":
		return fmt.Errorf("path is required")
	case len(value) > maxPathBytes:
		return fmt.Errorf("path exceeds %d bytes", maxPathBytes)
	case !strings.HasPrefix(value, "/"):
		return fmt.Errorf("path must start with /")
	case strings.ContainsAny(value, "?#%`*\\"):
		return fmt.Errorf("path must not contain a query, fragment, percent escape, wildcard, backslash or backtick")
	case strings.IndexFunc(value, func(r rune) bool { return r <= 0x20 || r == 0x7f }) >= 0:
		return fmt.Errorf("path must not contain whitespace or control characters")
	case strings.Contains(value, "//"):
		return fmt.Errorf("path must not contain an empty segment")
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "." || segment == ".." {
			return fmt.Errorf("path must not contain . or .. segments")
		}
	}
	return nil
}

func validateMatchPath(value string, match Match) (map[string]struct{}, error) {
	placeholders := map[string]struct{}{}
	for _, segment := range strings.Split(strings.TrimPrefix(value, "/"), "/") {
		if !strings.HasPrefix(segment, ":") {
			if strings.Contains(segment, ":") {
				return nil, fmt.Errorf("a template placeholder must occupy its whole segment")
			}
			continue
		}
		if match != MatchTemplate {
			return nil, fmt.Errorf(":name placeholders require template matching")
		}
		name := strings.TrimPrefix(segment, ":")
		if !parameterName.MatchString(name) {
			return nil, fmt.Errorf("invalid template parameter %q", name)
		}
		placeholders[name] = struct{}{}
	}
	if match == MatchTemplate && len(placeholders) == 0 {
		return nil, fmt.Errorf("template matching requires at least one :name segment")
	}
	return placeholders, nil
}

// PathExpression renders the provider-independent route as a Traefik rule
// matcher. Input has already passed Validate, so every interpolated value is
// constrained and no arbitrary expression can reach the proxy configuration.
func PathExpression(route Route) string {
	switch route.Match {
	case MatchPrefix:
		if route.Path == "/" || strings.HasSuffix(route.Path, "/") {
			return fmt.Sprintf("PathPrefix(`%s`)", route.Path)
		}
		return fmt.Sprintf("(Path(`%s`) || PathPrefix(`%s/`))", route.Path, route.Path)
	case MatchTemplate:
		var expression strings.Builder
		expression.WriteString("^")
		segments := strings.Split(route.Path, "/")
		for i, segment := range segments {
			if i > 0 {
				expression.WriteString("/")
			}
			if !strings.HasPrefix(segment, ":") {
				expression.WriteString(regexp.QuoteMeta(segment))
				continue
			}
			name := strings.TrimPrefix(segment, ":")
			values := route.Parameters[name]
			if len(values) == 0 {
				// Deliberately unreserved URL characters only: accepting `%`
				// here would let an encoded slash cross a one-segment boundary
				// depending on where a proxy/backend performs decoding.
				expression.WriteString("[A-Za-z0-9._~-]+")
			} else {
				expression.WriteString("(")
				for n, value := range values {
					if n > 0 {
						expression.WriteString("|")
					}
					expression.WriteString(regexp.QuoteMeta(value))
				}
				expression.WriteString(")")
			}
		}
		expression.WriteString("$")
		return fmt.Sprintf("PathRegexp(`%s`)", expression.String())
	default:
		return fmt.Sprintf("Path(`%s`)", route.Path)
	}
}

// MethodExpression renders the explicit method allow-list.
func MethodExpression(methods []string) string {
	if len(methods) == 1 {
		return fmt.Sprintf("Method(`%s`)", methods[0])
	}
	parts := make([]string, 0, len(methods))
	for _, method := range methods {
		parts = append(parts, fmt.Sprintf("Method(`%s`)", method))
	}
	return "(" + strings.Join(parts, " || ") + ")"
}

// Covers reports whether a resource route rooted at base can serve the public
// route at all. It is intentionally conservative for templates.
func Covers(base string, public Route) bool {
	if base == "" || base == "/" {
		return true
	}
	base = strings.TrimSuffix(base, "/")
	path := public.Path
	if i := strings.Index(path, "/:"); i >= 0 {
		path = path[:i]
	}
	return path == base || strings.HasPrefix(path, base+"/")
}

// IsNavigationMethod is kept here so UI/API validation and future policy
// checks share normal HTTP method semantics.
func IsNavigationMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead
}
