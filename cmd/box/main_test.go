package main

import (
	"flag"
	"slices"
	"testing"
)

func TestParseInterspersed(t *testing.T) {
	fs := flag.NewFlagSet("new", flag.ContinueOnError)
	image := fs.String("image", "", "")
	var domains list
	fs.Var(&domains, "domain", "")
	rest, err := parse(fs, []string{"blog", "--image", "nginx", "--domain", "a.com", "--domain=b.com"})
	if err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(rest, []string{"blog"}) || *image != "nginx" || !slices.Equal(domains, list{"a.com", "b.com"}) {
		t.Errorf("rest = %v, image = %q, domains = %v", rest, *image, domains)
	}
}
