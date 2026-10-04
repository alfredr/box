package box

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	DefaultKeep = 3
	maxKeep     = 100
)

type Kept struct {
	ID         string    `json:"id"`
	DeployedAt time.Time `json:"deployed_at,omitzero"`
}

type Limits struct {
	Keep   int
	Budget int64
}

var sizePattern = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*([a-zA-Z]*)$`)

var sizeUnits = map[string]float64{
	"": 1, "b": 1,
	"k": 1e3, "kb": 1e3, "m": 1e6, "mb": 1e6, "g": 1e9, "gb": 1e9, "t": 1e12, "tb": 1e12,
	"ki": 1 << 10, "kib": 1 << 10, "mi": 1 << 20, "mib": 1 << 20, "gi": 1 << 30, "gib": 1 << 30, "ti": 1 << 40, "tib": 1 << 40,
}

func ParseSize(s string) (int64, error) {
	m := sizePattern.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("size %q: want a number and a unit, like 500MB or 2GB", s)
	}

	unit, ok := sizeUnits[strings.ToLower(m[2])]
	if !ok {
		return 0, fmt.Errorf("size %q: unknown unit %q", s, m[2])
	}

	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, fmt.Errorf("size %q: %w", s, err)
	}

	return int64(n * unit), nil
}

func FormatSize(n int64) string {
	units := []string{"B", "kB", "MB", "GB", "TB"}
	f, i := float64(n), 0
	for f >= 1000 && i < len(units)-1 {
		f /= 1000
		i++
	}

	if i == 0 {
		return fmt.Sprintf("%dB", n)
	}

	return strconv.FormatFloat(f, 'f', 1, 64) + units[i]
}

func parseKeep(v string) (int, error) {
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n > maxKeep {
		return 0, fmt.Errorf("keep: want a whole number from 0 to %d, got %q", maxKeep, v)
	}

	return n, nil
}

func parseBudget(v string) (int64, error) {
	if v == "" || v == "off" {
		return 0, nil
	}

	return ParseSize(v)
}

func (c Config) Limits(s Service) Limits {
	lim := Limits{Keep: DefaultKeep}
	if n, err := parseKeep(c.Keep); err == nil {
		lim.Keep = n
	}

	if b, err := parseBudget(c.Budget); err == nil {
		lim.Budget = b
	}

	if s.Keep >= 0 {
		lim.Keep = s.Keep
	}

	if s.Budget >= 0 {
		lim.Budget = s.Budget
	}

	return lim
}

func histRef(site, service, id string) string {
	return keepRef(site, service) + "-" + ShortID(id)
}

type imageSize struct {
	Full, Unique int64
}

func imageSizes(ctx context.Context) (map[string]imageSize, error) {
	out, err := docker(ctx, "system", "df", "--verbose", "--format", "{{json .Images}}")
	if err != nil {
		return nil, err
	}

	var rows []struct {
		ID, Size, UniqueSize string
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rows); err != nil {
		return nil, fmt.Errorf("reading image sizes: %w", err)
	}

	sizes := map[string]imageSize{}
	for _, r := range rows {
		full, _ := ParseSize(r.Size)
		unique, _ := ParseSize(r.UniqueSize)
		sizes[r.ID] = imageSize{Full: full, Unique: unique}
	}

	return sizes, nil
}

func keptBytes(current string, hist []Kept, sizes map[string]imageSize) int64 {
	total := sizes[current].Full
	for _, k := range hist {
		total += sizes[k.ID].Unique
	}

	return total
}

func overLimits(current string, hist []Kept, sizes map[string]imageSize, lim Limits) (kept, dropped []Kept) {
	kept = slices.Clone(hist)
	for len(kept) > lim.Keep {
		dropped = append(dropped, kept[len(kept)-1])
		kept = kept[:len(kept)-1]
	}

	if lim.Budget <= 0 {
		return kept, dropped
	}

	for len(kept) > 0 && keptBytes(current, kept, sizes) > lim.Budget {
		dropped = append(dropped, kept[len(kept)-1])
		kept = kept[:len(kept)-1]
	}

	return kept, dropped
}

func trim(ctx context.Context, cfg Config, site string, s Service, rec *Record, sizes map[string]imageSize) {
	kept, dropped := overLimits(rec.Images[s.Name], rec.History[s.Name], sizes, cfg.Limits(s))
	for _, k := range dropped {
		docker(ctx, "image", "rm", histRef(site, s.Name, k.ID))
	}

	setHistory(rec, s.Name, kept)
}

func trimSite(ctx context.Context, cfg Config, site Site, rec *Record) {
	sizes, err := imageSizes(ctx)
	if err != nil {
		slog.Warn("reading image sizes", "site", site.Name, "err", err)
	}

	for _, s := range site.Services {
		trim(ctx, cfg, site.Name, s, rec, sizes)
	}
}

func setHistory(rec *Record, service string, hist []Kept) {
	if len(hist) == 0 {
		delete(rec.History, service)
		return
	}

	if rec.History == nil {
		rec.History = map[string][]Kept{}
	}

	rec.History[service] = hist
}

func TrimAll(ctx context.Context, cfg Config) error {
	names, err := ListSites()
	if err != nil {
		return err
	}

	for _, name := range names {
		if err := trimNamed(ctx, cfg, name); err != nil {
			return err
		}
	}

	return nil
}

func trimNamed(ctx context.Context, cfg Config, name string) error {
	unlock, err := lockSite(name)
	if err != nil {
		return err
	}

	defer unlock()

	site, err := LoadSite(name)
	if err != nil {
		return err
	}

	rec, err := LoadRecord(name)
	if err != nil {
		return err
	}

	trimSite(ctx, cfg, site, &rec)
	return rec.save(name)
}

func pickTarget(hist []Kept, to string) (Kept, bool) {
	if to == "" {
		to = "1"
	}

	if n, err := strconv.Atoi(to); err == nil {
		if n >= 1 && n <= len(hist) {
			return hist[n-1], true
		}

		return Kept{}, false
	}

	prefix := strings.TrimPrefix(strings.ToLower(to), "sha256:")
	if len(prefix) < 4 {
		return Kept{}, false
	}

	for _, k := range hist {
		if strings.HasPrefix(strings.TrimPrefix(k.ID, "sha256:"), prefix) {
			return k, true
		}
	}

	return Kept{}, false
}

type Usage struct {
	Site, Service string
	Kept, Worst   int64
	Running       int64
	Budget        int64
}

type Capacity struct {
	Kept, Worst, Free int64
	Usage             []Usage
	Warnings          []string
}

func serviceWorst(running int64, lim Limits) int64 {
	if lim.Budget > 0 {
		return max(lim.Budget, running) + running
	}

	return int64(lim.Keep+2) * running
}

func assess(usage []Usage, free int64) Capacity {
	c := Capacity{Free: free, Usage: usage}
	for _, u := range usage {
		c.Kept += u.Kept
		c.Worst += u.Worst
		if u.Budget > 0 && u.Running > u.Budget {
			c.Warnings = append(c.Warnings, fmt.Sprintf("%s/%s: its budget of %s is smaller than its running image (%s), so it keeps no history",
				u.Site, u.Service, FormatSize(u.Budget), FormatSize(u.Running)))
		}
	}

	if available := c.Free + c.Kept; c.Worst > available {
		largest := slices.Clone(usage)
		sort.Slice(largest, func(i, j int) bool { return largest[i].Worst > largest[j].Worst })
		var top []string
		for _, u := range largest[:min(3, len(largest))] {
			top = append(top, fmt.Sprintf("%s/%s up to %s", u.Site, u.Service, FormatSize(u.Worst)))
		}

		c.Warnings = append(c.Warnings, fmt.Sprintf("kept images could reach %s with the current limits, but only %s is available (%s free plus %s in use); lower keep or set budgets (largest: %s)",
			FormatSize(c.Worst), FormatSize(available), FormatSize(c.Free), FormatSize(c.Kept), strings.Join(top, ", ")))
	}

	return c
}

func freeSpace(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}

	return int64(st.Bavail) * int64(st.Bsize), nil
}

func Measure(ctx context.Context, cfg Config) (Capacity, error) {
	names, err := ListSites()
	if err != nil {
		return Capacity{}, err
	}

	sizes, err := imageSizes(ctx)
	if err != nil {
		return Capacity{}, err
	}

	var usage []Usage
	for _, name := range names {
		site, err := LoadSite(name)
		if err != nil {
			continue
		}

		rec, err := LoadRecord(name)
		if err != nil {
			return Capacity{}, err
		}

		for _, s := range site.Services {
			current := rec.Images[s.Name]
			lim := cfg.Limits(s)
			kept := keptBytes(current, rec.History[s.Name], sizes)
			if id := rec.Rejected[s.Name]; id != "" {
				kept += sizes[id].Unique
			}

			running := sizes[current].Full
			usage = append(usage, Usage{
				Site: name, Service: s.Name, Kept: kept, Running: running, Budget: lim.Budget,
				Worst: max(serviceWorst(running, lim), kept),
			})
		}
	}

	free, err := freeSpace(Root())
	if err != nil {
		return Capacity{}, err
	}

	return assess(usage, free), nil
}

func Warnings(ctx context.Context, cfg Config) []string {
	c, err := Measure(ctx, cfg)
	if err != nil {
		return []string{"couldn't check space for kept images: " + err.Error()}
	}

	return c.Warnings
}
