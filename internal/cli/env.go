package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

// The variable verb group (ADR-070 §2). Every competitor opens its tutorial on
// this command and we shipped none of it: setting a variable required a browser.
//
// The masking policy is the server's and stays there — no `read:sensitive` means
// `value` comes back null with `is_redacted`, and an `is_locked` variable never
// reveals its value to anyone (INV-003, §5.4). This client renders what it was
// handed and decides nothing; re-deriving the rule here would be a second
// implementation of a security policy, drifting from the first.
func envCmd(k resourceKind) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "env",
		Short: "Environment variables of " + envArticleLabel(k),
		Long: "Reads and writes the environment variables of " + envArticleLabel(k) + ".\n\n" +
			"Values you are not allowed to read come back masked and are shown as such: the " +
			"server decides what a token may see, and `get` fails rather than print an empty " +
			"string a script would take for a value.\n\n" +
			"Writing a variable does not change the running process — a container keeps the " +
			"environment it was created with. `--apply` is what makes the change effective.",
	}
	cmd.AddCommand(envListCmd(k), envGetCmd(k), envSetCmd(k), envUnsetCmd(k))
	return cmd
}

// envArticleLabel spells the group's label with its article, so every sentence
// below reads for the three kinds without a per-kind string.
func envArticleLabel(k resourceKind) string {
	if strings.ContainsRune("aeiou", rune(k.label[0])) {
		return "an " + k.label
	}
	return "a " + k.label
}

// envHasPreviews says whether a kind's variables can be scoped to a pull request.
// Only an application has previews (§20.4): a compose stack is deployed once,
// so `--pr` is not a flag it silently ignores — it is a flag it does not have.
func envHasPreviews(k resourceKind) bool { return k.group == kindApp.group }

// envVar is the subset of EnvironmentVariable this client acts on. Note what is
// absent: nothing here recomputes masking. `Value` is a pointer because the API
// sends null for a value the caller may not read, and null is the one state a
// bare string could not represent.
type envVar struct {
	Uuid              string  `json:"uuid"`
	Key               string  `json:"key"`
	Value             *string `json:"value"`
	IsRedacted        bool    `json:"is_redacted"`
	IsBuildTime       bool    `json:"is_build_time"`
	IsSecret          bool    `json:"is_secret"`
	IsLiteral         bool    `json:"is_literal"`
	IsMultiline       bool    `json:"is_multiline"`
	IsLocked          bool    `json:"is_locked"`
	IsPreviewOverride bool    `json:"is_preview_override"`
}

// envEntry keeps the server's own bytes alongside the decoded fields: `-o json`
// prints the API object untouched, fields this CLI never reads included, so a
// script consuming it is coupled to the contract rather than to our struct.
type envEntry struct {
	envVar
	raw json.RawMessage
}

// envScope is the collection one verb works on: the resource, plus the preview
// whose overrides shadow it when `--pr` was given.
type envScope struct {
	kind    resourceKind
	res     resource
	preview previewInfo
	pr      int
}

func (s envScope) scopedToPreview() bool { return s.pr > 0 }

// collection is the path the variables are listed on and created in. For a PR
// it is the preview's own collection, which serves the EFFECTIVE set — the
// shared preview variables merged with this PR's overrides (INV-010), never a
// production value.
func (s envScope) collection() string {
	if s.scopedToPreview() {
		return "/applications/" + s.res.Uuid + "/previews/" + s.preview.Uuid + "/envs"
	}
	return s.kind.path + "/" + s.res.Uuid + "/envs"
}

// item is the path of one variable. It hangs off the RESOURCE even for a
// preview override: the API exposes no per-preview item path, and an override is
// a variable of this application which the server resolves by (uuid, resource).
// Which is also why `set --pr` only ever patches a variable the preview listing
// reported as an override — patching an inherited one would rewrite the shared
// set behind every other PR.
func (s envScope) item(envUUID string) string {
	return s.kind.path + "/" + s.res.Uuid + "/envs/" + envUUID
}

