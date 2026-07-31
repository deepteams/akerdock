// The ten read-only tools of PRD §12, bound to the store. Every one is scoped
// to the caller's team (INV-002) and every projection is hand-written: no
// struct is echoed wholesale, so an `*_enc` column or a credential can never
// reach an assistant by accident.
package mcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/store"
)

// Store is the read surface the tools need — an interface so the tool tests
// run without PostgreSQL.
type Store interface {
	CountResourcesOnServer(context.Context, int64) (int64, error)
	ListServersPage(context.Context, store.ListServersPageParams) ([]store.Server, error)
	GetServerByUUID(context.Context, store.GetServerByUUIDParams) (store.Server, error)
	ListProjectsPage(context.Context, store.ListProjectsPageParams) ([]store.Project, error)
	ListApplicationsPage(context.Context, store.ListApplicationsPageParams) ([]store.ListApplicationsPageRow, error)
	GetApplicationByUUID(context.Context, store.GetApplicationByUUIDParams) (store.GetApplicationByUUIDRow, error)
	ListDatabasesPage(context.Context, store.ListDatabasesPageParams) ([]store.ListDatabasesPageRow, error)
	GetDatabaseByUUID(context.Context, store.GetDatabaseByUUIDParams) (store.GetDatabaseByUUIDRow, error)
	ListServiceStacksPage(context.Context, store.ListServiceStacksPageParams) ([]store.ListServiceStacksPageRow, error)
	GetServiceStackByUUID(context.Context, store.GetServiceStackByUUIDParams) (store.GetServiceStackByUUIDRow, error)
	ListServiceComponents(context.Context, int64) ([]store.ServiceComponent, error)
	ListDomainsForApplication(context.Context, *int64) ([]store.Domain, error)
}

