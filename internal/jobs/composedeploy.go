// Compose build pack (compose-spec.md, deployment-engine §5.7): the
// repository's compose file becomes a multi-service stack. This iteration
// replaces every changed service in place (recreate, topological order);
// the per-service zero-downtime switch of §8.2 is the next one.
package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/deepteams/akerdock/internal/adoption"
	"github.com/deepteams/akerdock/internal/compose"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/proxy"
	"github.com/deepteams/akerdock/internal/sshexec"
	"github.com/deepteams/akerdock/internal/store"
)

// executeCompose drives a deployment whose build pack is compose. It owns
// everything after `preparing`: cloning, validation, per-service create.
func (r *deploymentRun) executeCompose(ctx context.Context, appUUID, appDir, labels string) error {
	if r.d.IsRollback {
		return fmt.Errorf("rollback of a compose stack is not supported yet — redeploy a previous commit instead")
	}
	if r.service == nil && r.app.Application.GitRepositoryUrl == nil {
		return fmt.Errorf("the compose build pack requires a git source")
	}
	// No blind replay (§2.5): a compose deployment recovered past `preparing`
	// resumes by PER-SERVICE inspection (compose-spec §8.2 — "reprise
	// possible"). Everything up to the per-service replacement is idempotent
	// (validation, component sync, image pulls); each service is then either
	// hash-skipped (already done), completed from a surviving healthy
	// candidate (the promotion is finished, never replayed), or redone from
	// scratch after the stale candidate is discarded.
	resumed := r.d.Status != store.DeploymentStatusQueued && r.d.Status != store.DeploymentStatusPreparing
	if resumed {
		r.h.Logger.Warn("resuming a crashed compose deployment by per-service inspection",
			"deployment_uuid", pguuid.String(r.d.Uuid), "state", string(r.d.Status))
	}

	if err := r.step(ctx, "prepare", func() (*sshexec.Result, error) {
		return r.client.Run(ctx, fmt.Sprintf(
			"docker info --format ok >/dev/null && mkdir -p %s/env && chmod 700 %s %s/env && (docker network inspect %s >/dev/null 2>&1 || docker network create --label akerdock.managed=true %s)",
			appDir, appDir, appDir, r.dest.Network, r.dest.Network))
	}); err != nil {
		return err
	}

	// --- source: clone for the build pack, the services row inline ---------
	var content, workDir, sha string
	if r.service != nil {
		// Inline stack: the file IS the database row (§9.1); relative binds
		// resolve under the stack's own directory (§2.4). Builds were refused
		// at save time — there is no source to build from.
		content = r.service.ComposeContent
		workDir = appDir + "/mounts"
		sha = strings.ReplaceAll(pguuid.String(r.d.Uuid), "-", "")
		r.skipStep(ctx, "clone", "inline compose stack (no git source)")
		if err := r.setStatus(ctx, store.DeploymentStatusBuilding); err != nil {
			return err
		}
	} else {
		srcDir, gitSha, err := r.cloneForCompose(ctx, appUUID, appDir)
		if err != nil {
			return err
		}
		sha = gitSha
		if err := r.setStatus(ctx, store.DeploymentStatusBuilding); err != nil {
			return err
		}
		baseDir := strings.TrimPrefix(r.app.Application.BaseDirectory, "/")
		composePath := "docker-compose.yml"
		if p := r.app.BuildConfig.ComposeFilePath; p != nil && *p != "" {
			composePath = strings.TrimPrefix(*p, "/")
		}
		workDir = strings.TrimRight(srcDir+"/"+baseDir, "/")
		if err := r.step(ctx, "read_compose", func() (*sshexec.Result, error) {
			res, err := r.client.Run(ctx, fmt.Sprintf("cat %s/%s", workDir, composePath))
			if err == nil && res.ExitCode != 0 {
				return res, fmt.Errorf("compose file %q not found in the repository", composePath)
			}
			if err == nil {
				content = res.Stdout
			}
			return res, err
		}); err != nil {
			return err
		}
	}

	// --- validation + plan (control plane, compose-spec §1–5) -------------
	vars, err := r.plainEnvVars(ctx)
	if err != nil {
		return err
	}
	// First pass discovers the service names; magic variables may still be
	// undefined here, which only produces warnings.
	// An adopted stack pins its volumes as external objects (§20.7): the
	// adoption itself is the policy decision, taken explicitly by the
	// operator — everything else keeps the strict default.
	policy := compose.Policy{AllowExternalObjects: r.app.Resource.AdoptedAt.Valid}
	first, err := compose.Load(ctx, compose.Input{
		Content: content, StackUUID: appUUID, Variables: vars,
		Raw: r.app.BuildConfig.RawCompose, Policy: policy,
	})
	if err != nil {
		return err
	}
	if first.Plan != nil {
		// Components must exist before the magic FQDN/URL pass: referencing
		// SERVICE_FQDN_<ID> is a declaration of intent that CREATES the
		// component's domain from the server wildcard (§6). A preview stack
		// syncs nothing: service_components describe PRODUCTION — a preview
		// overwriting them would corrupt the real stack's state (INV-010).
		if r.preview == nil {
			if _, err := r.syncComponents(ctx, first.Plan); err != nil {
				return err
			}
		}
		if err := r.ensureMagicVariables(ctx, content, first.Plan, first.Project.ServiceNames(), vars); err != nil {
			return err
		}
	}
	// The magic variables were possibly just persisted: runtime.sh must carry
	// them, so it is rendered NOW, not in the shared preparing step.
	envFile, stackKeys, err := r.renderRuntimeEnv(ctx)
	if err != nil {
		return err
	}
	if res, err := r.client.RunInput(ctx, fmt.Sprintf("umask 077 && cat > %s/env/runtime.sh", appDir), envFile); err != nil || res.ExitCode != 0 {
		return fmt.Errorf("uploading runtime.sh failed")
	}

	result, err := compose.Load(ctx, compose.Input{
		Content: content, StackUUID: appUUID, Variables: vars,
		Raw: r.app.BuildConfig.RawCompose, Policy: policy,
	})
	if err != nil {
		return err
	}
	if err := r.reportFindings(ctx, result.Findings); err != nil || result.Plan == nil {
		if err == nil {
			err = fmt.Errorf("the compose file was refused by validation")
		}
		return err
	}
	plan := result.Plan
	// The scale-to-zero wake set (ADR-037 §5) is known as soon as the plan is:
	// set it before the per-service loop, whose stable routing steps provision
	// the waker — otherwise the waker would only know the routed services and
	// wake a stack without its dependencies.
	r.stzWakeOrder = stackWakeOrder(plan)
	if r.service != nil {
		for _, sp := range plan.Services {
			if sp.Build {
				return fmt.Errorf("service %s: build requires a git source — inline stacks deploy images only", sp.Name)
			}
		}
	}

	// --- component sync (data dictionary §9.2) ----------------------------
	componentIDs := map[string]int64{}
	if r.preview == nil {
		if componentIDs, err = r.syncComponents(ctx, plan); err != nil {
			return err
		}
		// The stack's volumes become visible in the Storages tab (§2.4):
		// mirrored rows, rewritten each deployment — the FILE is the truth.
		// Best-effort BY CONTRACT: this mirror exists for display, and a
		// display sync must never fail a deployment.
		if err := r.syncStackStorages(ctx, plan); err != nil {
			r.h.Logger.Warn("storages mirror sync failed — the Storages tab may be stale",
				"app", pguuid.String(r.app.Resource.Uuid), "error", err)
		}
	}

	// --- stack objects: network, extra networks, volumes (§2.1, §2.4) -----
	if err := r.step(ctx, "prepare_stack", func() (*sshexec.Result, error) {
		var b strings.Builder
		ensureNetwork := func(name string) {
			fmt.Fprintf(&b, "docker network inspect %s >/dev/null 2>&1 || docker network create %s %s\n", name, labels, name)
		}
		ensureNetwork(plan.NetworkName)
		extra := make([]string, 0, len(plan.ExtraNetworks))
		for dockerName := range plan.ExtraNetworks {
			extra = append(extra, dockerName)
		}
		sort.Strings(extra)
		for _, dockerName := range extra {
			ensureNetwork(dockerName)
		}
		volumes := make([]string, 0, len(plan.Volumes))
		for _, dockerName := range plan.Volumes {
			volumes = append(volumes, dockerName)
		}
		sort.Strings(volumes)
		for _, dockerName := range volumes {
			fmt.Fprintf(&b, "docker volume create %s %s >/dev/null\n", labels, dockerName)
		}
		return r.client.Run(ctx, b.String())
	}); err != nil {
		return err
	}

	// A private registry credential on the application applies to the whole
	// stack's pulls, exactly like the single-container packs.
	logout, err := r.registryLogin(ctx)
	if err != nil {
		return err
	}
	defer logout()

	// Which components carry a domain — they must be reachable by the proxy,
	// so they also join the destination network (§2.1). A preview routes from
	// its own derived FQDNs (§20.4.1), never from the production domains.
	var routed map[string]bool
	if r.preview != nil {
		routed = map[string]bool{}
		for name := range r.composePreviewRoutes(ctx, content, plan) {
			routed[name] = true
		}
	} else if routed, err = r.routedComponents(ctx, componentIDs); err != nil {
		return err
	}

	// Pre-deployment hooks (§10, x-akerdock): in the EXISTING container of
	// each service that declares one, before any build or mutation — a
	// failure here fails the deployment while the running stack is untouched.
	// A preview stack skips hooks entirely: the §10 guarantees are written
	// against the production switch, and a fresh ephemeral stack has neither
	// an existing container nor a candidate to promise anything about.
	for _, sp := range plan.Services {
		if sp.PreCommand == "" {
			continue
		}
		if r.preview != nil {
			r.skipStep(ctx, "pre_deployment_"+sp.Name, "hooks are skipped in previews")
			continue
		}
		cmd := sp.PreCommand
		if err := r.runHook(ctx, "pre_deployment_"+sp.Name, &cmd, sp.ContainerName); err != nil {
			return err
		}
	}

	// --- images first (§8.2 step 2): build/pull EVERYTHING before any
	// mutation — a stack must never be half-replaced because an image of a
	// later service turned out to be unpullable.
	images := map[string]composeImage{}
	for _, sp := range plan.Services {
		img, err := r.ensureComposeImage(ctx, sp, workDir, labels, sha)
		if err != nil {
			return fmt.Errorf("service %s: %w", sp.Name, err)
		}
		images[sp.Name] = img
	}

	// The §10 guarantee of a post hook — "a failure never switches" — only
	// holds for a service that actually switches: routed, zero-downtime
	// eligible, with a resolvable health check. Refused NOW, before anything
	// is mutated: a guarantee that silently degrades is worse than none.
	// (Previews run no hooks at all, so there is nothing to guarantee.)
	for _, sp := range plan.Services {
		if sp.PostCommand == "" || r.preview != nil {
			continue
		}
		if !routed[sp.Name] {
			return fmt.Errorf("service %s: a post-deployment command requires a routed service — without a switch there is no candidate to run it in (§10, INV-005)", sp.Name)
		}
		if eligible, reason := zeroDowntimeEligibility(sp, r.app.BuildConfig.RawCompose, images[sp.Name].HasHealthcheck); !eligible {
			return fmt.Errorf("service %s: a post-deployment command requires the zero-downtime switch, and this service is ineligible: %s (§10, INV-005)", sp.Name, reason)
		}
	}

	// An adopted stack awaiting normalization (§20.7): the containers of the
	// original compose project are retired NOW — after every image is pulled,
	// before anything is created. They hold the host ports and the volume
	// locks the new stack needs; the volumes themselves survive (they are
	// declared external under their original names). Brief interruption,
	// announced at adoption time — data loss is what §20.7 forbids, not a
	// restart during the normalizing redeployment.
	// Never from a preview: the adopted project IS production — a PR instance
	// retiring it would be the §20.7 data-loss scenario by another road.
	if p := adoption.ParsePointer(r.app.Resource.Adoption); r.preview == nil && p != nil && p.ComposeProject != "" {
		// AkerDock containers never carry the compose-CLI labels, so the
		// project filter only matches the original stack.
		if err := r.step(ctx, "retire_adopted_stack", func() (*sshexec.Result, error) {
			return r.client.Run(ctx, fmt.Sprintf(
				`ids=$(docker ps -aq --filter label=com.docker.compose.project=%s); `+
					`[ -n "$ids" ] && { docker stop -t 30 $ids >/dev/null 2>&1; docker rm $ids >/dev/null 2>&1; } || true; echo retired`,
				shellQuote(p.ComposeProject)))
		}); err != nil {
			return err
		}
	}

	// --- per-service replacement, in topological order (§8.2) --------------
	if err := r.setStatus(ctx, store.DeploymentStatusStarting); err != nil {
		return err
	}
	for _, sp := range plan.Services {
		if err := r.checkpoint(ctx); err != nil {
			return err
		}
		if resumed {
			// §4 switching, per service: what exists on the server decides.
			// A surviving healthy candidate means the crash hit mid-switch —
			// the promotion is FINISHED, never replayed (INV-004/005).
			done, err := r.resumeComposeService(ctx, plan, sp, appUUID, componentIDs)
			if err != nil {
				return fmt.Errorf("service %s: %w", sp.Name, err)
			}
			if done {
				continue
			}
		}
		if err := r.replaceComposeService(ctx, plan, sp, appDir, appUUID, labels, stackKeys, routed[sp.Name], images[sp.Name]); err != nil {
			if id, ok := componentIDs[sp.Name]; ok {
				_ = r.h.Store.SetServiceComponentObserved(ctx, store.SetServiceComponentObservedParams{
					ID: id, ObservedStatus: store.ResourceObservedStatusUnhealthy,
				})
			}
			return fmt.Errorf("service %s: %w", sp.Name, err)
		}
		observed := store.ResourceObservedStatusHealthy
		if sp.OneShot {
			observed = store.ResourceObservedStatusExited
		}
		if id, ok := componentIDs[sp.Name]; ok {
			_ = r.h.Store.SetServiceComponentObserved(ctx, store.SetServiceComponentObservedParams{
				ID: id, ObservedStatus: observed,
			})
		}
	}

	// --- routing + finishing ------------------------------------------------
	if err := r.setStatus(ctx, store.DeploymentStatusSwitching); err != nil {
		return err
	}
	if r.preview != nil {
		if err := r.applyComposePreviewRouting(ctx, content, plan, appUUID); err != nil {
			return err
		}
	} else if err := r.applyRouting(ctx, appUUID); err != nil {
		return err
	}
	return r.finish(ctx, appUUID)
}