// where names the target in a message, PR included, because "no variable named
// FOO" is a different fact on the application and on its PR #12.
func (s envScope) where() string {
	if s.scopedToPreview() {
		return fmt.Sprintf("%s %s (PR #%d)", s.kind.label, s.res.Name, s.pr)
	}
	return s.kind.label + " " + s.res.Name
}

// envTarget resolves the resource and, when asked, the preview — one helper so
// the four verbs share a single spelling of "which variables am I touching".
func (c *Client) envTarget(ctx context.Context, k resourceKind, pr int, nameArgs []string) (envScope, error) {
	res, err := c.target(ctx, k, nameArgs)
	if err != nil {
		return envScope{}, err
	}
	s := envScope{kind: k, res: res, pr: pr}
	if pr > 0 {
		p, err := c.resolvePreview(ctx, res.Uuid, pr)
		if err != nil {
			return envScope{}, err
		}
		s.preview = p
	}
	return s, nil
}

// listEnvs walks the collection. The resource collections paginate by cursor;
// the preview one answers with the whole effective set and declares no cursor,
// so it is asked plainly rather than with parameters it does not define.
func (c *Client) listEnvs(ctx context.Context, s envScope) ([]envEntry, error) {
	var out []envEntry
	cursor := ""
	for {
		q := url.Values{}
		if !s.scopedToPreview() {
			q.Set("limit", "100")
			if cursor != "" {
				q.Set("cursor", cursor)
			}
		}
		var page struct {
			Data       []json.RawMessage `json:"data"`
			NextCursor *string           `json:"next_cursor"`
		}
		if err := c.do(ctx, http.MethodGet, s.collection(), q, nil, &page); err != nil {
			return nil, err
		}
		for _, raw := range page.Data {
			var v envVar
			if err := json.Unmarshal(raw, &v); err != nil {
				return nil, fmt.Errorf("unreadable variable in the API response: %w", err)
			}
			out = append(out, envEntry{envVar: v, raw: raw})
		}
		if s.scopedToPreview() || page.NextCursor == nil || *page.NextCursor == "" {
			return out, nil
		}
		cursor = *page.NextCursor
	}
}

func findEnv(entries []envEntry, key string) (envEntry, bool) {
	for _, e := range entries {
		if e.Key == key {
			return e, true
		}
	}
	return envEntry{}, false
}

func envListCmd(k resourceKind) *cobra.Command {
	var pr int
	cmd := &cobra.Command{
		Use:     "list [NAME]",
		Aliases: listAliases(),
		Short:   "List the environment variables",
		Long: "Every variable of the set, with the value as the server returned it — a masked " +
			"value stays masked here, and `-o json` emits the API objects unaltered.",
		Example: "  akerdock " + k.group + " env list\n" +
			"  akerdock " + k.group + " env list my-" + k.group + "\n" +
			"  akerdock " + k.group + " env list -o json",
		Args: targetArgs(k),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(flags.context)
			if err != nil {
				return err
			}
			s, err := c.envTarget(cmd.Context(), k, pr, args)
			if err != nil {
				return err
			}
			entries, err := c.listEnvs(cmd.Context(), s)
			if err != nil {
				return err
			}
			if flags.output == "json" {
				raws := make([]json.RawMessage, 0, len(entries))
				for _, e := range entries {
					raws = append(raws, e.raw)
				}
				return printJSON(raws)
			}
			rows := make([][]string, 0, len(entries))
			for _, e := range entries {
				rows = append(rows, []string{e.Key, envValue(e.envVar), envFlags(e.envVar)})
			}
			table([]string{"KEY", "VALUE", "FLAGS"}, rows)
			return nil
		},
	}
	envAddPreviewFlag(cmd, k, &pr)
	return cmd
}

// envValue renders the value column. A masked value is named as masked rather
// than left blank: an empty cell reads as "the variable is empty", which is a
// different and wrong fact.
func envValue(v envVar) string {
	if v.Value == nil {
		return "<redacted>"
	}
	// A multi-line value — a private key, a certificate — would tear the table
	// apart. The listing answers "which keys, roughly what"; `get` is how a
	// value is read in full.
	return strings.ReplaceAll(strings.ReplaceAll(*v.Value, "\r\n", "\n"), "\n", `\n`)
}

