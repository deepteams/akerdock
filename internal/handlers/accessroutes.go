package handlers

import (
	"encoding/json"
	"fmt"

	"github.com/deepteams/akerdock/internal/accessroute"
	"github.com/deepteams/akerdock/internal/api"
)

func normalizeAPIPublicRoutes(input []api.AccessPublicRoute, field string) ([]accessroute.Route, []api.ErrorDetail) {
	routes := make([]accessroute.Route, 0, len(input))
	var details []api.ErrorDetail
	for i, item := range input {
		match := accessroute.MatchExact
		if item.Match != nil {
			match = accessroute.Match(*item.Match)
		}
		parameters := map[string][]string(nil)
		if item.Parameters != nil {
			parameters = *item.Parameters
		}
		route, err := accessroute.Validate(accessroute.Route{
			Path: item.Path, Match: match, Methods: item.Methods, Parameters: parameters,
		})
		if err != nil {
			details = append(details, api.ErrorDetail{
				Field: ptr(fmt.Sprintf("%s[%d]", field, i)), Code: ptr("invalid"),
				Message: err.Error(),
			})
			continue
		}
		routes = append(routes, route)
	}
	return routes, details
}

func marshalPublicRoutes(routes []accessroute.Route) ([]byte, error) {
	if len(routes) == 0 {
		return []byte("[]"), nil
	}
	return json.Marshal(routes)
}

func publicRoutesToAPI(raw []byte) *[]api.AccessPublicRoute {
	if len(raw) == 0 {
		return ptr([]api.AccessPublicRoute{})
	}
	var routes []accessroute.Route
	if err := json.Unmarshal(raw, &routes); err != nil {
		return ptr([]api.AccessPublicRoute{})
	}
	out := make([]api.AccessPublicRoute, 0, len(routes))
	for _, route := range routes {
		match := api.AccessPublicRouteMatch(route.Match)
		item := api.AccessPublicRoute{
			Path: route.Path, Match: &match, Methods: route.Methods,
		}
		if len(route.Parameters) > 0 {
			item.Parameters = &route.Parameters
		}
		out = append(out, item)
	}
	return &out
}
