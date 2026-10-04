package box

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// waitTimeout is the number of seconds passed to Compose for startup and health checks.
const waitTimeout = "180"

// Change records the image IDs before and after an operation on one service.
type Change struct {
	Service string
	// From is empty when no container existed before the operation.
	From, To string
}

// String formats the service name and abbreviated image IDs for logs and CLI output.
func (c Change) String() string {
	if c.From == "" {
		return fmt.Sprintf("%s started %s", c.Service, ShortID(c.To))
	}

	return fmt.Sprintf("%s %s -> %s", c.Service, ShortID(c.From), ShortID(c.To))
}

func changeLines(cs []Change) string {
	lines := make([]string, len(cs))
	for i, c := range cs {
		lines[i] = c.String()
	}

	return strings.Join(lines, "\n")
}

// Deploy starts the selected services, or all services when only is empty. It pulls images for
// services that are not pinned and waits for Compose startup checks. A failed startup triggers
// an attempt to restore the earlier images. The site lock covers deployment and recovery.
func Deploy(ctx context.Context, cfg Config, name string, only []string) ([]Change, error) {
	defer tidy(ctx)

	unlock, err := lockSite(name)
	if err != nil {
		return nil, err
	}

	defer unlock()

	site, err := LoadSite(name)
	if err != nil {
		return nil, err
	}

	svcs, err := site.pick(only)
	if err != nil {
		return nil, err
	}

	return deploy(ctx, cfg, site, svcs, len(only) > 0, true)
}

// deploy requires the site lock. A partial deployment uses --no-deps, while a full deployment
// removes orphaned containers. Setting pull to false skips the explicit pull command.
func deploy(ctx context.Context, cfg Config, site Site, svcs []Service, partial, pull bool) ([]Change, error) {
	rec, err := LoadRecord(site.Name)
	if err != nil {
		return nil, err
	}

	before, _, err := inspectSite(ctx, site.Name)
	if err != nil {
		return nil, err
	}

	if pull {
		var pulls []string
		for _, s := range svcs {
			if s.Policy != Pinned {
				pulls = append(pulls, s.Name)
			}
		}

		if len(pulls) > 0 {
			if _, err := docker(ctx, siteArgs(site.Name, append([]string{"pull", "--quiet"}, pulls...)...)...); err != nil {
				return nil, err
			}
		}
	}

	up := []string{"up", "--detach", "--wait", "--wait-timeout", waitTimeout}
	if partial {
		up = append(up, "--no-deps")
	} else {
		up = append(up, "--remove-orphans")
	}

	_, upErr := docker(ctx, siteArgs(site.Name, append(up, serviceNames(svcs)...)...)...)
	after, _, err := inspectSite(ctx, site.Name)
	if err != nil {
		return nil, errors.Join(upErr, err)
	}

	if upErr != nil {
		for _, s := range svcs {
			if id := after[s.Name]; id != "" && id != before[s.Name] {
				put(&rec.Rejected, s.Name, id)
				hold(ctx, site.Name, s, id)
			}
		}

		msg := upErr.Error()
		if err := restore(ctx, site, svcs, keptSources(ctx, site.Name, svcs, before, keepRef)); err != nil {
			msg += "\nputting the old images back also failed: " + err.Error()
			rec.Result = "failed, and so did putting the old images back"
		} else {
			msg += "\nthe old images are back up"
			rec.Result = "failed, old images back up"
		}

		if err := rec.save(site.Name); err != nil {
			slog.Error("saving record", "site", site.Name, "err", err)
		}

		Notify(cfg, site.Name+": deploy failed", msg, true)
		return nil, errors.New(msg)
	}

	changes := settle(ctx, cfg, site, svcs, before, after, &rec, true)
	if err := rec.save(site.Name); err != nil {
		return changes, err
	}

	if len(changes) > 0 {
		Notify(cfg, site.Name+": deployed", changeLines(changes), false)
	}

	return changes, nil
}