// cloneForCompose is the clone half of buildFromGit: resolve the branch to an
// immutable SHA, then shallow-clone it into the per-deployment directory.
func (r *deploymentRun) cloneForCompose(ctx context.Context, _, appDir string) (string, string, error) {
	repoURL := *r.app.Application.GitRepositoryUrl
	branch := "main"
	if r.app.Application.GitBranch != nil && *r.app.Application.GitBranch != "" {
		branch = *r.app.Application.GitBranch
	}
	if err := r.setStatus(ctx, store.DeploymentStatusCloning); err != nil {
		return "", "", err
	}
	gitEnv, cleanup, err := r.installDeployKey(ctx, appDir)
	if err != nil {
		return "", "", err
	}
	defer cleanup()

	var sha string
	if r.preview != nil && r.preview.HeadSha != nil && *r.preview.HeadSha != "" {
		// A preview deploys the PR head, pinned at delivery time (§20.4).
		sha = *r.preview.HeadSha
		r.skipStep(ctx, "resolve_sha", "preview head "+sha[:min(12, len(sha))])
		_ = r.h.Store.SetDeploymentCommit(ctx, store.SetDeploymentCommitParams{ID: r.d.ID, CommitSha: &sha, GitBranch: r.preview.SourceBranch})
	} else if err := r.step(ctx, "resolve_sha", func() (*sshexec.Result, error) {
		res, err := r.client.Run(ctx, fmt.Sprintf("%sgit ls-remote %s refs/heads/%s",
			gitEnv, shellQuote(repoURL), shellQuote(branch)))
		if err == nil && res.ExitCode == 0 {
			sha, _, _ = strings.Cut(firstLine(res.Stdout), "\t")
			sha = strings.TrimSpace(sha)
			if len(sha) != 40 {
				return res, fmt.Errorf("branch %q not found on %s", branch, repoURL)
			}
			_ = r.h.Store.SetDeploymentCommit(ctx, store.SetDeploymentCommitParams{ID: r.d.ID, CommitSha: &sha, GitBranch: &branch})
		}
		return res, err
	}); err != nil {
		return "", "", err
	}

	// A fork's commit does not exist in the base repository: the PR head ref
	// is the only fetchable name, and it must still carry the announced SHA
	// (§20.4.8 — same discipline as the single-container path).
	fetchRef := sha
	if ref := r.previewFetchRef(); ref != "" {
		fetchRef = ref
	}
	srcDir := fmt.Sprintf("%s/source/%s", appDir, pguuid.String(r.d.Uuid))
	if err := r.step(ctx, "clone", func() (*sshexec.Result, error) {
		return r.client.Run(ctx, fmt.Sprintf(
			"rm -rf %s && mkdir -p %s && cd %s && git init -q && git remote add origin %s && %sgit fetch -q --depth 1 origin %s && git checkout -q --detach FETCH_HEAD",
			srcDir, srcDir, srcDir, shellQuote(repoURL), gitEnv, shellQuote(fetchRef)))
	}); err != nil {
		return "", "", err
	}
	if fetchRef != sha {
		if err := r.step(ctx, "verify_head", func() (*sshexec.Result, error) {
			res, err := r.client.Run(ctx, "cd "+srcDir+" && git rev-parse HEAD")
			if err == nil && res.ExitCode == 0 {
				got := firstLine(res.Stdout)
				if got != sha {
					return res, fmt.Errorf("%s resolved to %s, but the delivery announced %s — the pull request moved", fetchRef, got, sha)
				}
			}
			return res, err
		}); err != nil {
			return "", "", err
		}
	}

	// Author and subject of the checked-out commit — best effort, "who last
	// pushed" for the deployment view (same as the single-container path).
	if res, err := r.client.Run(ctx, "cd "+srcDir+" && git log -1 --format='%an%x1f%s'"); err == nil && res.ExitCode == 0 {
		author, message, _ := strings.Cut(strings.TrimRight(res.Stdout, "\n"), "\x1f")
		var authorPtr, messagePtr *string
		if author = strings.TrimSpace(author); author != "" {
			authorPtr = &author
			r.d.CommitAuthor = &author
		}
		if message = strings.TrimSpace(message); message != "" {
			messagePtr = &message
			r.d.CommitMessage = &message
		}
		if authorPtr != nil || messagePtr != nil {
			_ = r.h.Store.SetDeploymentCommitMeta(ctx, store.SetDeploymentCommitMetaParams{
				ID: r.d.ID, CommitAuthor: authorPtr, CommitMessage: messagePtr,
			})
		}
	}
	return srcDir, sha, nil
}

