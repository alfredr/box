package box

import (
	"context"
	"log/slog"
	"os"
	"strings"
)

func rejectRef(site, service string) string { return keepRef(site, service) + "-rejected" }

func hold(ctx context.Context, site string, s Service, id string) {
	ref := rejectRef(site, s.Name)
	docker(ctx, "image", "rm", ref)
	if _, err := docker(ctx, "tag", id, ref); err != nil {
		if _, err := docker(ctx, "tag", s.Image, ref); err != nil {
			slog.Warn("keeping the rejected image", "site", site, "service", s.Name, "err", err)
		}
	}
}

func release(ctx context.Context, site, service string) {
	docker(ctx, "image", "rm", rejectRef(site, service))
}

func forget(ctx context.Context, site Site) {
	out, _ := docker(ctx, "images", "--filter", "reference=box-keep/"+site.Name+":*", "--format", "{{.Repository}}:{{.Tag}}")
	refs := strings.Fields(out)
	for _, s := range site.Services {
		refs = append(refs, s.Image)
	}

	for _, ref := range refs {
		docker(ctx, "image", "rm", ref)
	}
}

func pruneEnabled() bool { return os.Getenv("BOX_PRUNE") != "off" }

func Prune(ctx context.Context) (string, error) {
	out, err := docker(ctx, "image", "prune", "--force")
	if err != nil {
		return "", err
	}

	return reclaimedFrom(out), nil
}

func reclaimedFrom(out string) string {
	for line := range strings.Lines(out) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "Total reclaimed space:"); ok {
			return strings.TrimSpace(v)
		}
	}

	return "0B"
}

func tidy(ctx context.Context) {
	if !pruneEnabled() {
		return
	}

	reclaimed, err := Prune(ctx)
	switch {
	case err != nil:
		slog.Warn("pruning images", "err", err)
	case reclaimed != "0B":
		slog.Info("pruned images", "reclaimed", reclaimed)
	}
}

func dropStaleProxies(ctx context.Context) {
	repo, _, _ := strings.Cut(ProxyImage, ":")
	out, err := docker(ctx, "images", "--filter", "reference="+repo, "--format", "{{.Repository}}:{{.Tag}}")
	if err != nil {
		return
	}

	for _, ref := range strings.Fields(out) {
		if ref != ProxyImage {
			docker(ctx, "image", "rm", ref)
		}
	}
}