// restore retags each available source as the service image and starts those services without
// dependencies. Services with no source are skipped. The operation fails if no service can be
// restored.
func restore(ctx context.Context, site Site, svcs []Service, sources map[string]string) error {
	var names []string
	for _, s := range svcs {
		src := sources[s.Name]
		if src == "" {
			continue
		}

		if _, err := docker(ctx, "tag", src, s.Image); err != nil {
			return err
		}

		names = append(names, s.Name)
	}

	if len(names) == 0 {
		return errors.New("nothing to go back to")
	}

	up := append([]string{"up", "--detach", "--wait", "--wait-timeout", waitTimeout, "--no-deps"}, names...)
	_, err := docker(ctx, siteArgs(site.Name, up...)...)
	return err
}

// pinRunning resets local image references to the images used by existing containers. This
// prevents a prior check from selecting a pending or rejected image solely by moving the local
// tag.
func pinRunning(ctx context.Context, site string, svcs []Service, running map[string]string) error {
	for src, s := range keptSources(ctx, site, svcs, running, keepRef) {
		i := slices.IndexFunc(svcs, func(svc Service) bool { return svc.Name == src })
		ref, id := svcs[i].Image, running[src]
		if imageID(ctx, ref) == id {
			continue
		}

		if _, err := docker(ctx, "tag", s, ref); err != nil {
			return fmt.Errorf("keeping %s on the image it runs: %w", src, err)
		}
	}

	return nil
}

// Stable tags retain current and earlier images after a pull moves the original tag. These
// names also allow restoration when Docker cannot resolve an untagged image by ID.
func keepRef(site, service string) string { return "box-keep/" + site + ":" + strings.ToLower(service) }

// keptSources uses a retained tag only when it still refers to the requested image ID.
// Otherwise it falls back to the ID, allowing for changes made outside box.
func keptSources(ctx context.Context, site string, svcs []Service, ids map[string]string, ref func(string, string) string) map[string]string {
	sources := map[string]string{}
	for _, s := range svcs {
		id := ids[s.Name]
		if id == "" {
			continue
		}

		sources[s.Name] = id
		if name := ref(site, s.Name); imageID(ctx, name) == id {
			sources[s.Name] = name
		}
	}

	return sources
}

// settle records a successful operation, moves replaced images into history, and applies the
// keep and budget limits. When pulled is true, it also clears pending and rejected records.
func settle(ctx context.Context, cfg Config, site Site, svcs []Service, before, after map[string]string, rec *Record, pulled bool) []Change {
	var changes []Change
	for _, s := range svcs {
		from, to := before[s.Name], after[s.Name]
		if to == "" {
			continue
		}

		put(&rec.Images, s.Name, to)
		if pulled || rec.Pending[s.Name] == to {
			delete(rec.Pending, s.Name)
		}

		if pulled && rec.Rejected[s.Name] != "" {
			release(ctx, site.Name, s.Name)
			delete(rec.Rejected, s.Name)
		}

		if from == to {
			continue
		}

		remember(ctx, site.Name, s, from, to, rec)
		changes = append(changes, Change{Service: s.Name, From: from, To: to})
	}

	trimSite(ctx, cfg, site, rec)
	rec.DeployedAt = time.Now()
	rec.Result = "ok"
	return changes
}