// envFlags folds the booleans that change what a variable IS into one column:
// locked (never re-editable, never revealed), secret (mounted by BuildKit,
// never a build ARG), build (injected at build time) and, under --pr, whether
// the line is this PR's own override or inherited from the shared preview set.
func envFlags(v envVar) string {
	var f []string
	if v.IsLocked {
		f = append(f, "locked")
	}
	if v.IsSecret {
		f = append(f, "secret")
	}
	if v.IsBuildTime {
		f = append(f, "build")
	}
	if v.IsPreviewOverride {
		f = append(f, "override")
	}
	if len(f) == 0 {
		return "-"
	}
	return strings.Join(f, ",")
}

func envGetCmd(k resourceKind) *cobra.Command {
	var pr int
	cmd := &cobra.Command{
		Use:   "get KEY [NAME]",
		Short: "Print the value of one variable",
		Long: "Prints the value alone, so it can be captured: `PASS=$(akerdock " + k.group + " env get PASS)`.\n\n" +
			"When the server masks the value the command FAILS instead of printing nothing — a " +
			"blank line captured into a variable is how a masked secret becomes an empty " +
			"password in production.",
		Example: "  akerdock " + k.group + " env get DATABASE_URL\n" +
			"  akerdock " + k.group + " env get DATABASE_URL -o json",
		Args: func(_ *cobra.Command, args []string) error {
			// KEY first, the optional NAME last (ADR-070 §1). Exactly one key
			// here, so the two positionals never compete for a meaning.
			if len(args) == 1 || len(args) == 2 {
				return nil
			}
			return usageErrorf("usage: akerdock %s env get KEY [NAME]\n  example: akerdock %s env get DATABASE_URL", k.group, k.group)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			key := args[0]
			c, err := newClient(flags.context)
			if err != nil {
				return err
			}
			s, err := c.envTarget(cmd.Context(), k, pr, args[1:])
			if err != nil {
				return err
			}
			entries, err := c.listEnvs(cmd.Context(), s)
			if err != nil {
				return err
			}
			e, ok := findEnv(entries, key)
			if !ok {
				return fmt.Errorf("no variable named %q on %s — `akerdock %s env list` shows the set", key, s.where(), k.group)
			}
			if flags.output == "json" {
				return printJSON(e.raw)
			}
			if e.Value == nil {
				if e.IsLocked {
					return fmt.Errorf("%s is locked — its value is write-only and is never returned, to anyone (§5.4)", key)
				}
				return fmt.Errorf("%s is masked — reading a value needs the `read:sensitive` permission (INV-003)", key)
			}
			_, _ = fmt.Fprintln(os.Stdout, *e.Value)
			return nil
		},
	}
	envAddPreviewFlag(cmd, k, &pr)
	return cmd
}