// syncComponents mirrors the plan's services into service_components
// (data dictionary §9.2): upserts keep row identity stable, services removed
// from the file lose their row (and, by CASCADE, domains and backup plans).
func (r *deploymentRun) syncComponents(ctx context.Context, plan *compose.Plan) (map[string]int64, error) {
	names := make([]string, 0, len(plan.Services))
	componentIDs := map[string]int64{}
	for _, sp := range plan.Services {
		names = append(names, sp.Name)
		var image *string
		if sp.Image != "" {
			image = &sp.Image
		}
		var engine *store.DbEngine
		if sp.DatabaseEngine != "" {
			e := store.DbEngine(sp.DatabaseEngine)
			engine = &e
		}
		var routePort *int32
		if sp.DefaultRoutePort > 0 {
			p := int32(sp.DefaultRoutePort)
			routePort = &p
		}
		component, err := r.h.Store.UpsertServiceComponent(ctx, store.UpsertServiceComponentParams{
			ResourceID: r.app.Resource.ID, Name: sp.Name, Image: image,
			IsDatabase: sp.IsDatabase, DatabaseEngine: engine, ExcludeFromHc: sp.ExcludeFromHC,
			DefaultRoutePort: routePort,
		})
		if err != nil {
			return nil, fmt.Errorf("sync component %s: %w", sp.Name, err)
		}
		componentIDs[sp.Name] = component.ID
	}
	if _, err := r.h.Store.DeleteVanishedServiceComponents(ctx, store.DeleteVanishedServiceComponentsParams{
		ResourceID: r.app.Resource.ID, Names: names,
	}); err != nil {
		return nil, err
	}
	return componentIDs, nil
}

// plainEnvVars decrypts the stack's variables into the interpolation map
// (compose-spec §3.2) — never logged, never in argv (INV-003).
func (r *deploymentRun) plainEnvVars(ctx context.Context) (map[string]string, error) {
	if r.preview != nil {
		// The DEDICATED preview set (INV-010): production secrets and shared
		// scopes never reach a PR instance — plus the predefined preview
		// variables (§5.6), so ${AKERDOCK_URL} interpolates in the file too.
		rows, err := r.h.Store.ListPreviewEnvVars(ctx, store.ListPreviewEnvVarsParams{ResourceID: r.app.Resource.ID, PreviewID: &r.preview.ID})
		if err != nil {
			return nil, err
		}
		// The preview's own identity ({{deployment.*}}) resolves in values too —
		// not a production secret, so it is INV-010-safe unlike the shared scopes.
		depEnv := sharedEnv{}
		r.mergeDeploymentRefs(&depEnv, r.deploymentRefs(ctx))
		vars := make(map[string]string, len(rows)+3)
		for _, v := range rows {
			plaintext, err := r.h.Keyring.Decrypt("environment_variables", "value_enc", pguuid.String(v.Uuid), v.ValueEnc)
			if err != nil {
				return nil, fmt.Errorf("decrypt variable %s: %w", v.Key, err)
			}
			value := string(plaintext)
			if !v.IsLiteral {
				value = depEnv.interpolate(value)
			}
			vars[v.Key] = value
		}
		vars["AKERDOCK_PR_ID"] = fmt.Sprint(r.preview.PrID)
		if r.preview.Fqdn != nil && *r.preview.Fqdn != "" {
			vars["AKERDOCK_FQDN"] = *r.preview.Fqdn
			vars["AKERDOCK_URL"] = "https://" + *r.preview.Fqdn
		}
		return vars, nil
	}
	rows, err := r.h.Store.ListEnvVarsForDeploy(ctx, r.app.Resource.ID)
	if err != nil {
		return nil, err
	}
	// Shared scopes merged by the caller, as compose-spec §3.2 prescribes:
	// {{scope.KEY}} references resolve inside stack variable values, and the
	// server-scoped variables join the interpolation set unless the stack
	// defines the key itself.
	shared, err := resolveSharedEnv(ctx, r.h.Store, r.h.Keyring, r.app.Resource.ID)
	if err != nil {
		return nil, err
	}
	// {{deployment.fqdn|url|pr_id}} resolves inside stack values too (§5.4): the
	// deployment's own public identity, so a compose service can carry a CORS
	// origin that follows the app's domain.
	dep := r.deploymentRefs(ctx)
	r.mergeDeploymentRefs(&shared, dep)
	vars := make(map[string]string, len(rows)+len(shared.server))
	for k, v := range shared.server {
		vars[k] = v
	}
	for _, v := range rows {
		plaintext, err := r.h.Keyring.Decrypt("environment_variables", "value_enc", pguuid.String(v.Uuid), v.ValueEnc)
		if err != nil {
			return nil, fmt.Errorf("decrypt variable %s: %w", v.Key, err)
		}
		value := string(plaintext)
		if !v.IsLiteral {
			value = shared.interpolate(value)
		}
		vars[v.Key] = value
	}
	// Predefined standalone vars, also so ${AKERDOCK_URL} interpolates in the
	// compose file itself (§5.6). A stack variable of the same name wins.
	if fqdn := dep["deployment.fqdn"]; fqdn != "" {
		if _, ok := vars["AKERDOCK_FQDN"]; !ok {
			vars["AKERDOCK_FQDN"] = fqdn
		}
		if _, ok := vars["AKERDOCK_URL"]; !ok {
			vars["AKERDOCK_URL"] = dep["deployment.url"]
		}
	}
	return vars, nil
}

// ensureMagicVariables generates and persists the missing credential-type
// magic variables (compose-spec §4.3) and resolves FQDN/URL references from
// the components' domains — or, for a preview, from the preview's own
// derived FQDNs (§20.4.1). The map is updated in place for the second load.
func (r *deploymentRun) ensureMagicVariables(ctx context.Context, content string, plan *compose.Plan, services []string, vars map[string]string) error {
	refs, _ := compose.ScanMagicReferences(content, services)
	var previewRoutes map[string]previewComposeRoute
	if r.preview != nil {
		previewRoutes = r.composePreviewRoutes(ctx, content, plan)
	}
	for _, ref := range refs {
		if _, exists := vars[ref.Name]; exists {
			continue
		}
		switch {
		case ref.Credential:
			value, err := compose.GenerateMagicValue(ref)
			if err != nil {
				return err
			}
			u, err := pguuid.New()
			if err != nil {
				return err
			}
			enc, err := r.h.Keyring.Encrypt("environment_variables", "value_enc", pguuid.String(u), []byte(value))
			if err != nil {
				return err
			}
			// A preview's generated credentials live in the PREVIEW variable
			// set (INV-010): they are the preview's own, never production's.
			var inserted int64
			if r.preview != nil {
				inserted, err = r.h.Store.CreateGeneratedPreviewEnvVar(ctx, store.CreateGeneratedPreviewEnvVarParams{
					Uuid: u, ResourceID: r.app.Resource.ID, Key: ref.Name, ValueEnc: enc,
				})
			} else {
				inserted, err = r.h.Store.CreateGeneratedEnvVar(ctx, store.CreateGeneratedEnvVarParams{
					Uuid: u, ResourceID: r.app.Resource.ID, Key: ref.Name, ValueEnc: enc, IsSecret: true,
				})
			}
			if err != nil {
				return fmt.Errorf("persist magic variable %s: %w", ref.Name, err)
			}
			if inserted == 0 {
				// Raced by a concurrent writer or created between the list and
				// now: the stored value is the truth, ours is discarded.
				stored, err := r.plainEnvVars(ctx)
				if err != nil {
					return err
				}
				value = stored[ref.Name]
			}
			vars[ref.Name] = value
		default:
			// FQDN/URL: resolved from the component's domains (or the preview
			// route map); left undefined (warning) when there is none.
			fqdn := ""
			if r.preview != nil {
				for name, route := range previewRoutes {
					if compose.NormalizeComponentID(name) == ref.ID {
						fqdn = route.FQDN
						break
					}
				}
			} else {
				fqdn = r.componentFQDN(ctx, ref)
			}
			if fqdn != "" {
				if ref.Type == compose.MagicURL {
					vars[ref.Name] = "https://" + fqdn
				} else {
					vars[ref.Name] = fqdn
				}
			}
		}
	}
	return nil
}

// previewComposeRoute is one served service of a preview stack.
type previewComposeRoute struct {
	FQDN string
	Port int
}

