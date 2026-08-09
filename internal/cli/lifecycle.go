package cli

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"github.com/spf13/cobra"
)

// lifecycleCmds mounts `restart`, `start` and `stop` on one group. The same
// three endpoints exist for the three kinds — POST /{applications,databases,
// services}/{uuid}/{restart,start,stop} — so the three verbs are spelled the
// same way under each group (ADR-070 §2). The asymmetry that shapes the rest of
// the tree does not apply here, and inventing one in the client would be ours
// alone (ADR-070 §Alternatives rejected).
func lifecycleCmds(k resourceKind) []*cobra.Command {
	// Only the kinds that HAVE a deploy verb are pointed at it: a database has
	// no deployment, and a help text naming a command its group does not offer
	// is the runtime refusal ADR-070 §1 removes, moved into prose.
	rebuild := ""
	if k != kindDB {
		rebuild = " To pick up a new commit or a new image, the verb that builds is `akerdock " + k.group + " deploy run`."
	}
	verbs := []struct{ name, short, long string }{
		{
			"restart",
			"Restart the containers, without rebuilding",
			"Stops and starts what is already deployed — no clone, no build, the same " +
				"artifact. It is how a process is handed values it only reads at boot: the " +
				"variables an `env set` wrote, an image or credential change the platform " +
				"flagged as awaiting a restart." + rebuild,
		},
		{
			"start",
			"Start a stopped " + k.label,
			"Sets the desired status to `running` and starts the existing containers. " +
				"Nothing is built, so a " + k.label + " whose containers were never created " +
				"has nothing to start — deploy it first." + rebuild,
		},
		{
			"stop",
			"Stop the containers, keeping the volumes and the configuration",
			"Sets the desired status to `stopped`. The containers go down; the volumes, the " +
				"configuration and the " + k.label + " itself stay exactly as they are, and " +
				"`start` brings it back. That reversibility is why this verb asks for no " +
				"confirmation: destroying is a different act, and it is not in this CLI.",
		},
	}
	cmds := make([]*cobra.Command, 0, len(verbs))
	for _, v := range verbs {
		cmds = append(cmds, lifecycleCmd(k, v.name, v.short, v.long))
	}
	return cmds
}

// lifecycleCmd builds one lifecycle verb. All three take the same shape — one
// optional name and no flags — because the nine endpoints do: not one of them
// declares a request body, a query parameter or a component.
//
// `restart -c web` and `restart --pr 42` are therefore NOT offered. A flag the
// server would silently ignore is worse than its absence: the caller would walk
// away believing one container had been restarted, or that the PR instance had,
// when the whole production resource went down and up. Same reasoning as
// `deploy run --skip-build`, which exists on the app group only.
func lifecycleCmd(k resourceKind, verb, short, long string) *cobra.Command {
	example := "  akerdock " + k.group + " " + verb + " " + exampleName(k)
	if k.dirDefault {
		example += "\n  akerdock " + k.group + " " + verb + "   # the application named by the .akerdock file"
	}
	return &cobra.Command{
		Use:     verb + " [NAME]",
		Short:   short,
		Long:    long,
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
			return c.runLifecycle(cmd.Context(), k, res, verb)
		},
	}
}

// jobAccepted is JobAccepted, the 202 envelope of every long-running mutation
// (§24.1). It is the whole schema, so `-o json` on a lifecycle verb prints the
// API object entire.
type jobAccepted struct {
	JobUuid   string `json:"job_uuid"`
	StatusURL string `json:"status_url"`
}

// runLifecycle posts the verb and reports what the platform accepted.
//
// The wording is "accepted", never "restarted": the response is a 202 and a job
// uuid, so nothing has moved yet on the server. Claiming otherwise would make
// the CLI lie in the exact window an operator watches — `akerdock <group> info
// <name>` is where the observed status eventually lands.
func (c *Client) runLifecycle(ctx context.Context, k resourceKind, res resource, verb string) error {
	var acc jobAccepted
	if err := c.do(ctx, http.MethodPost, k.path+"/"+res.Uuid+"/"+verb, nil, nil, &acc); err != nil {
		return err
	}
	switch {
	case flags.output == "json":
		return printJSON(acc)
	case flags.quiet:
		// The scripted form: the job uuid alone, ready to be captured.
		_, _ = fmt.Fprintln(os.Stdout, acc.JobUuid)
	default:
		_, _ = fmt.Fprintf(os.Stdout, "%s: %s accepted (job %s)\n", res.Name, verb, acc.JobUuid)
	}
	return nil
}

// exampleName keeps `--help` concrete: a plausible name of the right kind reads
// faster than a `<NAME>` placeholder, and it costs nothing to be plausible.
func exampleName(k resourceKind) string {
	switch k {
	case kindDB:
		return "pg"
	case kindSvc:
		return "monitoring"
	default:
		return "varuna"
	}
}
