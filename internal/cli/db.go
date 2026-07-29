package cli

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// engineClient describes how to reach a database engine from the CLI.
type engineClient struct {
	port int
	bin  string
	// args builds the client argv given host, port and the DB detail map.
	args func(host string, port int, detail map[string]any) []string
	// env builds extra environment (e.g. PGPASSWORD) from the detail map.
	env func(detail map[string]any) []string
	// needsUser is true for engines whose client cannot connect without a
	// username (postgres, mysql) — we refuse to auto-launch when it is missing.
	needsUser bool
}

func str(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

// engines maps a normalized engine to its local client recipe. Credentials are
// best-effort from the database detail; the client prompts otherwise.
var engines = map[string]engineClient{
	"postgres": {
		port: 5432, bin: "psql", needsUser: true,
		args: func(h string, p int, d map[string]any) []string {
			db := str(d, "database")
			if db == "" {
				db = str(d, "username") // psql defaults the dbname to the user
			}
			return []string{"-h", h, "-p", strconv.Itoa(p), "-U", str(d, "username"), db}
		},
		env: func(d map[string]any) []string { return []string{"PGPASSWORD=" + str(d, "password")} },
	},
	"mysql": {
		port: 3306, bin: "mysql", needsUser: true,
		args: func(h string, p int, d map[string]any) []string {
			return []string{"-h", h, "-P", strconv.Itoa(p), "-u", str(d, "username"), str(d, "database")}
		},
		env: func(d map[string]any) []string { return []string{"MYSQL_PWD=" + str(d, "password")} },
	},
	"redis": {
		port: 6379, bin: "redis-cli",
		args: func(h string, p int, _ map[string]any) []string { return []string{"-h", h, "-p", strconv.Itoa(p)} },
		env:  func(d map[string]any) []string { return []string{"REDISCLI_AUTH=" + str(d, "password")} },
	},
	"mongo": {
		port: 27017, bin: "mongosh",
		args: func(h string, p int, _ map[string]any) []string {
			return []string{fmt.Sprintf("mongodb://%s:%d", h, p)}
		},
		env: func(_ map[string]any) []string { return nil },
	},
}

// engineRecipe normalizes an engine name (compose services report the long form,
// e.g. "postgresql"; standalone databases report the same) to a client recipe.
func engineRecipe(engine string) (engineClient, bool) {
	switch engine {
	case "postgres", "postgresql":
		return engines["postgres"], true
	case "mysql", "mariadb":
		return engines["mysql"], true
	case "redis", "keydb", "dragonfly":
		return engines["redis"], true
	case "mongo", "mongodb":
		return engines["mongo"], true
	default:
		return engineClient{}, false
	}
}

func dbCmd() *cobra.Command {
	var component string
	var pr int
	cmd := &cobra.Command{
		Use:   "db REF",
		Short: "Open a database console (port-forward + local client)",
		Example: "  akerdock db db/pg\n" +
			"  akerdock db app/varuna -c postgres          # a compose db service\n" +
			"  akerdock db app/varuna -c postgres --pr 8   # in a PR preview",
		Args: usageArgs(1, "db <ref>", "db db/pg"),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(flags.context)
			if err != nil {
				return err
			}
			// Same default as logs/shell/port-forward: a `.akerdock` naming a
			// component applies here too. Without this, `db` was the one command
			// that ignored it — an exception nobody could have guessed.
			component = defaultComponent(component)
			r, err := parseRef(args[0])
			if err != nil {
				return err
			}

			// Resolve the engine, the port-forward mint path and (best-effort)
			// the connection detail for the target — a standalone database or a
			// database service inside a compose stack (optionally a PR preview).
			var engineName, mintPath string
			var detail map[string]any
			switch r.kind {
			case "databases":
				if pr > 0 {
					return fmt.Errorf("--pr only applies to an app/… reference")
				}
				res, err := c.resolve(cmd.Context(), r)
				if err != nil {
					return err
				}
				engineName = res.Engine
				mintPath = "/databases/" + res.Uuid + "/port-forwards"
				detail = map[string]any{}
				_ = c.do(cmd.Context(), http.MethodGet, "/databases/"+res.Uuid, nil, nil, &detail)
			case "apps":
				if component == "" {
					return fmt.Errorf("a compose stack has several services — name the database with -c (e.g. -c postgres)")
				}
				res, err := c.resolve(cmd.Context(), r)
				if err != nil {
					return err
				}
				engineName, err = c.componentEngine(cmd.Context(), res.Uuid, component)
				if err != nil {
					return err
				}
				preview := false
				if pr > 0 {
					p, err := c.resolvePreview(cmd.Context(), res.Uuid, pr)
					if err != nil {
						return err
					}
					mintPath = "/applications/" + res.Uuid + "/previews/" + p.Uuid + "/port-forwards"
					preview = true
				} else {
					mintPath = "/applications/" + res.Uuid + "/port-forwards"
				}
				detail = c.componentCreds(cmd.Context(), res.Uuid, component, preview)
			default:
				return fmt.Errorf("db expects a db/… or app/… reference")
			}

			eng, ok := engineRecipe(engineName)
			if !ok {
				return fmt.Errorf("unsupported engine %q — use `akerdock port-forward` instead", engineName)
			}

			// Pick a free local port and forward it to the engine port.
			localPort, err := freePort()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()
			ready := make(chan error, 1)
			go func() { ready <- c.runPortForward(ctx, mintPath, component, localPort, eng.port) }()

			// No credentials the engine needs (redacted without read:sensitive):
			// forward the port and let the operator connect with their own.
			if eng.needsUser && str(detail, "username") == "" {
				fmt.Fprintf(os.Stderr,
					"credentials are redacted for this token — forwarding 127.0.0.1:%d.\n"+
						"connect with your database user (set the password env yourself), e.g.:\n"+
						"  %s -h 127.0.0.1 -p %d -U <user> <db>\n"+
						"tip: `akerdock login --scopes read,write,read:sensitive` lets db auto-launch.\n",
					localPort, eng.bin, localPort)
				return <-ready
			}
			if _, err := exec.LookPath(eng.bin); err != nil {
				fmt.Fprintf(os.Stderr, "%s not found locally — forwarding 127.0.0.1:%d to the database; connect manually.\n", eng.bin, localPort)
				return <-ready
			}
			// Give the listener a moment, then launch the client.
			waitPort(localPort)
			client := exec.CommandContext(ctx, eng.bin, eng.args("127.0.0.1", localPort, detail)...)
			client.Stdin, client.Stdout, client.Stderr = os.Stdin, os.Stdout, os.Stderr
			client.Env = append(os.Environ(), eng.env(detail)...)
			runErr := client.Run()
			cancel()
			<-ready
			return runErr
		},
	}
	cmd.Flags().StringVarP(&component, "component", "c", "", "compose service to target")
	cmd.Flags().IntVar(&pr, "pr", 0, "target the preview of this PR number instead of production")
	return cmd
}