func envSetCmd(k resourceKind) *cobra.Command {
	var pr int
	var apply, secret bool
	cmd := &cobra.Command{
		Use:   "set KEY=VALUE [KEY=VALUE...] [NAME]",
		Short: "Create or update variables",
		Long: "Sets one or more variables in a single call. A key that exists is updated, a key " +
			"that does not is created; nothing is echoed back, because half of what this " +
			"command writes is a secret and terminals keep scrollback.\n\n" +
			"The change reaches the running process only with `--apply`.",
		Example: "  akerdock " + k.group + " env set LOG_LEVEL=debug\n" +
			"  akerdock " + k.group + " env set LOG_LEVEL=debug SENTRY_DSN=https://… my-" + k.group + " --apply\n" +
			"  akerdock " + k.group + " env set NPM_TOKEN=… --secret",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				return usageErrorf("usage: akerdock %s env set KEY=VALUE [KEY=VALUE...] [NAME]\n  example: akerdock %s env set LOG_LEVEL=debug", k.group, k.group)
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			// Parsed before the client exists: a typo in a pair must cost no
			// request at all, and above all must not write half of them.
			pairs, nameArgs, err := parseEnvPairs(k, args)
			if err != nil {
				return err
			}
			c, err := newClient(flags.context)
			if err != nil {
				return err
			}
			s, err := c.envTarget(cmd.Context(), k, pr, nameArgs)
			if err != nil {
				return err
			}
			existing, err := c.listEnvs(cmd.Context(), s)
			if err != nil {
				return err
			}
			written := make([]json.RawMessage, 0, len(pairs))
			for _, p := range pairs {
				raw, err := c.writeEnv(cmd.Context(), s, existing, p, secret, cmd.Flags().Changed("secret"))
				if err != nil {
					return err
				}
				written = append(written, raw)
				if !flags.quiet && flags.output != "json" {
					// The key, never the value (ADR-070 §Verification).
					_, _ = fmt.Fprintf(os.Stdout, "%s set\n", p.key)
				}
			}
			if flags.output == "json" {
				if err := printJSON(written); err != nil {
					return err
				}
			}
			if apply {
				return c.applyEnvChange(cmd.Context(), s)
			}
			if !flags.quiet {
				_, _ = fmt.Fprintln(os.Stderr, "not applied: the running containers keep the values they started with — rerun with --apply")
			}
			return nil
		},
	}
	envAddPreviewFlag(cmd, k, &pr)
	cmd.Flags().BoolVar(&apply, "apply", false, envApplyHelp(k))
	cmd.Flags().BoolVar(&secret, "secret", false,
		"mark the variable as a build secret: mounted by BuildKit for one RUN instead of passed as a build ARG, which `docker history` would expose (§5.2)")
	return cmd
}

// envPair is one KEY=VALUE the caller typed.
type envPair struct{ key, value string }

// parseEnvPairs splits the positionals into pairs and the optional trailing
// NAME. The `=` is what tells them apart, so a name is never mistaken for a
// variable and the tree's rule — the name is the last positional — holds.
func parseEnvPairs(k resourceKind, args []string) ([]envPair, []string, error) {
	var pairs []envPair
	var nameArgs []string
	for i, arg := range args {
		key, value, ok := strings.Cut(arg, "=")
		if !ok {
			if i == len(args)-1 {
				nameArgs = []string{arg}
				break
			}
			return nil, nil, usageErrorf("expected KEY=VALUE, got %q — only the last argument may be the %s's name", arg, k.label)
		}
		if key == "" {
			return nil, nil, usageErrorf("missing key in %q — the form is KEY=VALUE", arg)
		}
		if !envKeyShape.MatchString(key) {
			return nil, nil, usageErrorf("invalid variable name %q — a key matches [A-Za-z_][A-Za-z0-9_]*", key)
		}
		if value == "" {
			// The shell strips the quotes of `KEY=""` before we ever see it, so
			// an empty value and a forgotten one are the same six bytes. We
			// refuse rather than guess which one wipes a production secret.
			return nil, nil, usageErrorf("no value in %q — write KEY=VALUE, or `akerdock %s env unset %s` to remove the variable", arg, k.group, key)
		}
		pairs = append(pairs, envPair{key: key, value: value})
	}
	if len(pairs) == 0 {
		return nil, nil, usageErrorf("no KEY=VALUE given — usage: akerdock %s env set KEY=VALUE [KEY=VALUE...] [NAME]", k.group)
	}
	return pairs, nameArgs, nil
}

