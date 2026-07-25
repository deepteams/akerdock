package cli

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"

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
}

func str(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

// engines maps a detected engine to its local client recipe. Credentials are
// best-effort from the database detail; the client prompts otherwise.
var engines = map[string]engineClient{
	"postgres": {port: 5432, bin: "psql",
		args: func(h string, p int, d map[string]any) []string {
			user := str(d, "username")
			db := str(d, "database")
			return []string{"-h", h, "-p", strconv.Itoa(p), "-U", user, db}
		},
		env: func(d map[string]any) []string { return []string{"PGPASSWORD=" + str(d, "password")} }},
	"mysql": {port: 3306, bin: "mysql",
		args: func(h string, p int, d map[string]any) []string {
			return []string{"-h", h, "-P", strconv.Itoa(p), "-u", str(d, "username"), str(d, "database")}
		},
		env: func(d map[string]any) []string { return []string{"MYSQL_PWD=" + str(d, "password")} }},
	"redis": {port: 6379, bin: "redis-cli",
		args: func(h string, p int, d map[string]any) []string { return []string{"-h", h, "-p", strconv.Itoa(p)} },
		env:  func(d map[string]any) []string { return []string{"REDISCLI_AUTH=" + str(d, "password")} }},
	"mongo": {port: 27017, bin: "mongosh",
		args: func(h string, p int, d map[string]any) []string {
			return []string{fmt.Sprintf("mongodb://%s:%d", h, p)}
		},
		env: func(d map[string]any) []string { return nil }},
}

func dbCmd() *cobra.Command {
	var component string
	cmd := &cobra.Command{
		Use:     "db REF",
		Short:   "Open a database console (port-forward + local client)",
		Example: "  akerdock db db/pg",
		Args:    usageArgs(1, "db <db/name>", "db db/pg"),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := newClient(flags.context)
			if err != nil {
				return err
			}
			r, err := parseRef(args[0])
			if err != nil {
				return err
			}
			if r.kind != "databases" {
				return fmt.Errorf("db expects a db/… reference")
			}
			res, err := c.resolve(cmd.Context(), r)
			if err != nil {
				return err
			}
			eng, ok := engines[res.Engine]
			if !ok {
				return fmt.Errorf("unsupported engine %q — use `akerdock port-forward` instead", res.Engine)
			}

			// Best-effort connection detail (may require read:sensitive).
			detail := map[string]any{}
			_ = c.do(cmd.Context(), http.MethodGet, "/databases/"+res.Uuid, nil, nil, &detail)

			// Pick a free local port and forward it to the engine port.
			localPort, err := freePort()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()
			ready := make(chan error, 1)
			go func() {
				ready <- c.runPortForward(ctx, "/databases/"+res.Uuid+"/port-forwards", component, localPort, eng.port)
			}()

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
	return cmd
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
	for i := 0; i < 50; i++ {
		conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			_ = conn.Close()
			return
		}
	}
}
