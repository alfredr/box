package box

import (
	"context"
)

// ServiceStatus combines a Compose service definition with container state and recorded image
// history.
type ServiceStatus struct {
	Service
	// State includes health status when available, such as "running (healthy)". It is "not
	// created" when no container is found.
	State string
	// Running is the image ID used to create the inspected container, even if that container is
	// stopped.
	Running   string
	Pending   string
	History   []Kept
	Limits    Limits
	KeptBytes int64
}

// SiteStatus combines a site definition, its saved record, and inspected service states.
type SiteStatus struct {
	Site
	Record   Record
	Services []ServiceStatus
}

// Status reads a site definition, deployment record, and container state. These reads do not
// hold the site lock and may overlap a deployment.
func Status(ctx context.Context, name string) (SiteStatus, error) {
	site, err := LoadSite(name)
	if err != nil {
		return SiteStatus{}, err
	}

	rec, err := LoadRecord(name)
	if err != nil {
		return SiteStatus{}, err
	}

	images, cs, err := inspectSite(ctx, name)
	if err != nil {
		return SiteStatus{}, err
	}

	cfg, err := LoadConfig()
	if err != nil {
		return SiteStatus{}, err
	}

	sizes, _ := imageSizes(ctx)

	out := SiteStatus{Site: site, Record: rec}
	for _, s := range site.Services {
		ss := ServiceStatus{
			Service:   s,
			State:     "not created",
			Running:   images[s.Name],
			Pending:   rec.Pending[s.Name],
			History:   rec.History[s.Name],
			Limits:    cfg.Limits(s),
			KeptBytes: keptBytes(rec.Images[s.Name], rec.History[s.Name], sizes),
		}
		for _, c := range cs {
			if c.Service == s.Name {
				ss.State = c.State
				if c.Health != "" {
					ss.State += " (" + c.Health + ")"
				}
			}
		}

		out.Services = append(out.Services, ss)
	}

	return out, nil
}
