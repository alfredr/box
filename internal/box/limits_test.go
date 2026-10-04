package box

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSizeBoundaries(t *testing.T) {
	for input, want := range map[string]int64{
		"9223372036854775807":    math.MaxInt64,
		"9223372036.854775807GB": math.MaxInt64,
		"0.9B":                   0,
		"1.999B":                 1,
		"1.1KiB":                 1126,
	} {
		if got, err := ParseSize(input); err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", input, got, err, want)
		}
	}

	for _, input := range []string{"9223372036854775808", "9223372036854775807.1", "10000000TB", strings.Repeat("9", 400) + "TB"} {
		if _, err := ParseSize(input); err == nil {
			t.Errorf("ParseSize(%q) accepted overflow", input)
		}
		var cfg Config
		if err := cfg.Set("budget", input); !IsInvalid(err) {
			t.Errorf("budget %q: %v", input, err)
		}
	}
}

func TestBudgetIncludesSharedHistoryLayers(t *testing.T) {
	// Both earlier images share a 990 MB base that the current image no longer uses. Counting
	// only their 10 MB unique layers would let both survive a 500 MB budget.
	sizes := map[string]int64{"current": 100_000_000, "old1": 1_000_000_000, "old2": 1_000_000_000}
	hist := []Kept{{ID: "old1"}, {ID: "old2"}}
	kept, dropped := overLimits("current", hist, sizes, Limits{Keep: 3, Budget: 500_000_000})
	if len(kept) != 0 || len(dropped) != 2 {
		t.Fatalf("kept %v, dropped %v", kept, dropped)
	}

	duplicates := []Kept{{ID: "current"}, {ID: "old1"}, {ID: "old1"}}
	if got := keptBytes("current", duplicates, sizes); got != 1_100_000_000 {
		t.Errorf("distinct retained images = %d", got)
	}
}

func TestCapacityDoesNotOverflow(t *testing.T) {
	if got := serviceWorst(1, Limits{Budget: math.MaxInt64}); got != math.MaxInt64 {
		t.Errorf("budget estimate overflowed: %d", got)
	}
	if got := serviceWorst(math.MaxInt64, Limits{Keep: 100}); got != math.MaxInt64 {
		t.Errorf("count estimate overflowed: %d", got)
	}
	c := assess([]Usage{{Kept: math.MaxInt64, Worst: math.MaxInt64}, {Kept: 1, Worst: 1}}, math.MaxInt64)
	if c.Kept != math.MaxInt64 || c.Worst != math.MaxInt64 {
		t.Errorf("capacity overflowed: %+v", c)
	}
}

func TestAmbiguousTarget(t *testing.T) {
	hist := []Kept{{ID: "sha256:abcd1111"}, {ID: "sha256:abcd2222"}, {ID: "sha256:12345678"}}
	for _, target := range []string{"abcd", "sha256:abcd", "ABCD"} {
		if k, ok, err := pickTarget(hist, target); err == nil || ok || k.ID != "" {
			t.Errorf("ambiguous %q selected %+v, %v, %v", target, k, ok, err)
		}
	}
	for target, want := range map[string]string{"abcd1": hist[0].ID, "sha256:1234": hist[2].ID, "2": hist[1].ID} {
		if k, ok, err := pickTarget(hist, target); err != nil || !ok || k.ID != want {
			t.Errorf("target %q = %+v, %v, %v", target, k, ok, err)
		}
	}
}

// fakeDocker isolates command tests from the host daemon and records every attempted command.
func fakeDocker(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "commands")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$BOX_TEST_DOCKER_LOG\"\n" + body
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BOX_TEST_DOCKER_LOG", log)
	t.Setenv("BOX_PRUNE", "off")
	return log
}

func TestRollbackRejectsAmbiguityBeforeMutating(t *testing.T) {
	t.Setenv("BOX_ROOT", t.TempDir())
	log := fakeDocker(t, `case "$1 $2" in
  'image inspect') printf 'sha256:eeee1111\n' ;;
esac
`)
	if err := writeFileAtomic(ComposePath("blog"), []byte("services:\n  app: {image: app:latest}\n  worker: {image: worker:latest}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := Record{History: map[string][]Kept{
		"app":    {{ID: "sha256:abcd0000"}},
		"worker": {{ID: "sha256:abcd1111"}, {ID: "sha256:abcd2222"}},
	}}
	if err := r.save("blog"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(recordPath("blog"))
	if _, err := Rollback(t.Context(), Config{}, "blog", "abcd"); !IsInvalid(err) || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("rollback = %v", err)
	}
	after, _ := os.ReadFile(recordPath("blog"))
	if string(before) != string(after) {
		t.Error("rejected rollback changed the record")
	}
	commands, _ := os.ReadFile(log)
	for _, line := range strings.Split(strings.TrimSpace(string(commands)), "\n") {
		if !strings.Contains(line, " ps ") && !strings.HasPrefix(line, "image inspect ") {
			t.Errorf("rejected rollback attempted a mutation: %s", line)
		}
	}
}