// composePreviewRoutes decides which services of a preview stack are served
// and on which port (§20.4.1): the SERVICE_FQDN/URL declarations of the
// compose file plus the components routed in production (§5.6 parity — the
// preview serves what production serves). A single served service takes the
// preview's own fqdn; several each take "<service>-<fqdn>", kept ONE dns
// level deep so the server wildcard still covers them. Cached: the magic
// pass, the network wiring and the final routing must all see the same map.
func (r *deploymentRun) composePreviewRoutes(ctx context.Context, content string, plan *compose.Plan) map[string]previewComposeRoute {
	if r.previewComposeRouted != nil {
		return r.previewComposeRouted
	}
	routes := map[string]previewComposeRoute{}
	r.previewComposeRouted = routes
	if r.preview.Fqdn == nil || *r.preview.Fqdn == "" {
		return routes // no fqdn resolved: the preview runs unrouted
	}
	base := *r.preview.Fqdn

	// Preview route table (ADR-035): {{service}} row is the per-service pattern;
	// explicit rows override a specific service (resolved by port); absent → the
	// legacy base / <service>-base scheme.
	random := previewRandom(r.preview)
	domain := ""
	if appDomains, err := r.h.Store.ListDomainsForApplication(ctx, &r.app.Resource.ID); err == nil && len(appDomains) > 0 {
		domain = appDomains[0].Fqdn
	}
	templates := previewTemplates(r.app)
	svcTmpl := serviceTemplate(templates)

	names := make([]string, 0, len(plan.Services))
	plans := map[string]compose.ServicePlan{}
	for _, sp := range plan.Services {
		names = append(names, sp.Name)
		plans[sp.Name] = sp
	}

	components, _ := r.h.Store.ListServiceComponents(ctx, r.app.Resource.ID)
	// Explicit rows ({{service}}-free) map to the component exposing their port
	// (app-domain-style resolve) — that service takes that host and port.
	explicitByService := map[string]previewRouteTemplate{}
	for _, row := range explicitTemplates(templates) {
		var portPtr *int32
		if row.Port != nil {
			p := int32(*row.Port)
			portPtr = &p
		}
		c, err := resolveWebComponent(components, portPtr)
		if err != nil {
			continue
		}
		if _, inStack := plans[c.Name]; !inStack {
			continue
		}
		explicitByService[c.Name] = row
	}

	// ports maps every SERVED service to its target port; 0 means "served,
	// port still to resolve from the plan default".
	ports := map[string]int{}
	// Explicit intent: SERVICE_FQDN_<ID> / SERVICE_URL_<ID> in the file.
	refs, _ := compose.ScanMagicReferences(content, names)
	for _, ref := range refs {
		if ref.Credential {
			continue
		}
		for _, name := range names {
			if compose.NormalizeComponentID(name) == ref.ID {
				if ref.Port > 0 || ports[name] == 0 {
					ports[name] = ref.Port
				}
				break
			}
		}
	}
	// Production parity: a component served in production is served in the
	// preview too (read-only — the preview never touches the domain rows).
	if len(components) > 0 {
		for _, c := range components {
			if _, inStack := plans[c.Name]; !inStack {
				continue
			}
			domains, err := r.h.Store.ListServiceComponentDomains(ctx, &c.ID)
			if err != nil || len(domains) == 0 {
				continue
			}
			if ports[c.Name] == 0 {
				switch {
				case domains[0].TargetPort != nil:
					ports[c.Name] = int(*domains[0].TargetPort)
				case c.DefaultRoutePort != nil:
					ports[c.Name] = int(*c.DefaultRoutePort)
				default:
					ports[c.Name] = 0
				}
			}
		}
		// APPLICATION-level domains route to the stack's web component in
		// production (compose-spec §6): the preview serves that component
		// too — same resolver, same determinism. Without this, a stack whose
		// only domain lives on the application deploys its previews green
		// and unrouted: no route, no certificate, a default-cert dead end.
		if appDomains, err := r.h.Store.ListDomainsForApplication(ctx, &r.app.Resource.ID); err == nil {
			for _, d := range appDomains {
				c, err := resolveWebComponent(components, d.TargetPort)
				if err != nil {
					continue
				}
				if _, inStack := plans[c.Name]; !inStack {
					continue
				}
				if _, already := ports[c.Name]; already {
					continue
				}
				switch {
				case d.TargetPort != nil:
					ports[c.Name] = int(*d.TargetPort)
				case c.DefaultRoutePort != nil:
					ports[c.Name] = int(*c.DefaultRoutePort)
				default:
					ports[c.Name] = 0
				}
			}
		}
	}

	served := make([]string, 0, len(ports))
	for name, port := range ports {
		if port == 0 {
			port = plans[name].DefaultRoutePort
		}
		if port == 0 {
			// No resolvable port: same rule as production routing — never a
			// guessed port (compose-spec §6), the service stays unserved.
			r.h.Logger.Warn("preview service has no resolvable port, unrouted",
				"service", name, "preview", pguuid.String(r.preview.Uuid))
			continue
		}
		ports[name] = port
		served = append(served, name)
	}
	// An explicit row can serve a component that has no domain of its own.
	for name := range explicitByService {
		if _, ok := ports[name]; !ok {
			ports[name] = 0
			if p := explicitByService[name].Port; p != nil {
				ports[name] = *p
			}
			served = append(served, name)
		}
	}

	prID := int(r.preview.PrID)
	// A template needing {{domain}} we do not have is unusable — fall back.
	usable := func(host string) bool { return domain != "" || !strings.Contains(host, "{{domain}}") }

	sort.Strings(served)
	for _, name := range served {
		port := ports[name]
		if port == 0 {
			port = plans[name].DefaultRoutePort
		}
		fqdn := ""
		switch {
		case explicitByService[name].Host != "" && usable(explicitByService[name].Host):
			row := explicitByService[name]
			fqdn = resolvePreviewHost(row.Host, prID, domain, name, random)
			if row.Port != nil {
				port = *row.Port
			}
		case svcTmpl != nil && usable(svcTmpl.Host):
			fqdn = resolvePreviewHost(svcTmpl.Host, prID, domain, name, random)
			if svcTmpl.Port != nil {
				port = *svcTmpl.Port
			}
		default:
			fqdn = base
			if len(served) > 1 {
				fqdn = strings.ToLower(strings.ReplaceAll(name, "_", "-")) + "-" + base
			}
		}
		if port == 0 {
			continue // never a guessed port (compose-spec §6)
		}
		routes[name] = previewComposeRoute{FQDN: fqdn, Port: port}
	}
	return routes
}

// applyComposePreviewRouting serves the preview stack (§20.4.1): every route
// targets its own component container and carries the preview protection —
// basic auth by default — and X-Robots-Tag: noindex, exactly like a
// single-container preview (§20.4.4).
func (r *deploymentRun) applyComposePreviewRouting(ctx context.Context, content string, plan *compose.Plan, appUUID string) error {
	if r.server.ProxyType != store.ProxyTypeTraefik {
		return nil
	}
	routes := r.composePreviewRoutes(ctx, content, plan)
	names := make([]string, 0, len(routes))
	for name := range routes {
		names = append(names, name)
	}
	sort.Strings(names)
	rg := proxy.RouteGroup{AppUUID: appUUID, ForceHTTPS: true}
	for _, name := range names {
		route := routes[name]
		rg.Routes = append(rg.Routes, proxy.Route{
			FQDN: route.FQDN, Path: "/", TargetPort: route.Port,
			// The component's own container: StackUUID is the preview uuid,
			// so the name is already preview-scoped (INV-011).
			Endpoint: plans(plan)[name].ContainerName,
		})
	}
	// Scale-to-zero (ADR-036): route the stack's traffic through the waker,
	// which forwards to each component and wakes the stack on demand. The waker
	// routes by Host, so only the service target changes — the protection
	// middlewares injected below are untouched.
	if r.app.Application.PreviewScaleToZero && len(rg.Routes) > 0 {
		wcfg := wakerConfigFromRouteGroup(appUUID, rg, stackWakeOrder(plan))
		if err := ensureWaker(ctx, r.client, r.dest.Network, r.h.WakerImage, appUUID, wcfg); err != nil {
			return err
		}
		rg = pointRouteGroupAtWaker(rg)
	}
	routingContent := ""
	if len(rg.Routes) > 0 {
		ssoURL, err := r.previewSSOAuthURL(ctx)
		if err != nil {
			return err
		}
		routingContent = injectPreviewMiddlewares(proxy.GenerateDynamic(rg, r.d.ID), appUUID,
			r.app.Application.PreviewProtection, r.previewAuthHash(ctx), ssoURL)
		if r.app.Application.PreviewProtection == store.PreviewProtectionSso && ssoURL != "" {
			hosts := make([]string, 0, len(rg.Routes))
			for _, route := range rg.Routes {
				hosts = append(hosts, route.FQDN)
			}
			routingContent = injectPreviewSSOCallback(routingContent, appUUID, hosts,
				strings.TrimSuffix(ssoURL, "/webhooks/previews/forward-auth"))
		}
	}
	applier := &ProxyApplier{Store: r.h.Store, Client: r.client, Server: r.server, Network: r.dest.Network}
	return r.step(ctx, "apply_routing", func() (*sshexec.Result, error) {
		return nil, applier.Apply(ctx, appUUID, routingContent, "")
	})
}

// plans indexes a compose plan's services by name.
func plans(plan *compose.Plan) map[string]compose.ServicePlan {
	out := make(map[string]compose.ServicePlan, len(plan.Services))
	for _, sp := range plan.Services {
		out[sp.Name] = sp
	}
	return out
}