func remember(ctx context.Context, site string, s Service, from, to string, rec *Record) {
	cur := keepRef(site, s.Name)
	wasKept := slices.ContainsFunc(rec.History[s.Name], func(k Kept) bool { return k.ID == to })
	hist := slices.DeleteFunc(slices.Clone(rec.History[s.Name]), func(k Kept) bool { return k.ID == from || k.ID == to })
	if from != "" {
		src := cur
		if imageID(ctx, cur) != from {
			src = from
		}

		if _, err := docker(ctx, "tag", src, histRef(site, s.Name, from)); err != nil {
			slog.Warn("keeping the previous image", "site", site, "service", s.Name, "err", err)
		} else {
			hist = append([]Kept{{ID: from, DeployedAt: rec.Since[s.Name]}}, hist...)
		}
	}

	if _, err := docker(ctx, "tag", to, cur); err != nil {
		if _, err := docker(ctx, "tag", s.Image, cur); err != nil {
			slog.Warn("keeping the current image", "site", site, "service", s.Name, "err", err)
		}
	}

	if wasKept {
		docker(ctx, "image", "rm", histRef(site, s.Name, to))
	}

	setHistory(rec, s.Name, hist)
	if rec.Since == nil {
		rec.Since = map[string]time.Time{}
	}

	rec.Since[s.Name] = time.Now()
}

// Rollback restores services to an image from their history: the most recent one when target
// is empty, the nth when it is a number, or the one whose ID starts with target. The images it
// replaces join the history and are recorded as rejected so checks skip them. It does not
// restore Compose configuration or volume data.
func Rollback(ctx context.Context, cfg Config, name, target string) ([]Change, error) {
	defer tidy(ctx)

	unlock, err := lockSite(name)
	if err != nil {
		return nil, err
	}

	defer unlock()

	site, err := LoadSite(name)
	if err != nil {
		return nil, err
	}

	rec, err := LoadRecord(name)
	if err != nil {
		return nil, err
	}

	before, _, err := inspectSite(ctx, name)
	if err != nil {
		return nil, err
	}

	var svcs []Service
	sources := map[string]string{}
	for _, s := range site.Services {
		k, ok := pickTarget(rec.History[s.Name], target)
		if !ok || k.ID == before[s.Name] {
			continue
		}

		svcs = append(svcs, s)
		sources[s.Name] = k.ID
		if ref := histRef(name, s.Name, k.ID); imageID(ctx, ref) == k.ID {
			sources[s.Name] = ref
		}
	}

	if len(svcs) == 0 {
		if target == "" {
			return nil, Invalid(fmt.Errorf("%s has no earlier images to go back to", name))
		}

		return nil, Invalid(fmt.Errorf("%s has no kept image matching %q (box status %s lists them)", name, target, name))
	}

	if err := restore(ctx, site, svcs, sources); err != nil {
		rec.Result = "rollback failed"
		if err := rec.save(name); err != nil {
			slog.Error("saving record", "site", name, "err", err)
		}

		Notify(cfg, name+": rollback failed", err.Error(), true)
		return nil, err
	}

	after, _, err := inspectSite(ctx, name)
	if err != nil {
		return nil, err
	}

	var changes []Change
	for _, s := range svcs {
		from, to := before[s.Name], after[s.Name]
		put(&rec.Images, s.Name, to)
		delete(rec.Pending, s.Name)

		if from != "" && from != to {
			release(ctx, name, s.Name)
			put(&rec.Rejected, s.Name, from)
			remember(ctx, name, s, from, to, &rec)
			changes = append(changes, Change{Service: s.Name, From: from, To: to})
		}
	}

	trimSite(ctx, cfg, site, &rec)

	rec.Result = "rolled back"
	rec.DeployedAt = time.Now()
	if err := rec.save(name); err != nil {
		return changes, err
	}

	Notify(cfg, name+": rolled back", changeLines(changes), false)
	return changes, nil
}

// CheckResult reports changes and any failure from one site image check.
type CheckResult struct {
	Site string
	// Deployed contains image changes applied to automatic services during this check.
	Deployed []Change
	// Waiting contains newly discovered manual updates. Previously reported pending images are
	// omitted.
	Waiting []Change
	Err     error
}