// componentEngine returns the database engine of a compose service, or an error
// if the service is not a database (nothing to open a console on).
func (c *Client) componentEngine(ctx context.Context, appUUID, component string) (string, error) {
	var page struct {
		Data []struct {
			Name           string `json:"name"`
			IsDatabase     bool   `json:"is_database"`
			DatabaseEngine string `json:"database_engine"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/applications/"+appUUID+"/components", nil, nil, &page); err != nil {
		return "", err
	}
	for _, comp := range page.Data {
		if comp.Name == component {
			if !comp.IsDatabase || comp.DatabaseEngine == "" {
				return "", fmt.Errorf("service %q is not a database — use `akerdock shell` or `akerdock port-forward`", component)
			}
			return comp.DatabaseEngine, nil
		}
	}
	return "", fmt.Errorf("no service %q in this stack", component)
}

// componentCreds best-effort reads the generated magic variables of a compose
// database service (SERVICE_USER_<ID> / SERVICE_PASSWORD_<ID>, §5.4) to fill the
// console. Values are null without read:sensitive — then the map stays empty and
// the caller forwards the port for a manual connection.
func (c *Client) componentCreds(ctx context.Context, appUUID, component string, preview bool) map[string]any {
	id := normalizeComponentID(component)
	q := url.Values{"limit": {"200"}}
	if preview {
		q.Set("preview", "true")
	}
	var page struct {
		Data []struct {
			Key   string  `json:"key"`
			Value *string `json:"value"`
		} `json:"data"`
	}
	detail := map[string]any{}
	if err := c.do(ctx, http.MethodGet, "/applications/"+appUUID+"/envs", q, nil, &page); err != nil {
		return detail
	}
	vals := map[string]string{}
	for _, e := range page.Data {
		if e.Value != nil {
			vals[e.Key] = *e.Value
		}
	}
	if user := vals["SERVICE_USER_"+id]; user != "" {
		detail["username"] = user
		detail["database"] = user // compose db images default the DB name to the user
	}
	if pass := firstNonEmpty(vals["SERVICE_PASSWORD_"+id], vals["SERVICE_PASSWORD_WITH_SYMBOLS_"+id]); pass != "" {
		detail["password"] = pass
	}
	return detail
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// normalizeComponentID mirrors compose.NormalizeComponentID (§4.1): uppercase,
// non-alphanumerics to underscores — the <ID> half of a magic variable name.
func normalizeComponentID(service string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r >= 'a' && r <= 'z':
			return r - ('a' - 'A')
		default:
			return '_'
		}
	}, service)
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// waitPort blocks briefly until something accepts on the local port.
func waitPort(port int) {
	for range 50 {
		conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			_ = conn.Close()
			return
		}
	}
}