// componentFQDN returns the first domain of the component named by the
// reference. When the component has none, referencing SERVICE_FQDN_<ID> is a
// declaration of intent (§6): a domain is generated from the server wildcard
// — no wildcard, no generation, and the variable stays undefined (warned).
func (r *deploymentRun) componentFQDN(ctx context.Context, ref compose.MagicRef) string {
	components, err := r.h.Store.ListServiceComponents(ctx, r.app.Resource.ID)
	if err != nil {
		return ""
	}
	for _, c := range components {
		if compose.NormalizeComponentID(c.Name) != ref.ID {
			continue
		}
		domains, err := r.h.Store.ListServiceComponentDomains(ctx, &c.ID)
		if err != nil {
			return ""
		}
		if len(domains) > 0 {
			return domains[0].Fqdn
		}
		if r.server.WildcardDomain == nil || *r.server.WildcardDomain == "" {
			return ""
		}
		appUUID := pguuid.String(r.app.Resource.Uuid)
		fqdn := fmt.Sprintf("%s-%s.%s",
			strings.ToLower(strings.ReplaceAll(c.Name, "_", "-")), appUUID[:8], *r.server.WildcardDomain)
		u, err := pguuid.New()
		if err != nil {
			return ""
		}
		var targetPort *int32
		if ref.Port > 0 {
			p := int32(ref.Port)
			targetPort = &p
		}
		if _, err := r.h.Store.CreateComponentDomain(ctx, store.CreateComponentDomainParams{
			Uuid: u, ServiceComponentID: &c.ID, Fqdn: fqdn, TargetPort: targetPort,
		}); err != nil {
			// ON CONFLICT DO NOTHING + :one returns no rows when the fqdn is
			// already taken elsewhere: the intent loses, the variable stays
			// undefined rather than pointing at someone else's route.
			r.h.Logger.Warn("generated component domain conflicts, skipped", "fqdn", fqdn, "component", c.Name)
			return ""
		}
		return fqdn
	}
	return ""
}

// routedComponents maps service name -> true when the component carries at
// least one domain: those must join the destination network so the proxy can
// reach them (§2.1).
func (r *deploymentRun) routedComponents(ctx context.Context, componentIDs map[string]int64) (map[string]bool, error) {
	out := map[string]bool{}
	for name, id := range componentIDs {
		domains, err := r.h.Store.ListServiceComponentDomains(ctx, &id)
		if err != nil {
			return nil, err
		}
		out[name] = len(domains) > 0
	}
	// Application-level domains resolve to the stack's web component
	// (compose-spec §6): that component must join the destination network
	// too, or the route just fixed would point at a container the proxy
	// cannot reach — a correct route to an unreachable target is still a 502.
	appDomains, err := r.h.Store.ListDomainsForApplication(ctx, &r.app.Resource.ID)
	if err != nil {
		return nil, err
	}
	if len(appDomains) > 0 {
		components, err := r.h.Store.ListServiceComponents(ctx, r.app.Resource.ID)
		if err != nil {
			return nil, err
		}
		for _, d := range appDomains {
			if c, err := resolveWebComponent(components, d.TargetPort); err == nil {
				out[c.Name] = true
			}
		}
	}
	return out, nil
}

// reportFindings traces every finding in the deployment log (compose-spec
// §11) and returns an error when at least one blocks.
func (r *deploymentRun) reportFindings(ctx context.Context, findings []compose.Finding) error {
	if len(findings) == 0 {
		r.skipStep(ctx, "validate_compose", "no findings")
		return nil
	}
	var b strings.Builder
	for _, f := range findings {
		fmt.Fprintf(&b, "%s\n", f.String())
	}
	summary := b.String()
	return r.step(ctx, "validate_compose", func() (*sshexec.Result, error) {
		res := &sshexec.Result{Stdout: summary}
		if compose.HasErrors(findings) {
			res.ExitCode = 1
			return res, fmt.Errorf("compose validation failed — see the findings above")
		}
		return res, nil
	})
}

// composeImage is one resolved image of the stack: the reference to create
// containers from (digest-pinned when the registry provides one, §18.3) and
// whether the image ships its own HEALTHCHECK (§7.1).
type composeImage struct {
	Ref            string
	HasHealthcheck bool
}

// ensureComposeImage builds or pulls one service's image — before any
// container of the stack is touched (§8.2 step 2).
func (r *deploymentRun) ensureComposeImage(ctx context.Context, sp compose.ServicePlan, workDir, labels, sha string) (composeImage, error) {
	svc := sp.Service
	ref := sp.Image

	if sp.Build {
		buildCtx := "."
		if svc.Build.Context != "" {
			buildCtx = "./" + strings.TrimPrefix(svc.Build.Context, "./")
		}
		dockerfile := "Dockerfile"
		if svc.Build.Dockerfile != "" {
			dockerfile = svc.Build.Dockerfile
		}
		ref = sp.BuildImage + ":" + sha[:12]
		args := ""
		for key, value := range svc.Build.Args {
			if value == nil {
				continue
			}
			args += fmt.Sprintf(" --build-arg %s=%s", key, shellQuote(*value))
		}
		// Streamed: a service image build is often the longest part of a compose
		// deploy — its progress must show live, not arrive in one block at the end.
		if err := r.streamStep(ctx, "build_"+sp.Name, func(onOutput func(string)) (*sshexec.Result, error) {
			return r.client.RunStream(ctx, fmt.Sprintf(
				"cd %s/%s && DOCKER_BUILDKIT=1 docker build --file %s --progress plain%s --tag %s %s --label akerdock.commit_sha=%s --label akerdock.component=%s .",
				workDir, strings.TrimPrefix(buildCtx, "./"), dockerfile, args, ref, labels, sha, sp.Name), onOutput)
		}); err != nil {
			return composeImage{}, err
		}
	} else if err := r.streamStep(ctx, "pull_"+sp.Name, func(onOutput func(string)) (*sshexec.Result, error) {
		// A pull of a large image is a long, opaque wait otherwise.
		return r.client.RunStream(ctx, "docker pull "+ref, onOutput)
	}); err != nil {
		return composeImage{}, err
	}

	// Digest resolution (§18.3): what the stack runs is provably what was
	// pulled — a moved tag between two services cannot split the stack.
	img := composeImage{Ref: ref}
	if err := r.step(ctx, "resolve_"+sp.Name, func() (*sshexec.Result, error) {
		res, err := r.client.Run(ctx, fmt.Sprintf(
			"docker image inspect --format '{{if .RepoDigests}}{{index .RepoDigests 0}}{{end}}|{{if .Config.Healthcheck}}yes{{else}}no{{end}}' %s", ref))
		if err == nil && res.ExitCode == 0 {
			digest, hc, _ := strings.Cut(firstLine(res.Stdout), "|")
			if !sp.Build && digest != "" {
				img.Ref = digest
			}
			img.HasHealthcheck = hc == "yes"
		}
		return res, err
	}); err != nil {
		return composeImage{}, err
	}
	return img, nil
}

// composeConfigHash fingerprints the desired state of one service: the fully
// rendered create command (with the deployment-scoped labels blanked) plus
// the environment content. Same hash on the running container = the service
// is unchanged and is not replaced (§8.2 step 1).
func composeConfigHash(createCommand, envContent string) string {
	sum := sha256.Sum256([]byte(createCommand + "\x00" + envContent))
	return hex.EncodeToString(sum[:6])
}

// zeroDowntimeEligibility decides how a WEB service (one with a domain) is
// replaced (§8.4). The returned reason names why a web service falls back to
// recreate — surfaced in the deployment log, never silent.
func zeroDowntimeEligibility(sp compose.ServicePlan, raw, imageHealthcheck bool) (bool, string) {
	switch {
	case raw:
		return false, "raw compose mode (§9)"
	case sp.ZeroDowntimeOptOut:
		return false, "x-akerdock.zero_downtime: false"
	case sp.HasHostPorts:
		return false, "host port mapping — two instances cannot bind the same port"
	case (sp.Health == nil || sp.Health.Disable || len(sp.Health.Test) == 0) && !imageHealthcheck:
		return false, "no resolved healthcheck (compose or image)"
	default:
		return true, ""
	}
}

