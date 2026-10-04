package engine

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
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

func TestImageStoreDiscovery(t *testing.T) {
	for _, tc := range []struct{ name, response, want string }{
		{"classic custom root", `{"Driver":"overlay2","DockerRootDir":"/data/docker"}`, "/data/docker"},
		{"containerd", `{"Driver":"overlayfs","DockerRootDir":"/metadata/docker","DriverStatus":[["driver-type","io.containerd.snapshotter.v1"]]}`, ""},
		{"containerd custom driver", `{"Driver":"overlay2","DockerRootDir":"/metadata/docker","DriverStatus":[["driver-type","io.containerd.snapshotter.v1"]]}`, ""},
		{"unknown backend", `{"Driver":"devicemapper","DockerRootDir":"/metadata/docker"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/"+apiVersion+"/info" {
					t.Errorf("unexpected path: %s", r.URL.Path)
				}
				w.Write([]byte(tc.response))
			}))
			defer ts.Close()
			c := New(func(ctx context.Context) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", ts.Listener.Addr().String())
			})
			got, err := c.ImageStore(t.Context())
			if err != nil || got != tc.want {
				t.Fatalf("image store = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestReadOnlyStorageMount(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var body struct {
				HostConfig struct {
					Mounts []struct {
						Type, Source, Target string
						ReadOnly             bool
					}
				}
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if len(body.HostConfig.Mounts) != 1 {
				t.Errorf("mounts = %+v", body.HostConfig.Mounts)
			} else {
				m := body.HostConfig.Mounts[0]
				if m.Type != "bind" || m.Source != "/data/images" || m.Target != "/images" || !m.ReadOnly {
					t.Errorf("mount = %+v", m)
				}
			}
			w.Write([]byte(`{"Id":"created"}`))
		case http.MethodGet:
			w.Write([]byte(`{"Id":"created","Mounts":[{"Type":"bind","Source":"/data/images","Destination":"/images"},{"Type":"volume","Source":"/volumes/ignore","Destination":"/volume"}]}`))
		}
	}))
	defer ts.Close()
	c := New(func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", ts.Listener.Addr().String())
	})
	if _, err := c.Create(t.Context(), Container{Image: "box:test", ReadOnlyBinds: map[string]string{"/images": "/data/images"}}); err != nil {
		t.Fatal(err)
	}
	st, err := c.Inspect(t.Context(), "created")
	if err != nil || st.Mounts["/images"] != "/data/images" || len(st.Mounts) != 1 {
		t.Fatalf("mounts = %v, %v", st.Mounts, err)
	}
}