// Check pulls images for all non-pinned services and applies their update policies. A site that
// has never been deployed starts once its auto services have images. Otherwise it skips
// deployment for services without containers and for rejected images. New manual updates are
// recorded and announced once. The site lock is held for the entire check.
func Check(ctx context.Context, cfg Config, name string) CheckResult {
	r := CheckResult{Site: name}
	defer func() {
		if len(r.Deployed)+len(r.Waiting) > 0 {
			tidy(ctx)
		}
	}()

	unlock, err := lockSite(name)
	if err != nil {
		r.Err = err
		return r
	}

	defer unlock()

	site, err := LoadSite(name)
	if err != nil {
		r.Err = err
		return r
	}

	var pulls []Service
	for _, s := range site.Services {
		if s.Policy != Pinned {
			pulls = append(pulls, s)
		}
	}

	if len(pulls) == 0 {
		return r
	}

	rec, err := LoadRecord(name)
	if err != nil {
		r.Err = err
		return r
	}

	before, _, err := inspectSite(ctx, name)
	if err != nil {
		r.Err = err
		return r
	}

	rec.CheckedAt = time.Now()
	_, err = docker(ctx, siteArgs(name, append([]string{"pull", "--quiet"}, serviceNames(pulls)...)...)...)
	if err != nil {
		// Report only the first failure in a sequence of failed checks to avoid repeated outage
		// notifications.
		if rec.CheckError == "" {
			Notify(cfg, name+": check failing", err.Error(), true)
		}

		rec.CheckError = err.Error()
		r.Err = errors.Join(err, rec.save(name))
		return r
	}

	rec.CheckError = ""

	// A site that has never been deployed starts once its auto services have images, so CI can bring up a new site
	// with its first push. A site that was deployed and later stopped is left alone.
	if rec.DeployedAt.IsZero() && len(before) == 0 {
		if err := rec.save(name); err != nil {
			r.Err = err
			return r
		}

		for _, s := range pulls {
			if s.Policy == Auto && imageID(ctx, s.Image) != "" {
				r.Deployed, r.Err = deploy(ctx, cfg, site, site.Services, false, false)
				return r
			}
		}

		return r
	}

	var auto []Service
	for _, s := range pulls {
		id := imageID(ctx, s.Image)
		running := before[s.Name]
		if id == "" || running == "" || id == running || id == rec.Rejected[s.Name] {
			continue
		}

		if s.Policy == Auto {
			auto = append(auto, s)
			continue
		}

		if rec.Pending[s.Name] != id {
			put(&rec.Pending, s.Name, id)
			r.Waiting = append(r.Waiting, Change{Service: s.Name, From: running, To: id})
		}
	}

	if err := rec.save(name); err != nil {
		r.Err = err
		return r
	}

	if len(r.Waiting) > 0 {
		Notify(cfg, name+": update waiting", changeLines(r.Waiting)+"\nbox deploy "+name+" starts it", false)
	}

	if len(auto) > 0 {
		r.Deployed, r.Err = deploy(ctx, cfg, site, auto, true, false)
	}

	return r
}

// CheckAll checks sites sequentially in name order. Per-site failures are returned in the
// results. The error return is reserved for a failure to list sites.
func CheckAll(ctx context.Context, cfg Config) ([]CheckResult, error) {
	names, err := ListSites()
	if err != nil {
		return nil, err
	}

	results := make([]CheckResult, 0, len(names))
	for _, n := range names {
		results = append(results, Check(ctx, cfg, n))
	}

	return results, nil
}

// CheckCompose validates data with both the box parser and Docker Compose. It writes a
// temporary file beside the site file so relative paths resolve consistently, then removes it.
// The caller must hold the site lock.
func CheckCompose(ctx context.Context, name string, data []byte) error {
	if _, err := ParseSite(name, data); err != nil {
		return err
	}

	draft := filepath.Join(siteDir(name), ".compose.check.yml")
	if err := os.WriteFile(draft, data, 0o600); err != nil {
		return err
	}

	defer os.Remove(draft)

	if _, err := docker(ctx, composeArgs(name, draft, "config", "--quiet")...); err != nil {
		return Invalid(err)
	}

	return nil
}