// envKeyShape is the server's own key format (handlers/envs.go). Reproduced —
// not to validate on the platform's behalf, but because it is what tells a KEY
// from a resource NAME on `unset`, where both are bare words.
var envKeyShape = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// writeEnv creates or updates one variable and returns the API object.
//
// Under --pr the choice is not "create or update" but "override or not": a key
// the preview inherits from the shared set is COPIED into an override of this
// PR, never patched where it lies (INV-010 — one PR's value must not travel to
// the others).
func (c *Client) writeEnv(ctx context.Context, s envScope, existing []envEntry, p envPair, secret, secretSet bool) (json.RawMessage, error) {
	body := map[string]any{"key": p.key, "value": p.value}
	if secretSet {
		body["is_secret"] = secret
	}
	e, found := findEnv(existing, p.key)
	if !found || (s.scopedToPreview() && !e.IsPreviewOverride) {
		var raw json.RawMessage
		if err := c.do(ctx, http.MethodPost, s.collection(), nil, body, &raw); err != nil {
			return nil, err
		}
		return raw, nil
	}
	// A PATCH is partial and `key` is not modifiable: send the value, and the
	// secret flag only when the caller actually asked for it, so a plain `set`
	// on an existing build secret does not quietly demote it to an ARG.
	patch := map[string]any{"value": p.value}
	if secretSet {
		patch["is_secret"] = secret
	}
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodPatch, s.item(e.Uuid), nil, patch, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func envUnsetCmd(k resourceKind) *cobra.Command {
	var pr int
	var apply bool
	cmd := &cobra.Command{
		Use:   "unset KEY [KEY...] [NAME]",
		Short: "Delete variables",
		Long: "Removes one or more variables. Every key is resolved first: if one of them does " +
			"not exist, nothing is deleted at all — a typo in a list of five must not leave " +
			"four of them gone.\n\n" +
			"KEY and NAME are both bare words here, so a trailing argument that names one of " +
			"your " + k.plural + " is read as the name, and any other is read as a key.",
		Example: "  akerdock " + k.group + " env unset LOG_LEVEL\n" +
			"  akerdock " + k.group + " env unset LOG_LEVEL SENTRY_DSN my-" + k.group + " --apply",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				return usageErrorf("usage: akerdock %s env unset KEY [KEY...] [NAME]\n  example: akerdock %s env unset LOG_LEVEL", k.group, k.group)
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(flags.context)
			if err != nil {
				return err
			}
			s, keys, err := c.envUnsetTarget(cmd.Context(), k, pr, args)
			if err != nil {
				return err
			}
			existing, err := c.listEnvs(cmd.Context(), s)
			if err != nil {
				return err
			}
			targets, err := resolveUnsetKeys(s, existing, keys)
			if err != nil {
				return err
			}
			for _, e := range targets {
				if err := c.do(cmd.Context(), http.MethodDelete, s.item(e.Uuid), nil, nil, nil); err != nil {
					return err
				}
				if !flags.quiet {
					_, _ = fmt.Fprintf(os.Stdout, "%s unset\n", e.Key)
				}
			}
			if apply {
				return c.applyEnvChange(cmd.Context(), s)
			}
			if !flags.quiet {
				_, _ = fmt.Fprintln(os.Stderr, "not applied: the running containers keep the values they started with — rerun with --apply")
			}
			return nil
		},
	}
	envAddPreviewFlag(cmd, k, &pr)
	cmd.Flags().BoolVar(&apply, "apply", false, envApplyHelp(k))
	return cmd
}

// envUnsetTarget splits `KEY [KEY...] [NAME]` and resolves the target in one
// pass. Unlike `set`, whose `=` separates the two, both positionals here are
// bare words and no shape rule tells them apart honestly — a variable key and
// a resource name can be spelled identically.
//
// So the split is settled against the team's actual resources: a trailing
// argument that names one of them IS the name (the tree's rule — the name is
// the last positional), and one that names none is a key, whatever it looks
// like. The listing it costs is the one `target` would have made anyway.
func (c *Client) envUnsetTarget(ctx context.Context, k resourceKind, pr int, args []string) (envScope, []string, error) {
	last := args[len(args)-1]
	// The removed REF is caught here rather than three lines later as "no
	// variable named app/varuna", which would be true and useless (ADR-070 §5).
	if _, err := checkNotARef(k, last); err != nil {
		return envScope{}, nil, err
	}
	keys, nameArgs := args, []string(nil)
	if len(args) > 1 {
		items, err := c.listAll(ctx, k.path)
		if err != nil {
			return envScope{}, nil, err
		}
		for _, it := range items {
			if it.Name == last || it.Uuid == last {
				keys, nameArgs = args[:len(args)-1], []string{last}
				break
			}
		}
	}
	s, err := c.envTarget(ctx, k, pr, nameArgs)
	return s, keys, err
}