// RegisterTools wires the ten read-only tools onto the server.
func RegisterTools(s *Server, q Store) {
	pageProps := map[string]any{"limit": IntProp("Page size, 50 by default, 100 maximum.")}

	s.Register(Tool{
		Name: "overview",
		Description: "Inventory summary of the team: how many servers, projects, applications, " +
			"databases and compose stacks, and how many of them are not healthy. Start here.",
		InputSchema: ObjectSchema(map[string]any{}),
		RequiredPermissions: []auth.Permission{
			auth.PermServersRead, auth.PermProjectsRead, auth.PermApplicationsRead,
			auth.PermDatabasesRead, auth.PermServicesRead,
		},
	}, func(ctx context.Context, teamID int64, _ map[string]any) (any, error) {
		return overview(ctx, q, teamID)
	})

	s.Register(Tool{
		Name:                "list_servers",
		Description:         "Servers of the team: host, status, proxy state, architecture, docker version.",
		InputSchema:         ObjectSchema(pageProps),
		RequiredPermissions: []auth.Permission{auth.PermServersRead},
	}, func(ctx context.Context, teamID int64, args map[string]any) (any, error) {
		rows, err := q.ListServersPage(ctx, store.ListServersPageParams{TeamID: teamID, PageLimit: PageSize(args)})
		if err != nil {
			return nil, err
		}
		out := make([]map[string]any, 0, len(rows))
		for _, s := range rows {
			out = append(out, serverView(s))
		}
		return map[string]any{"servers": out, "count": len(out)}, nil
	})

	s.Register(Tool{
		Name:                "get_server",
		Description:         "One server by uuid, with how many resources it hosts.",
		InputSchema:         ObjectSchema(map[string]any{"uuid": StringProp("Server uuid.")}, "uuid"),
		RequiredPermissions: []auth.Permission{auth.PermServersRead},
	}, func(ctx context.Context, teamID int64, args map[string]any) (any, error) {
		id, err := uuidArg(args)
		if err != nil {
			return nil, err
		}
		server, err := q.GetServerByUUID(ctx, store.GetServerByUUIDParams{Uuid: id, TeamID: teamID})
		if err != nil {
			return nil, fmt.Errorf("no server with this uuid in this team")
		}
		view := serverView(server)
		if n, err := q.CountResourcesOnServer(ctx, server.ID); err == nil {
			view["resources"] = n
		}
		return view, nil
	})

	s.Register(Tool{
		Name:                "list_projects",
		Description:         "Projects of the team and their environments count.",
		InputSchema:         ObjectSchema(pageProps),
		RequiredPermissions: []auth.Permission{auth.PermProjectsRead},
	}, func(ctx context.Context, teamID int64, args map[string]any) (any, error) {
		rows, err := q.ListProjectsPage(ctx, store.ListProjectsPageParams{TeamID: teamID, PageLimit: PageSize(args)})
		if err != nil {
			return nil, err
		}
		out := make([]map[string]any, 0, len(rows))
		for _, p := range rows {
			out = append(out, map[string]any{
				"uuid":        pguuid.String(p.Uuid),
				"name":        p.Name,
				"description": p.Description,
			})
		}
		return map[string]any{"projects": out, "count": len(out)}, nil
	})

	s.Register(Tool{
		Name: "list_applications",
		Description: "Applications of the team: desired and observed status, source, server, " +
			"and whether scale-to-zero or an access wall is enabled.",
		InputSchema:         ObjectSchema(pageProps),
		RequiredPermissions: []auth.Permission{auth.PermApplicationsRead},
	}, func(ctx context.Context, teamID int64, args map[string]any) (any, error) {
		rows, err := q.ListApplicationsPage(ctx, store.ListApplicationsPageParams{TeamID: teamID, PageLimit: PageSize(args)})
		if err != nil {
			return nil, err
		}
		out := make([]map[string]any, 0, len(rows))
		for _, a := range rows {
			out = append(out, map[string]any{
				"uuid":            pguuid.String(a.Resource.Uuid),
				"name":            a.Resource.Name,
				"desired_status":  string(a.Resource.DesiredStatus),
				"observed_status": string(a.Resource.ObservedStatus),
				"build_pack":      string(a.BuildConfig.BuildPack),
				"server_uuid":     pguuid.String(a.ServerUuid),
				"project_uuid":    pguuid.String(a.ProjectUuid),
			})
		}
		return map[string]any{"applications": out, "count": len(out)}, nil
	})

	s.Register(Tool{
		Name: "get_application",
		Description: "One application by uuid: status, source, routing domains, compose components " +
			"with their observed state, previews and scale-to-zero settings. Never returns " +
			"environment variable values.",
		InputSchema:         ObjectSchema(map[string]any{"uuid": StringProp("Application uuid.")}, "uuid"),
		RequiredPermissions: []auth.Permission{auth.PermApplicationsRead},
	}, func(ctx context.Context, teamID int64, args map[string]any) (any, error) {
		id, err := uuidArg(args)
		if err != nil {
			return nil, err
		}
		app, err := q.GetApplicationByUUID(ctx, store.GetApplicationByUUIDParams{Uuid: id, TeamID: teamID})
		if err != nil {
			return nil, fmt.Errorf("no application with this uuid in this team")
		}
		view := map[string]any{
			"uuid":              pguuid.String(app.Resource.Uuid),
			"name":              app.Resource.Name,
			"description":       app.Resource.Description,
			"desired_status":    string(app.Resource.DesiredStatus),
			"observed_status":   string(app.Resource.ObservedStatus),
			"build_pack":        string(app.BuildConfig.BuildPack),
			"git_repository":    app.Application.GitRepositoryUrl,
			"git_branch":        app.Application.GitBranch,
			"auto_deploy":       app.Application.AutoDeployEnabled,
			"previews_enabled":  app.Application.PreviewsEnabled,
			"scale_to_zero":     app.Application.ScaleToZero,
			"asleep":            app.Application.ScaleSleptAt.Valid,
			"access_protection": string(app.Application.AccessProtection),
			"server_uuid":       pguuid.String(app.ServerUuid),
			"project_uuid":      pguuid.String(app.ProjectUuid),
			"environment_uuid":  pguuid.String(app.EnvironmentUuid),
		}
		if domains, err := q.ListDomainsForApplication(ctx, &app.Resource.ID); err == nil {
			hosts := make([]string, 0, len(domains))
			for _, d := range domains {
				hosts = append(hosts, d.Fqdn)
			}
			view["domains"] = hosts
		}
		if comps, err := q.ListServiceComponents(ctx, app.Resource.ID); err == nil && len(comps) > 0 {
			view["components"] = componentViews(comps)
		}
		return view, nil
	})

	s.Register(Tool{
		Name:                "list_databases",
		Description:         "Managed databases of the team: engine, version, status, server. No credentials.",
		InputSchema:         ObjectSchema(pageProps),
		RequiredPermissions: []auth.Permission{auth.PermDatabasesRead},
	}, func(ctx context.Context, teamID int64, args map[string]any) (any, error) {
		rows, err := q.ListDatabasesPage(ctx, store.ListDatabasesPageParams{TeamID: teamID, PageLimit: PageSize(args)})
		if err != nil {
			return nil, err
		}
		out := make([]map[string]any, 0, len(rows))
		for _, d := range rows {
			out = append(out, map[string]any{
				"uuid":            pguuid.String(d.Resource.Uuid),
				"name":            d.Resource.Name,
				"engine":          string(d.Database.Engine),
				"image_tag":       d.Database.ImageTag,
				"desired_status":  string(d.Resource.DesiredStatus),
				"observed_status": string(d.Resource.ObservedStatus),
				"server_uuid":     pguuid.String(d.ServerUuid),
			})
		}
		return map[string]any{"databases": out, "count": len(out)}, nil
	})

	s.Register(Tool{
		Name:                "get_database",
		Description:         "One managed database by uuid. Credentials are never returned.",
		InputSchema:         ObjectSchema(map[string]any{"uuid": StringProp("Database uuid.")}, "uuid"),
		RequiredPermissions: []auth.Permission{auth.PermDatabasesRead},
	}, func(ctx context.Context, teamID int64, args map[string]any) (any, error) {
		id, err := uuidArg(args)
		if err != nil {
			return nil, err
		}
		db, err := q.GetDatabaseByUUID(ctx, store.GetDatabaseByUUIDParams{Uuid: id, TeamID: teamID})
		if err != nil {
			return nil, fmt.Errorf("no database with this uuid in this team")
		}
		return map[string]any{
			"uuid":            pguuid.String(db.Resource.Uuid),
			"name":            db.Resource.Name,
			"engine":          string(db.Database.Engine),
			"image_tag":       db.Database.ImageTag,
			"desired_status":  string(db.Resource.DesiredStatus),
			"observed_status": string(db.Resource.ObservedStatus),
			"public_port":     db.Database.PublicPort,
			"server_uuid":     pguuid.String(db.ServerUuid),
			"project_uuid":    pguuid.String(db.ProjectUuid),
		}, nil
	})

	s.Register(Tool{
		Name:                "list_services",
		Description:         "Compose stacks of the team: status and server.",
		InputSchema:         ObjectSchema(pageProps),
		RequiredPermissions: []auth.Permission{auth.PermServicesRead},
	}, func(ctx context.Context, teamID int64, args map[string]any) (any, error) {
		rows, err := q.ListServiceStacksPage(ctx, store.ListServiceStacksPageParams{TeamID: teamID, PageLimit: PageSize(args)})
		if err != nil {
			return nil, err
		}
		out := make([]map[string]any, 0, len(rows))
		for _, s := range rows {
			out = append(out, map[string]any{
				"uuid":            pguuid.String(s.Resource.Uuid),
				"name":            s.Resource.Name,
				"desired_status":  string(s.Resource.DesiredStatus),
				"observed_status": string(s.Resource.ObservedStatus),
				"server_uuid":     pguuid.String(s.ServerUuid),
			})
		}
		return map[string]any{"services": out, "count": len(out)}, nil
	})

	s.Register(Tool{
		Name:                "get_service",
		Description:         "One compose stack by uuid, with its components and their observed state.",
		InputSchema:         ObjectSchema(map[string]any{"uuid": StringProp("Service (compose stack) uuid.")}, "uuid"),
		RequiredPermissions: []auth.Permission{auth.PermServicesRead},
	}, func(ctx context.Context, teamID int64, args map[string]any) (any, error) {
		id, err := uuidArg(args)
		if err != nil {
			return nil, err
		}
		svc, err := q.GetServiceStackByUUID(ctx, store.GetServiceStackByUUIDParams{Uuid: id, TeamID: teamID})
		if err != nil {
			return nil, fmt.Errorf("no compose stack with this uuid in this team")
		}
		view := map[string]any{
			"uuid":            pguuid.String(svc.Resource.Uuid),
			"name":            svc.Resource.Name,
			"description":     svc.Resource.Description,
			"desired_status":  string(svc.Resource.DesiredStatus),
			"observed_status": string(svc.Resource.ObservedStatus),
			"server_uuid":     pguuid.String(svc.ServerUuid),
			"project_uuid":    pguuid.String(svc.ProjectUuid),
		}
		if comps, err := q.ListServiceComponents(ctx, svc.Resource.ID); err == nil {
			view["components"] = componentViews(comps)
		}
		return view, nil
	})
}