// ReadCompose validates the site name and existing definition, then returns the stored Compose
// file bytes.
func ReadCompose(name string) ([]byte, error) {
	if _, err := LoadSite(name); err != nil {
		return nil, err
	}

	return os.ReadFile(ComposePath(name))
}

// Apply validates and replaces a site Compose file, then applies it with Docker Compose.
// Existing services with unchanged image references are retagged to their current images first.
// If startup fails, it attempts to restore and apply the old file. The site lock covers the
// entire operation.
func Apply(ctx context.Context, cfg Config, name string, compose []byte) error {
	defer tidy(ctx)

	unlock, err := lockSite(name)
	if err != nil {
		return err
	}

	defer unlock()

	current, err := LoadSite(name)
	if err != nil {
		return err
	}

	if err := CheckCompose(ctx, name, compose); err != nil {
		return err
	}

	site, err := ParseSite(name, compose)
	if err != nil {
		return err
	}

	old, err := os.ReadFile(ComposePath(name))
	if err != nil {
		return err
	}

	rec, err := LoadRecord(name)
	if err != nil {
		return err
	}

	before, _, err := inspectSite(ctx, name)
	if err != nil {
		return err
	}

	// A check may have moved a local tag to a pending or rejected image. Restore tags for
	// unchanged image references before Compose can recreate those services.
	var unchanged []Service
	for _, s := range site.Services {
		if i := slices.IndexFunc(current.Services, func(c Service) bool { return c.Name == s.Name }); i >= 0 && current.Services[i].Image == s.Image {
			unchanged = append(unchanged, s)
		}
	}

	if err := pinRunning(ctx, name, unchanged, before); err != nil {
		return err
	}

	if err := writeFileAtomic(ComposePath(name), compose, 0o644); err != nil {
		return err
	}

	up := []string{"up", "--detach", "--wait", "--wait-timeout", waitTimeout, "--remove-orphans"}
	if _, upErr := docker(ctx, siteArgs(name, up...)...); upErr != nil {
		msg := upErr.Error()
		if err := writeFileAtomic(ComposePath(name), old, 0o644); err != nil {
			msg += "\nputting the old compose file back failed: " + err.Error()
		} else if _, err := docker(ctx, siteArgs(name, up...)...); err != nil {
			msg += "\nthe old compose file is back, but it didn't come up either: " + err.Error()
		} else {
			msg += "\nthe old compose file is back and up"
		}

		Notify(cfg, name+": edit failed", msg, true)
		return errors.New(msg)
	}

	after, _, err := inspectSite(ctx, name)
	if err != nil {
		return err
	}

	settle(ctx, cfg, site, site.Services, before, after, &rec, false)
	return rec.save(name)
}

// Remove stops a site and removes its secret and deployment record. With purge set, it also
// deletes Compose-managed volumes and the site directory. Otherwise it preserves volumes, moves
// the directory under removed/, and returns that path.
func Remove(ctx context.Context, name string, purge bool) (string, error) {
	defer tidy(ctx)

	unlock, err := lockSite(name)
	if err != nil {
		return "", err
	}

	defer unlock()

	site, err := LoadSite(name)
	if err != nil {
		return "", err
	}

	down := []string{"down", "--remove-orphans"}
	if purge {
		down = append(down, "--volumes")
	}

	if _, err := docker(ctx, siteArgs(name, down...)...); err != nil {
		return "", err
	}

	forget(ctx, site)
	os.Remove(secretPath(name))
	os.Remove(recordPath(name))

	if purge {
		return "", os.RemoveAll(siteDir(name))
	}

	dest := filepath.Join(removedDir(), name+"-"+time.Now().Format("20060102-150405"))
	if err := os.MkdirAll(removedDir(), 0o755); err != nil {
		return "", err
	}

	return dest, os.Rename(siteDir(name), dest)
}
