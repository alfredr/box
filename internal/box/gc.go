package box

import (
	"context"
	"log/slog"
	"os"
	"strings"
)

func rejectRef(site, service string) string { return keepRef(site, service) + "-rejected" }

// hold tags a rejected image before recovery moves the service tag back to its earlier image.
// This keeps image pruning from deleting the rejected image and forcing checks to pull it again.
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

// forget attempts to remove the site's retention tags and the image references in its Compose
// file. Docker removal errors are ignored, and image removal is never forced.
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

// Prune removes unused dangling images across the Docker daemon, including images unrelated to
// box. It returns Docker's formatted reclaimed size and runs even when BOX_PRUNE is off. It does
// not prune volumes or build cache.
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

// tidy runs best-effort image pruning unless BOX_PRUNE is off. Site mutations defer it until
// after releasing the site lock.
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
