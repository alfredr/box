package engine

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func frame(stream byte, s string) []byte {
	h := make([]byte, 8)
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(s)))
	return append(h, s...)
}

func TestDemux(t *testing.T) {
	var in bytes.Buffer
	in.Write(frame(1, "out one\n"))
	in.Write(frame(2, "err one\n"))
	in.Write(frame(1, ""))
	in.Write(frame(1, "out two\n"))

	var out bytes.Buffer
	if err := demux(&in, &out); err != nil {
		t.Fatal(err)
	}

	if out.String() != "out one\nerr one\nout two\n" {
		t.Errorf("demux = %q", out.String())
	}

	if err := demux(bytes.NewReader(frame(1, "cut short")[:12]), &out); err == nil {
		t.Error("a truncated frame should fail")
	}
}

func TestVersionSupported(t *testing.T) {
	cases := map[string]bool{"1.44": true, "1.51": true, "2.0": true, "1.43": false, "1.41": false, "": false}
	for api, want := range cases {
		if got := (Version{APIVersion: api}).Supported(); got != want {
			t.Errorf("API %q supported = %v, want %v", api, got, want)
		}
	}
}