// resolveUnsetKeys maps every key to the variable it deletes, and refuses the
// whole call if any of them cannot be. Deleting is the one verb here with no
// undo, so it either does what was asked or does nothing.
func resolveUnsetKeys(s envScope, existing []envEntry, keys []string) ([]envEntry, error) {
	var targets []envEntry
	var missing, inherited []string
	for _, key := range keys {
		e, ok := findEnv(existing, key)
		switch {
		case !ok:
			missing = append(missing, key)
		case s.scopedToPreview() && !e.IsPreviewOverride:
			// The preview listing is the effective set: this key comes from the
			// shared preview variables, which every open PR runs with. Deleting
			// it here would silently change all of them.
			inherited = append(inherited, key)
		default:
			targets = append(targets, e)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("no variable named %s on %s — nothing was deleted", strings.Join(envQuoted(missing), ", "), s.where())
	}
	if len(inherited) > 0 {
		verb := "is"
		if len(inherited) > 1 {
			verb = "are"
		}
		return nil, fmt.Errorf("%s %s not overridden by PR #%d but inherited from the shared preview set — deleting there would change every preview, so do it without --pr; nothing was deleted",
			strings.Join(envQuoted(inherited), ", "), verb, s.pr)
	}
	return targets, nil
}

func envQuoted(ss []string) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, fmt.Sprintf("%q", s))
	}
	return out
}

// addPreviewFlag registers --pr on the kinds that have previews, and only
// those: `svc env list --pr 3` fails at parse time with "unknown flag" rather
// than at runtime with an explanation (ADR-070 §1).
func envAddPreviewFlag(cmd *cobra.Command, k resourceKind, pr *int) {
	if !envHasPreviews(k) {
		return
	}
	cmd.Flags().IntVar(pr, "pr", 0, "act on the variables of this pull request's preview instead of the production set (§20.4)")
}

// applyHelp says what --apply costs, which differs by kind: an application
// redeploys its current artifact (ADR-048's skip_build), a compose stack has no
// such option in its deploy endpoint and is deployed the ordinary way.
func envApplyHelp(k resourceKind) string {
	if envHasPreviews(k) {
		return "redeploy the current artifact so the new values reach the process — no clone, no build (ADR-048)"
	}
	return "redeploy the stack so the new values reach its containers (a stack's deploy has no skip_build: this rebuilds)"
}

// applyEnvChange is the whole point of --apply: a variable written and never
// applied does nothing at all, because a container carries the environment it
// was created with until it is replaced. `restart` is the trap — it hands the
// same frozen values back to the process.
func (c *Client) applyEnvChange(ctx context.Context, s envScope) error {
	path := s.kind.path + "/" + s.res.Uuid + "/deploy"
	var body any
	switch {
	case s.scopedToPreview():
		// A preview redeploys THIS instance at the SHA it is pinned to; a plain
		// deploy would re-read the pull request.
		path = "/applications/" + s.res.Uuid + "/previews/" + s.preview.Uuid + "/redeploy"
		body = map[string]any{"skip_build": true}
	case s.kind.group == kindApp.group:
		body = map[string]any{"skip_build": true}
	default:
		// POST /services/{uuid}/deploy accepts no body at all — sending one
		// would be inventing a field the contract does not have.
		body = nil
	}
	var accepted struct {
		DeploymentUuid string `json:"deployment_uuid"`
	}
	if err := c.do(ctx, http.MethodPost, path, nil, body, &accepted); err != nil {
		return err
	}
	if !flags.quiet {
		_, _ = fmt.Fprintf(os.Stdout, "deployment %s queued\n", accepted.DeploymentUuid)
	}
	return nil
}