func TestLegacyHistorySurvivesSaveAndTrim(t *testing.T) {
	t.Setenv("BOX_ROOT", t.TempDir())
	log := fakeDocker(t, "exit 0\n")
	old := []byte(`{"images":{"app":"sha256:current"},"previous":{"app":"sha256:earlier"}}`)
	if err := writeFileAtomic(recordPath("blog"), old, 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := LoadRecord("blog")
	if err != nil {
		t.Fatal(err)
	}
	want := []Kept{{ID: "sha256:earlier", Legacy: true}}
	if !slices.Equal(r.History["app"], want) {
		t.Fatalf("migrated history = %v", r.History)
	}
	if err := r.save("blog"); err != nil {
		t.Fatal(err)
	}
	saved, _ := os.ReadFile(recordPath("blog"))
	if strings.Contains(string(saved), `"previous"`) {
		t.Fatalf("old field survived migration: %s", saved)
	}
	r, err = LoadRecord("blog")
	if err != nil || !slices.Equal(r.History["app"], want) {
		t.Fatalf("reloaded history = %v, %v", r.History, err)
	}
	trim(context.Background(), Config{}, "blog", Service{Name: "app", Keep: 0, Budget: -1}, &r, nil)
	commands, _ := os.ReadFile(log)
	if string(commands) != "image rm box-keep/blog:app-previous\n" || len(r.History["app"]) != 0 {
		t.Errorf("trim: history %v, commands %s", r.History, commands)
	}
}

func TestHistoryTakesPrecedenceOverLegacyRecord(t *testing.T) {
	t.Setenv("BOX_ROOT", t.TempDir())
	data := []byte(`{"images":{"same":"sha256:current"},"previous":{"app":"old","empty":"old","same":"sha256:current"},"history":{"app":[{"id":"new"}],"empty":[]}}`)
	if err := writeFileAtomic(recordPath("blog"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := LoadRecord("blog")
	if err != nil || len(r.History["app"]) != 1 || r.History["app"][0].ID != "new" || len(r.History["empty"])+len(r.History["same"]) != 0 {
		t.Fatalf("mixed record: %+v, %v", r, err)
	}
}

func TestImageSizesUseExactBytes(t *testing.T) {
	fakeDocker(t, `case "$1 $2" in
  'image ls') printf 'sha256:b\nsha256:a\nsha256:a\n' ;;
  'image inspect') printf 'sha256:a 999999999\nsha256:b 123456789\n' ;;
  *) exit 1 ;;
esac
`)
	sizes, err := imageSizes(t.Context())
	if err != nil || len(sizes) != 2 || sizes["sha256:a"] != 999999999 || sizes["sha256:b"] != 123456789 {
		t.Fatalf("image sizes = %v, %v", sizes, err)
	}
}

func TestUnavailableImageStorageDoesNotUseSiteDisk(t *testing.T) {
	t.Setenv("BOX_ROOT", t.TempDir())
	t.Setenv("BOX_IMAGE_STORE", "")
	if free, err := imageFreeSpace(); err == nil || free != -1 {
		t.Fatalf("unconfigured image storage = %d, %v", free, err)
	}
	t.Setenv("BOX_IMAGE_STORE", filepath.Join(t.TempDir(), "missing"))
	if free, err := imageFreeSpace(); err == nil || free != -1 {
		t.Fatalf("missing image storage = %d, %v", free, err)
	}
	t.Setenv("BOX_IMAGE_STORE", t.TempDir())
	if free, err := imageFreeSpace(); err != nil || free < 0 {
		t.Fatalf("configured image storage = %d, %v", free, err)
	}
	c := assess([]Usage{{Site: "blog", Service: "app", Running: 100, Budget: 50, Worst: 200}}, -1)
	if len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "keeps no history") {
		t.Fatalf("unknown free space should preserve budget warnings only: %v", c.Warnings)
	}
}