// replaceComposeService replaces one service of the stack: untouched when
// unchanged, zero-downtime switch when web and eligible, recreate otherwise.
func (r *deploymentRun) replaceComposeService(ctx context.Context, plan *compose.Plan, sp compose.ServicePlan, appDir, appUUID, labels string, stackKeys []string, routedComponent bool, img composeImage) error {
	envPath, envKeys, envContent, err := r.uploadComposeEnv(ctx, sp, appDir)
	if err != nil {
		return err
	}
	allKeys := append(append([]string{}, stackKeys...), envKeys...)

	// The hash is computed over the deployment-invariant parts only: the
	// per-deployment labels would make every service look changed.
	hash := composeConfigHash(
		r.composeCreateCommand(plan, sp, appDir, "", envPath, allKeys, img.Ref, composeCreateOpts{Name: sp.ContainerName, Aliases: sp.Aliases}),
		envContent)
	labeled := labels + " --label akerdock.config_hash=" + hash

	if !r.d.ForceRebuild && !sp.OneShot {
		currentHash, running := r.containerConfigState(ctx, sp.ContainerName)
		if currentHash == hash && running {
			// An unchanged container still converges its NETWORK membership:
			// network attachments are not part of the create command, so the
			// hash cannot see them — and becoming routed (a domain added
			// since the last deployment) must not require a recreate. An
			// already-connected network errors and is tolerated.
			for _, connect := range r.connectCommands(sp, sp.ContainerName, routedComponent, true) {
				_, _ = r.client.Run(ctx, connect+" >/dev/null 2>&1 || true")
			}
			r.skipStep(ctx, "start_"+sp.Name, "unchanged since the last deployment (config hash "+hash+")")
			return nil
		}
	}

	if sp.OneShot {
		return r.recreateComposeService(ctx, plan, sp, appDir, labeled, envPath, allKeys, img.Ref, routedComponent)
	}
	if routedComponent {
		switch {
		case r.preview != nil:
			// A preview is ephemeral by definition (§20.4.1): a redeploy may
			// interrupt it, and nobody is served by keeping two instances of
			// a PR stack alive through a switch.
			r.skipStep(ctx, "zero_downtime_"+sp.Name, "preview stack: recreate")
		default:
			eligible, reason := zeroDowntimeEligibility(sp, r.app.BuildConfig.RawCompose, img.HasHealthcheck)
			if eligible {
				return r.zeroDowntimeReplace(ctx, plan, sp, appDir, appUUID, labeled, envPath, allKeys, img.Ref)
			}
			// compose_zero_downtime_ineligible (§8.4): the interruption is
			// assumed and displayed, never silent.
			r.skipStep(ctx, "zero_downtime_"+sp.Name, "recreate with interruption: "+reason)
		}
	}
	return r.recreateComposeService(ctx, plan, sp, appDir, labeled, envPath, allKeys, img.Ref, routedComponent)
}

// uploadComposeEnv writes the per-service environment file (§2.7): values
// travel by stdin and are sourced by name, never argv (INV-003/012).
func (r *deploymentRun) uploadComposeEnv(ctx context.Context, sp compose.ServicePlan, appDir string) (string, []string, string, error) {
	svc := sp.Service
	var envFile strings.Builder
	envKeys := make([]string, 0, len(svc.Environment))
	for key, value := range svc.Environment {
		if value == nil {
			continue
		}
		envKeys = append(envKeys, key)
	}
	sort.Strings(envKeys)
	for _, key := range envKeys {
		fmt.Fprintf(&envFile, "export %s=%s\n", key, shellQuote(*svc.Environment[key]))
	}
	envPath := fmt.Sprintf("%s/env/%s.sh", appDir, sp.Name)
	if res, err := r.client.RunInput(ctx, "umask 077 && cat > "+envPath, envFile.String()); err != nil || res.ExitCode != 0 {
		return "", nil, "", fmt.Errorf("uploading the %s environment failed", sp.Name)
	}
	return envPath, envKeys, envFile.String(), nil
}

// containerConfigState reads the config hash label and run state of the
// current container ("", false when it does not exist).
func (r *deploymentRun) containerConfigState(ctx context.Context, name string) (string, bool) {
	res, err := r.client.Run(ctx, fmt.Sprintf(
		"docker inspect --format '{{index .Config.Labels \"akerdock.config_hash\"}} {{.State.Status}}' %s 2>/dev/null", name))
	if err != nil || res.ExitCode != 0 {
		return "", false
	}
	hash, status, _ := strings.Cut(firstLine(res.Stdout), " ")
	return hash, status == "running"
}

// composeVolumeSources lists the docker names of the NAMED volumes a service
// mounts — the ones whose ownership the empty-volume chown may need to fix.
// Binds and tmpfs are the operator's (or the kernel's) business.
func composeVolumeSources(sp compose.ServicePlan) []string {
	var vols []string
	for _, m := range sp.Mounts {
		if m.Type == "volume" {
			vols = append(vols, m.Source)
		}
	}
	return vols
}

// previewSeedScript initializes the still-empty preview volumes of one
// service from their production counterparts (ADR-029): `cp -a` inside a
// throwaway container of the service's image, production mounted READ-ONLY.
// A missing production volume skips the pair (nothing to clone — and `docker
// run -v` must not create it as a side effect); a failing copy fails the
// chain deliberately: the operator declared they want data, a silently empty
// database would betray that.
func previewSeedScript(imageRef string, pairs [][2]string) string {
	var b strings.Builder
	for _, p := range pairs {
		prod, preview := p[0], p[1]
		fmt.Fprintf(&b, "if docker volume inspect %s >/dev/null 2>&1; then ", prod)
		fmt.Fprintf(&b, "docker run --rm --user 0 --entrypoint /bin/sh -v %s:/akerdock-seed-from:ro -v %s:/akerdock-volume %s ",
			prod, preview, imageRef)
		b.WriteString(`-c '[ -n "$(ls -A /akerdock-volume)" ] || cp -a /akerdock-seed-from/. /akerdock-volume/'`)
		b.WriteString("; fi && ")
	}
	return strings.TrimSuffix(b.String(), " && ")
}

// previewSeedPairs matches this service's mounted volumes against the plan's
// preview_seed declarations and returns (production, preview) docker-name
// pairs. Empty outside previews: seeding is a PREVIEW contract only.
func (r *deploymentRun) previewSeedPairs(plan *compose.Plan, sp compose.ServicePlan) [][2]string {
	if r.preview == nil {
		return nil
	}
	appUUID := pguuid.String(r.app.Resource.Uuid)
	var pairs [][2]string
	for _, m := range sp.Mounts {
		if m.Type != "volume" {
			continue
		}
		declared, ok := plan.SeedVolumes[m.Source]
		if !ok {
			continue
		}
		pairs = append(pairs, [2]string{appUUID + "_" + declared, m.Source})
	}
	return pairs
}

// connectCommands attaches a container to its extra stack networks, and to
// the destination network when the proxy must reach it (§2.1).
func (r *deploymentRun) connectCommands(sp compose.ServicePlan, container string, routedComponent bool, shortAliases bool) []string {
	var connects []string
	for _, network := range sp.ExtraNetworks {
		alias := container
		if shortAliases {
			alias = sp.Name
		}
		connects = append(connects, fmt.Sprintf("docker network connect --alias %s %s %s", alias, network, container))
	}
	if routedComponent {
		connects = append(connects, fmt.Sprintf("docker network connect --alias %s %s %s", container, r.dest.Network, container))
	}
	return connects
}

// recreateComposeService is the stop-then-start path (§7.4 semantics): used
// for non-web services, one-shots, and web services ineligible to the switch.
func (r *deploymentRun) recreateComposeService(ctx context.Context, plan *compose.Plan, sp compose.ServicePlan, appDir, labels, envPath string, envKeys []string, runRef string, routedComponent bool) error {
	create := r.composeCreateCommand(plan, sp, appDir, labels, envPath, envKeys, runRef,
		composeCreateOpts{Name: sp.ContainerName, Aliases: sp.Aliases, ReplaceOld: true})
	commands := append([]string{create}, r.connectCommands(sp, sp.ContainerName, routedComponent, true)...)
	// Still-empty volumes are handed to the service image's runtime user
	// BEFORE the first start — same contract as the single-container packs:
	// a USER'd image must not crash-loop on its own storage.
	if vols := composeVolumeSources(sp); len(vols) > 0 {
		commands = append([]string{chownEmptyVolumesScript(runRef, vols)}, commands...)
	}
	// Preview seeding runs FIRST (ADR-029): `cp -a` carries the production
	// ownership over, so the chown right after sees a non-empty volume and
	// leaves it alone.
	if pairs := r.previewSeedPairs(plan, sp); len(pairs) > 0 {
		commands = append([]string{previewSeedScript(runRef, pairs)}, commands...)
	}
	commands = append(commands, "docker start "+sp.ContainerName)
	if err := r.step(ctx, "start_"+sp.Name, func() (*sshexec.Result, error) {
		return r.client.Run(ctx, strings.Join(commands, " && "))
	}); err != nil {
		return err
	}

	if sp.OneShot {
		// One-shot job (§7.3): runs at its topological position; a non-zero
		// exit fails the deployment deterministically.
		return r.step(ctx, "wait_"+sp.Name, func() (*sshexec.Result, error) {
			res, err := r.client.Run(ctx, fmt.Sprintf("code=$(timeout 600 docker wait %s); echo \"exit=$code\"; [ \"$code\" = 0 ]", sp.ContainerName))
			if err == nil && res.ExitCode != 0 {
				return res, fmt.Errorf("one-shot service exited non-zero (%s)", firstLine(res.Stdout))
			}
			return res, err
		})
	}
	if sp.ExcludeFromHC {
		r.skipStep(ctx, "healthcheck_"+sp.Name, "excluded from the stack health (x-akerdock.exclude_from_hc)")
		return nil
	}
	return r.waitComposeHealthy(ctx, sp, sp.ContainerName)
}

