package box

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	// DefaultKeep is the number of earlier images retained per service when no limit is configured.
	DefaultKeep = 3
	maxKeep     = 100
)

// Kept identifies an earlier service image retained for rollback.
type Kept struct {
	// ID is the full Docker image ID, including its algorithm prefix.
	ID string `json:"id"`
	// DeployedAt is when this image most recently became current. It is zero when that time was not
	// recorded.
	DeployedAt time.Time `json:"deployed_at,omitzero"`
	// Legacy means this image still uses the pre-history <service>-previous tag. Keeping this
	// marker in the record lets rollback and trimming manage that tag after an upgrade.
	Legacy bool `json:"legacy,omitempty"`
}

// Limits contains the effective retention limits for one service. The current image is always
// retained.
type Limits struct {
	// Keep limits the number of earlier images, excluding the current and rejected images.
	Keep int
	// Budget limits the estimated bytes used by the current image and its history. Zero disables
	// this limit.
	Budget int64
}

var sizePattern = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*([a-zA-Z]*)$`)

var sizeUnits = map[string]int64{
	"": 1, "b": 1,
	"k": 1e3, "kb": 1e3, "m": 1e6, "mb": 1e6, "g": 1e9, "gb": 1e9, "t": 1e12, "tb": 1e12,
	"ki": 1 << 10, "kib": 1 << 10, "mi": 1 << 20, "mib": 1 << 20, "gi": 1 << 30, "gib": 1 << 30, "ti": 1 << 40, "tib": 1 << 40,
}

// ParseSize parses a nonnegative size in bytes. Units are case-insensitive: KB through TB use
// powers of 1000, and KiB through TiB use powers of 1024. The trailing B is optional, and a
// number without a unit means bytes. Fractional bytes are discarded. Sizes above MaxInt64
// bytes are rejected before conversion.
func ParseSize(s string) (int64, error) {
	m := sizePattern.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("size %q: want a number and a unit, like 500MB or 2GB", s)
	}

	unit, ok := sizeUnits[strings.ToLower(m[2])]
	if !ok {
		return 0, fmt.Errorf("size %q: unknown unit %q", s, m[2])
	}

	n, ok := new(big.Rat).SetString(m[1])
	if !ok {
		return 0, fmt.Errorf("size %q: invalid number", s)
	}

	n.Mul(n, new(big.Rat).SetInt64(unit))
	if n.Cmp(new(big.Rat).SetInt64(math.MaxInt64)) > 0 {
		return 0, fmt.Errorf("size %q: exceeds %d bytes", s, int64(math.MaxInt64))
	}

	return new(big.Int).Quo(n.Num(), n.Denom()).Int64(), nil
}

// FormatSize formats bytes with decimal units, using one decimal place for kB and larger units.
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

// Limits resolves service overrides against server settings. Negative service values inherit the
// corresponding server setting. Invalid server settings fall back to DefaultKeep and no byte
// limit.
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

func retainedRef(site, service string, k Kept) string {
	if k.Legacy {
		return keepRef(site, service) + "-previous"
	}

	return histRef(site, service, k.ID)
}

// imageSizes reads exact byte counts from image inspection. Unlike system df's unique sizes,
// these counts include layers shared with any other image on the daemon.
func imageSizes(ctx context.Context) (map[string]int64, error) {
	out, err := docker(ctx, "image", "ls", "--all", "--quiet", "--no-trunc")
	if err != nil {
		return nil, err
	}

	ids := strings.Fields(out)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	sizes := map[string]int64{}
	for batch := range slices.Chunk(ids, 100) {
		out, err := docker(ctx, append([]string{"image", "inspect", "--format", "{{.Id}} {{.Size}}"}, batch...)...)
		if err != nil {
			return nil, err
		}

		fields := strings.Fields(out)
		if len(fields) != 2*len(batch) {
			return nil, fmt.Errorf("reading image sizes: incomplete inspection output")
		}

		for i := 0; i < len(fields); i += 2 {
			n, err := strconv.ParseInt(fields[i+1], 10, 64)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("reading image size for %s: invalid byte count %q", fields[i], fields[i+1])
			}

			sizes[fields[i]] = n
		}
	}

	return sizes, nil
}

// keptBytes counts every distinct retained image in full. This deliberately counts shared
// layers more than once rather than omit layers shared only among earlier images.
func keptBytes(current string, hist []Kept, sizes map[string]int64) int64 {
	total := sizes[current]
	seen := map[string]bool{current: true}
	for _, k := range hist {
		if !seen[k.ID] {
			total = addBytes(total, sizes[k.ID])
			seen[k.ID] = true
		}
	}

	return total
}

// overLimits expects history in newest-first order and removes the oldest entries first. It
// leaves the current image in place even when that image alone exceeds the budget.
func overLimits(current string, hist []Kept, sizes map[string]int64, lim Limits) (kept, dropped []Kept) {
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

func trim(ctx context.Context, cfg Config, site string, s Service, rec *Record, sizes map[string]int64) {
	kept, dropped := overLimits(rec.Images[s.Name], rec.History[s.Name], sizes, cfg.Limits(s))
	for _, k := range dropped {
		docker(ctx, "image", "rm", retainedRef(site, s.Name, k))
	}

	setHistory(rec, s.Name, kept)
}

// trimSite removes excess history tags and updates rec in memory. If image sizes cannot be read,
// it still enforces the count limit. The caller must hold the site lock and save the record.
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

// TrimAll applies each service's retention limits and saves the updated records. It locks one
// site at a time and stops at the first site or record error. It does not run Docker image
// prune.
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

// pickTarget treats an empty target as index 1 and integer targets as one-based history indices.
// Other targets must uniquely match an image ID, ignoring an optional sha256: prefix. ID
// prefixes must have at least four characters. Ambiguity is an error, not a missing match.
func pickTarget(hist []Kept, to string) (Kept, bool, error) {
	if to == "" {
		to = "1"
	}

	if n, err := strconv.Atoi(to); err == nil {
		if n >= 1 && n <= len(hist) {
			return hist[n-1], true, nil
		}

		return Kept{}, false, nil
	}

	prefix := strings.TrimPrefix(strings.ToLower(to), "sha256:")
	if len(prefix) < 4 {
		return Kept{}, false, nil
	}

	var match Kept
	for _, k := range hist {
		if strings.HasPrefix(strings.TrimPrefix(k.ID, "sha256:"), prefix) {
			if match.ID != "" && match.ID != k.ID {
				return Kept{}, false, fmt.Errorf("image prefix %q is ambiguous; use a longer image ID", to)
			}

			match = k
		}
	}

	return match, match.ID != "", nil
}

// Usage estimates retained image storage for one service. All sizes are in bytes.
type Usage struct {
	Site, Service string
	// Kept includes the recorded current image, history, and rejected image. Worst estimates usage
	// at the configured limits with room for one incoming image.
	Kept, Worst int64
	// Running is the full size of the recorded current image, regardless of container state.
	Running int64
	// Budget is the effective byte limit. Zero disables it.
	Budget int64
}

// Capacity combines service storage estimates and compares them with available space. Sizes are
// in bytes. Totals may count images or layers shared by services more than once.
type Capacity struct {
	// Kept and Worst sum the corresponding service estimates. Free is the available space on
	// the configured image-storage filesystem, or -1 when it cannot be measured.
	Kept, Worst, Free int64
	Usage             []Usage
	Warnings          []string
}

// serviceWorst reserves room for an incoming image as large as the current image. Without a
// budget, it assumes every retained image is also that size. This is a planning estimate, not an
// upper bound on future usage.
func serviceWorst(running int64, lim Limits) int64 {
	if lim.Budget > 0 {
		return addBytes(max(lim.Budget, running), running)
	}

	if running > math.MaxInt64/int64(lim.Keep+2) {
		return math.MaxInt64
	}

	return int64(lim.Keep+2) * running
}

// addBytes saturates nonnegative storage totals so a large valid budget cannot wrap a capacity
// estimate into a negative value.
func addBytes(a, b int64) int64 {
	if b > math.MaxInt64-a {
		return math.MaxInt64
	}

	return a + b
}

func assess(usage []Usage, free int64) Capacity {
	c := Capacity{Free: free, Usage: usage}
	for _, u := range usage {
		c.Kept = addBytes(c.Kept, u.Kept)
		c.Worst = addBytes(c.Worst, u.Worst)
		if u.Budget > 0 && u.Running > u.Budget {
			c.Warnings = append(c.Warnings, fmt.Sprintf("%s/%s: its budget of %s is smaller than its running image (%s), so it keeps no history",
				u.Site, u.Service, FormatSize(u.Budget), FormatSize(u.Running)))
		}
	}

	if available := addBytes(max(0, c.Free), c.Kept); c.Free >= 0 && c.Worst > available {
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

// imageFreeSpace uses an explicitly selected image-storage filesystem. Root contains site
// files and is not a fallback: it may be on an unrelated disk or a Docker Desktop host share.
func imageFreeSpace() (int64, error) {
	path := os.Getenv("BOX_IMAGE_STORE")
	if path == "" {
		return -1, fmt.Errorf("image storage is not mounted; rerun box setup with --image-store pointing to the daemon's image storage directory")
	}

	free, err := freeSpace(path)
	if err != nil {
		return -1, err
	}

	return free, nil
}

// Measure estimates storage from saved image records and Docker's reported sizes. It skips
// unreadable site definitions and does not lock sites, so results may overlap deployments. Free
// space is measured only on the configured image-storage filesystem. An unavailable measurement
// produces a warning without discarding image sizes or budget warnings.
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
			hist := slices.Clone(rec.History[s.Name])
			if id := rec.Rejected[s.Name]; id != "" {
				hist = append(hist, Kept{ID: id})
			}

			kept := keptBytes(current, hist, sizes)
			running := sizes[current]
			usage = append(usage, Usage{
				Site: name, Service: s.Name, Kept: kept, Running: running, Budget: lim.Budget,
				Worst: max(serviceWorst(running, lim), kept),
			})
		}
	}

	free, err := imageFreeSpace()
	c := assess(usage, free)
	if err != nil {
		c.Warnings = append(c.Warnings, "couldn't check free space for images: "+err.Error())
	}

	return c, nil
}

// Warnings returns capacity warnings or a message explaining why capacity could not be measured.
func Warnings(ctx context.Context, cfg Config) []string {
	c, err := Measure(ctx, cfg)
	if err != nil {
		return []string{"couldn't check space for kept images: " + err.Error()}
	}

	return c.Warnings
}