// overview counts what the team has and what is not healthy — the first
// question an assistant asks, answered without walking every list.
func overview(ctx context.Context, q Store, teamID int64) (any, error) {
	servers, err := q.ListServersPage(ctx, store.ListServersPageParams{TeamID: teamID, PageLimit: MaxPageSize})
	if err != nil {
		return nil, err
	}
	apps, err := q.ListApplicationsPage(ctx, store.ListApplicationsPageParams{TeamID: teamID, PageLimit: MaxPageSize})
	if err != nil {
		return nil, err
	}
	dbs, err := q.ListDatabasesPage(ctx, store.ListDatabasesPageParams{TeamID: teamID, PageLimit: MaxPageSize})
	if err != nil {
		return nil, err
	}
	stacks, err := q.ListServiceStacksPage(ctx, store.ListServiceStacksPageParams{TeamID: teamID, PageLimit: MaxPageSize})
	if err != nil {
		return nil, err
	}
	projects, err := q.ListProjectsPage(ctx, store.ListProjectsPageParams{TeamID: teamID, PageLimit: MaxPageSize})
	if err != nil {
		return nil, err
	}

	unhealthy := []map[string]any{}
	note := func(kind string, uuid pgtype.UUID, name string, observed store.ResourceObservedStatus, desired store.ResourceDesiredStatus) {
		// "Not healthy" means: meant to run, and not observed healthy. A
		// deliberately stopped resource is not a problem to report.
		if desired == store.ResourceDesiredStatusRunning && observed != store.ResourceObservedStatusHealthy {
			unhealthy = append(unhealthy, map[string]any{
				"kind": kind, "uuid": pguuid.String(uuid), "name": name, "observed_status": string(observed),
			})
		}
	}
	asleep := 0
	for _, a := range apps {
		note("application", a.Resource.Uuid, a.Resource.Name, a.Resource.ObservedStatus, a.Resource.DesiredStatus)
		if a.Application.ScaleSleptAt.Valid {
			asleep++
		}
	}
	for _, d := range dbs {
		note("database", d.Resource.Uuid, d.Resource.Name, d.Resource.ObservedStatus, d.Resource.DesiredStatus)
	}
	for _, s := range stacks {
		note("service", s.Resource.Uuid, s.Resource.Name, s.Resource.ObservedStatus, s.Resource.DesiredStatus)
	}
	serversDown := 0
	for _, s := range servers {
		if s.Status != store.ServerStatusReady {
			serversDown++
		}
	}
	return map[string]any{
		"counts": map[string]any{
			"servers": len(servers), "projects": len(projects), "applications": len(apps),
			"databases": len(dbs), "services": len(stacks),
		},
		"servers_not_ready":      serversDown,
		"applications_asleep":    asleep,
		"unhealthy_resources":    unhealthy,
		"unhealthy_count":        len(unhealthy),
		"counts_capped_at":       MaxPageSize,
		"secrets_and_env_values": "never exposed through MCP",
	}, nil
}

func serverView(s store.Server) map[string]any {
	return map[string]any{
		"uuid":                  pguuid.String(s.Uuid),
		"name":                  s.Name,
		"host":                  s.Host,
		"status":                string(s.Status),
		"is_build_server":       s.IsBuildServer,
		"is_localhost":          s.IsLocalhost,
		"proxy_type":            string(s.ProxyType),
		"proxy_observed_status": string(s.ProxyObservedStatus),
		"architecture":          s.Architecture,
		"docker_version":        s.DockerVersion,
	}
}

func componentViews(comps []store.ServiceComponent) []map[string]any {
	out := make([]map[string]any, 0, len(comps))
	for _, c := range comps {
		out = append(out, map[string]any{
			"name":            c.Name,
			"image":           c.Image,
			"is_database":     c.IsDatabase,
			"observed_status": string(c.ObservedStatus),
		})
	}
	return out
}

func uuidArg(args map[string]any) (pgtype.UUID, error) {
	var id pgtype.UUID
	raw, err := RequireUUID(args, "uuid")
	if err != nil {
		return id, err
	}
	if err := id.Scan(raw); err != nil {
		return id, errors.New("uuid must be a uuid")
	}
	return id, nil
}