// zeroDowntimeReplace is the two-instance switch of one web service (§8.2
// step 4): candidate next to the old, healthy first, proxy switched for THIS
// component only, then promotion. A failure removes the candidate and leaves
// the old container serving (C2); services already switched stay (C3).
func (r *deploymentRun) zeroDowntimeReplace(ctx context.Context, plan *compose.Plan, sp compose.ServicePlan, appDir, appUUID, labels, envPath string, envKeys []string, runRef string) error {
	candidate := sp.CandidateName
	discard := func() { _, _ = r.client.Run(ctx, "docker rm -f "+candidate+" >/dev/null 2>&1 || true") }

	// The candidate joins the stack network WITHOUT the short alias (§8.3):
	// the other services keep resolving <service> to the OLD container until
	// the promotion — they never see the candidate early.
	create := r.composeCreateCommand(plan, sp, appDir, labels, envPath, envKeys, runRef,
		composeCreateOpts{Name: candidate, Aliases: []string{candidate}})
	commands := append([]string{"docker rm -f " + candidate + " >/dev/null 2>&1 || true", create},
		r.connectCommands(sp, candidate, true, false)...)
	// Same empty-volume ownership contract as the recreate path.
	if vols := composeVolumeSources(sp); len(vols) > 0 {
		commands = append([]string{chownEmptyVolumesScript(runRef, vols)}, commands...)
	}
	// Same preview seeding contract as the recreate path (ADR-029).
	if pairs := r.previewSeedPairs(plan, sp); len(pairs) > 0 {
		commands = append([]string{previewSeedScript(runRef, pairs)}, commands...)
	}
	commands = append(commands, "docker start "+candidate)
	if err := r.step(ctx, "start_candidate_"+sp.Name, func() (*sshexec.Result, error) {
		return r.client.Run(ctx, strings.Join(commands, " && "))
	}); err != nil {
		discard()
		return err
	}
	if err := r.waitComposeHealthy(ctx, sp, candidate); err != nil {
		discard()
		return err
	}

	// The post-deployment hook (§10, x-akerdock) runs in the HEALTHY
	// candidate, before its switch: a failure removes the candidate and the
	// old container keeps serving (C2) — exactly the §10 guarantee.
	if sp.PostCommand != "" {
		cmd := sp.PostCommand
		if err := r.runHook(ctx, "post_deployment_"+sp.Name, &cmd, candidate); err != nil {
			discard()
			return err
		}
	}

	// Cancellation barrier (§8.2): past this point this component's switch
	// runs to its end.
	if err := r.checkpoint(ctx); err != nil {
		discard()
		return err
	}
	if err := r.promoteComposeCandidate(ctx, plan, sp, appUUID); err != nil {
		discard()
		return err
	}
	return nil
}

// promoteComposeCandidate is the switch tail of one service (§8.2 step 4):
// route this component to the candidate's IP, stop the old container, take
// its name and short alias, then stabilize the routing on the name. It is
// exactly what a resume-by-inspection replays when a crash interrupted it —
// every step is idempotent.
func (r *deploymentRun) promoteComposeCandidate(ctx context.Context, plan *compose.Plan, sp compose.ServicePlan, appUUID string) error {
	candidate := sp.CandidateName

	// Route THIS component to the candidate's IP: the old container keeps
	// serving until the proxy really exposes the new endpoint (INV-005).
	var candidateIP string
	if err := r.step(ctx, "resolve_endpoint_"+sp.Name, func() (*sshexec.Result, error) {
		res, err := r.client.Run(ctx, fmt.Sprintf(
			"docker inspect --format '{{(index .NetworkSettings.Networks %q).IPAddress}}' %s", r.dest.Network, candidate))
		if err == nil && res.ExitCode == 0 {
			candidateIP = firstLine(res.Stdout)
			if candidateIP == "" {
				return res, fmt.Errorf("could not resolve the candidate IP on network %s", r.dest.Network)
			}
		}
		return res, err
	}); err != nil {
		return err
	}
	if err := r.switchComponentRouting(ctx, appUUID, sp.Name, candidateIP); err != nil {
		return err
	}

	// Promotion: stop the old, take its name, and give the promoted
	// container the short DNS alias on the stack network (§8.3). The alias
	// swap disconnects/reconnects the STACK network only — the proxy talks
	// over the destination network, so routing never blinks.
	grace := 30
	if sp.Service.StopGracePeriod != nil {
		grace = int(time.Duration(*sp.Service.StopGracePeriod).Seconds())
	}
	if err := r.step(ctx, "switch_"+sp.Name, func() (*sshexec.Result, error) {
		return r.client.Run(ctx, fmt.Sprintf(
			"(docker stop -t %d %s >/dev/null 2>&1 && docker rm %s >/dev/null 2>&1) || true; "+
				"docker rename %s %s && docker network disconnect %s %s >/dev/null 2>&1; "+
				"docker network connect --alias %s --alias %s %s %s",
			grace, sp.ContainerName, sp.ContainerName,
			candidate, sp.ContainerName, plan.NetworkName, sp.ContainerName,
			sp.Name, sp.ContainerName, plan.NetworkName, sp.ContainerName))
	}); err != nil {
		return err
	}
	// Stabilize the routing on the container NAME: the candidate IP dies with
	// the next replacement, the name does not.
	return r.switchComponentRouting(ctx, appUUID, sp.Name, "")
}

// resumeComposeService inspects what a crashed deployment left behind for ONE
// service (§4 switching, per service — compose-spec §8.2 "reprise possible").
// Returns true when the service needs nothing more from the normal path.
//
//   - a healthy surviving candidate: the crash hit mid-switch — FINISH the
//     promotion, never replay it (INV-004/005);
//   - a dead or unhealthy candidate: discard it, let the normal path redo
//     the replacement from scratch (C2 semantics);
//   - no candidate: nothing to decide — the config-hash check of the normal
//     path recognizes the services that were already completed.
func (r *deploymentRun) resumeComposeService(ctx context.Context, plan *compose.Plan, sp compose.ServicePlan, appUUID string, componentIDs map[string]int64) (bool, error) {
	res, err := r.client.Run(ctx, fmt.Sprintf(
		"docker inspect --format '{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' %s 2>/dev/null || echo absent",
		sp.CandidateName))
	if err != nil {
		return false, err
	}
	state := firstLine(res.Stdout)
	if state == "absent" || state == "" {
		return false, nil
	}
	status, health, _ := strings.Cut(state, " ")
	if status != "running" || health == "unhealthy" {
		r.skipStep(ctx, "resume_"+sp.Name, "stale candidate ("+state+") discarded; the service is redone from scratch")
		_, _ = r.client.Run(ctx, "docker rm -f "+sp.CandidateName+" >/dev/null 2>&1 || true")
		return false, nil
	}

	// The candidate survived the crash. Wait for its health verdict (it may
	// still be starting), then finish what the dead worker was about to do.
	r.skipStep(ctx, "resume_"+sp.Name, "healthy candidate found after the crash; finishing its interrupted switch")
	if err := r.waitComposeHealthy(ctx, sp, sp.CandidateName); err != nil {
		_, _ = r.client.Run(ctx, "docker rm -f "+sp.CandidateName+" >/dev/null 2>&1 || true")
		//nolint:nilerr // an unhealthy candidate is not a job failure: signal "redo from scratch".
		return false, nil
	}
	if err := r.promoteComposeCandidate(ctx, plan, sp, appUUID); err != nil {
		return true, err
	}
	if id, ok := componentIDs[sp.Name]; ok {
		_ = r.h.Store.SetServiceComponentObserved(ctx, store.SetServiceComponentObservedParams{
			ID: id, ObservedStatus: store.ResourceObservedStatusHealthy,
		})
	}
	return true, nil
}

// waitComposeHealthy waits for a container's health (compose or image
// healthcheck), or a stable running state when it has none.
func (r *deploymentRun) waitComposeHealthy(ctx context.Context, sp compose.ServicePlan, container string) error {
	return r.step(ctx, "healthcheck_"+sp.Name, func() (*sshexec.Result, error) {
		res, err := r.client.Run(ctx, fmt.Sprintf(
			"deadline=$(( $(date +%%s) + %d )); while [ $(date +%%s) -lt $deadline ]; do "+
				"st=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none-{{.State.Status}}{{end}}' %s 2>/dev/null); "+
				"case \"$st\" in healthy) echo healthy; exit 0;; unhealthy) echo unhealthy; exit 1;; "+
				"none-running) sleep 5; st2=$(docker inspect --format '{{.State.Status}}' %s); [ \"$st2\" = running ] && echo running && exit 0; echo \"$st2\"; exit 1;; esac; sleep 2; done; echo timeout; exit 1",
			r.composeHealthBudget(sp), container, container))
		if err == nil && res.ExitCode != 0 {
			logs, _ := r.client.Run(ctx, "docker logs --tail 100 "+container+" 2>&1")
			detail := ""
			if logs != nil {
				detail = "\n" + logs.Stdout
			}
			return res, fmt.Errorf("the service did not turn healthy (%s)%s", firstLine(res.Stdout), detail)
		}
		return res, err
	})
}

// composeHealthBudget bounds the health wait (§4): start_period +
// (interval + timeout) × retries + 30 s, with a floor for image healthchecks
// whose parameters are unknown here.
func (r *deploymentRun) composeHealthBudget(sp compose.ServicePlan) int {
	if sp.Health == nil {
		return 90
	}
	budget := int(sp.Health.StartPeriod.Seconds()) +
		int((sp.Health.Interval+sp.Health.Timeout).Seconds())*int(sp.Health.Retries) + 30
	if budget < 60 {
		budget = 60
	}
	return budget
}

// switchComponentRouting re-renders the stack's routing with ONE component
// pointed at an explicit endpoint (the candidate IP during its switch, the
// stable container name after), applied atomically and verified.
func (r *deploymentRun) switchComponentRouting(ctx context.Context, appUUID, component, endpoint string) error {
	if r.server.ProxyType != store.ProxyTypeTraefik {
		return nil
	}
	// Scale-to-zero app (ADR-037): routing goes through the waker, so there is no
	// per-component rolling switch. Candidate steps are skipped; the stable step
	// re-applies the waker routing.
	if r.app.Application.ScaleToZero {
		if endpoint != "" {
			return nil
		}
		return r.applyRouting(ctx, appUUID)
	}
	overrides := map[string]string{}
	expect := ""
	name := "switch_routing_" + component
	if endpoint != "" {
		overrides[component] = endpoint
		expect = "http://" + endpoint + ":"
	} else {
		name = "stabilize_routing_" + component
	}
	content, err := RenderRoutingFileWithComponentEndpoints(ctx, r.h.Store, r.app, r.d.ID, "", overrides)
	if err != nil {
		return err
	}
	applier := &ProxyApplier{Store: r.h.Store, Client: r.client, Server: r.server, Network: r.dest.Network}
	return r.step(ctx, name, func() (*sshexec.Result, error) {
		return nil, applier.Apply(ctx, appUUID, content, expect)
	})
}

// composeCreateOpts names the container a create command produces: the
// final name for a recreate, the -next candidate for a zero-downtime switch.
type composeCreateOpts struct {
	Name    string
	Aliases []string
	// ReplaceOld prepends the stop+rm of the previous container (§7.4).
	ReplaceOld bool
}

// composeCreateCommand renders the docker create for one service — every
// value that could be hostile is shell-quoted, every secret travels by
// environment sourcing, never argv (INV-003, INV-012). The container is NOT
// started: callers connect its networks first, then start it.
func (r *deploymentRun) composeCreateCommand(plan *compose.Plan, sp compose.ServicePlan, appDir, labels, envPath string, envKeys []string, runRef string, opts composeCreateOpts) string {
	svc := sp.Service
	var flags strings.Builder

	fmt.Fprintf(&flags, " --restart %s", sp.Restart)
	if sp.Restart == "" {
		flags.Reset()
		flags.WriteString(" --restart no") // raw mode without a policy: compose default
	}
	fmt.Fprintf(&flags, " --network %s", plan.NetworkName)
	for _, alias := range opts.Aliases {
		fmt.Fprintf(&flags, " --network-alias %s", alias)
	}
	fmt.Fprintf(&flags, " %s --label akerdock.component=%s", labels, sp.Name)
	if sp.OneShot {
		// The lifecycle job must not re-run one-shot jobs on start/restart.
		flags.WriteString(" --label akerdock.oneshot=true")
	}
	userLabels := make([]string, 0, len(svc.Labels))
	for key, value := range svc.Labels {
		userLabels = append(userLabels, fmt.Sprintf(" --label %s", shellQuote(key+"="+value)))
	}
	sort.Strings(userLabels)
	flags.WriteString(strings.Join(userLabels, ""))

	for _, mount := range sp.Mounts {
		switch mount.Type {
		case "volume":
			suffix := ""
			if mount.ReadOnly {
				suffix = ":ro"
			}
			fmt.Fprintf(&flags, " -v %s", shellQuote(mount.Source+":"+mount.Target+suffix))
		case "bind":
			source := mount.Source
			if !strings.HasPrefix(source, "/") {
				// Relative binds resolve inside the clone (§2.4).
				source = appDir + "/mounts/" + strings.TrimPrefix(source, "./")
			}
			suffix := ""
			if mount.ReadOnly {
				suffix = ":ro"
			}
			fmt.Fprintf(&flags, " -v %s", shellQuote(source+":"+mount.Target+suffix))
		case "tmpfs":
			fmt.Fprintf(&flags, " --tmpfs %s", shellQuote(mount.Target))
		}
	}

	for _, port := range svc.Ports {
		spec := port.Published + ":" + fmt.Sprint(port.Target)
		if port.HostIP != "" {
			spec = port.HostIP + ":" + spec
		}
		if port.Protocol != "" && port.Protocol != "tcp" {
			spec += "/" + port.Protocol
		}
		fmt.Fprintf(&flags, " -p %s", shellQuote(spec))
	}

	limits := sp.Limits
	if limits.Memory > 0 {
		fmt.Fprintf(&flags, " --memory %d", limits.Memory)
	}
	if limits.MemoryReservation > 0 {
		fmt.Fprintf(&flags, " --memory-reservation %d", limits.MemoryReservation)
	}
	if limits.MemorySwap > 0 {
		fmt.Fprintf(&flags, " --memory-swap %d", limits.MemorySwap)
	}
	if limits.CPUs > 0 {
		fmt.Fprintf(&flags, " --cpus %g", limits.CPUs)
	}
	if limits.CPUShares > 0 {
		fmt.Fprintf(&flags, " --cpu-shares %d", limits.CPUShares)
	}
	if limits.CPUSet != "" {
		fmt.Fprintf(&flags, " --cpuset-cpus %s", shellQuote(limits.CPUSet))
	}
	if limits.Pids > 0 {
		fmt.Fprintf(&flags, " --pids-limit %d", limits.Pids)
	}

	if health := sp.Health; health != nil {
		switch {
		case health.Disable || (len(health.Test) > 0 && health.Test[0] == "NONE"):
			flags.WriteString(" --no-healthcheck")
		case len(health.Test) > 1:
			cmd := strings.Join(health.Test[1:], " ")
			fmt.Fprintf(&flags, " --health-cmd %s --health-interval %s --health-timeout %s --health-retries %d --health-start-period %s",
				shellQuote(cmd), health.Interval, health.Timeout, health.Retries, health.StartPeriod)
		}
	}

	if svc.User != "" {
		fmt.Fprintf(&flags, " --user %s", shellQuote(svc.User))
	}
	if svc.WorkingDir != "" {
		fmt.Fprintf(&flags, " --workdir %s", shellQuote(svc.WorkingDir))
	}
	if svc.Init != nil && *svc.Init {
		flags.WriteString(" --init")
	}
	if svc.ReadOnly {
		flags.WriteString(" --read-only")
	}
	for _, host := range svc.ExtraHosts.AsList(":") {
		fmt.Fprintf(&flags, " --add-host %s", shellQuote(host))
	}
	if svc.StopGracePeriod != nil {
		fmt.Fprintf(&flags, " --stop-timeout %d", int(time.Duration(*svc.StopGracePeriod).Seconds()))
	}
	if svc.StopSignal != "" {
		fmt.Fprintf(&flags, " --stop-signal %s", shellQuote(svc.StopSignal))
	}

	command := ""
	if len(svc.Entrypoint) > 0 {
		fmt.Fprintf(&flags, " --entrypoint %s", shellQuote(svc.Entrypoint[0]))
		for _, arg := range svc.Entrypoint[1:] {
			command += " " + shellQuote(arg)
		}
	}
	for _, arg := range svc.Command {
		command += " " + shellQuote(arg)
	}

	replace := ""
	if opts.ReplaceOld {
		replace = fmt.Sprintf("docker stop -t 30 %s >/dev/null 2>&1; docker rm -f %s >/dev/null 2>&1; ", opts.Name, opts.Name)
	}
	return fmt.Sprintf(
		". %s/env/runtime.sh; . %s; %sdocker create --name %s%s%s %s%s >/dev/null",
		appDir, envPath, replace, opts.Name, flags.String(), envFlags(envKeys), runRef, command)
}

// syncStackStorages mirrors the stack's named volumes into
// persistent_storages (compose-spec §2.4): the Storages tab shows what the
// deployment actually creates, and the preview page derives its per-PR
// names from the same rows. Mirrored rows are is_generated and rewritten
// wholesale — the compose FILE is the source of truth, never these rows.
// Production only: a preview must not rewrite the application's records
// (INV-010), and its volumes derive from the same declared names anyway.
func (r *deploymentRun) syncStackStorages(ctx context.Context, plan *compose.Plan) error {
	if err := r.h.Store.DeleteGeneratedStoragesForResource(ctx, r.app.Resource.ID); err != nil {
		return err
	}
	// First mount wins for display: a volume mounted by several services has
	// one canonical row, not one per consumer.
	type volumeRow struct {
		declared, mountPath string
		external            *string
	}
	dockerToDeclared := map[string]string{}
	for declared, dockerName := range plan.Volumes {
		dockerToDeclared[dockerName] = declared
	}
	externals := map[string]string{}
	for declared, dockerName := range plan.ExternalVolumes {
		dockerToDeclared[dockerName] = declared
		externals[declared] = dockerName
	}
	seen := map[string]bool{}
	var rows []volumeRow
	for _, sp := range plan.Services {
		for _, m := range sp.Mounts {
			if m.Type != "volume" {
				continue
			}
			declared, ok := dockerToDeclared[m.Source]
			if !ok || seen[declared] {
				continue
			}
			seen[declared] = true
			row := volumeRow{declared: declared, mountPath: m.Target}
			if external, isExternal := externals[declared]; isExternal {
				row.external = &external
			}
			rows = append(rows, row)
		}
	}
	for _, row := range rows {
		u, err := pguuid.New()
		if err != nil {
			return err
		}
		if err := r.h.Store.CreateGeneratedStorage(ctx, store.CreateGeneratedStorageParams{
			Uuid: u, ResourceID: r.app.Resource.ID, Name: &row.declared,
			MountPath: row.mountPath, ExternalName: row.external,
		}); err != nil {
			return err
		}
	}
	return nil
}
