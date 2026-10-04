// Package server exposes the signed command API, site webhooks, and periodic image checks. It
// runs inside the box container with access to the host Docker socket.
package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/alfredr/box/internal/box"
	boxv1 "github.com/alfredr/box/internal/gen/box/v1"
	"github.com/alfredr/box/internal/gen/box/v1/boxv1connect"
	"github.com/alfredr/box/internal/sign"
)

// Options supplies the HTTP listener address, proxy startup choice, and reported build version.
type Options struct {
	// Listen is the host:port address for the HTTP listener and the proxy upstream.
	Listen string
	// Proxy enables proxy startup during host preparation. Disabling it still writes proxy
	// configuration and ensures the web network exists.
	Proxy   bool
	Version string
}

// Server handles API requests and webhook checks using the files under box.Root. It keeps the
// current API key and recent request nonces in memory.
type Server struct {
	opts     Options
	verifier *sign.Verifier
	hooks    runner
	poll     poller

	mu  sync.Mutex
	key string
}

var _ boxv1connect.BoxServiceHandler = (*Server)(nil)

// New loads the stored API key and initializes request verification, webhook execution, and
// polling state. It does not start a listener or prepare Docker resources. An empty key causes
// API authentication to fail.
func New(opts Options) (*Server, error) {
	key, err := box.APIKey()
	if err != nil {
		return nil, err
	}

	s := &Server{opts: opts, key: key}
	s.verifier = &sign.Verifier{Key: s.currentKey}
	s.hooks.run = func(site string) {
		// Reload settings for each webhook check so configuration changes apply without a
		// restart.
		cfg, err := box.LoadConfig()
		if err != nil {
			slog.Error("loading config", "err", err)
			return
		}

		// Webhook checks outlive the HTTP request that queued them. Shutdown waits for this
		// work separately.
		r := box.Check(context.Background(), cfg, site)
		slog.Info("webhook check", "site", site, "deployed", len(r.Deployed), "waiting", len(r.Waiting), "err", r.Err)
	}
	s.poll.wake = make(chan struct{}, 1)
	return s, nil
}

func (s *Server) currentKey() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.key
}

// Run starts the HTTP listener, prepares Docker resources, and starts polling. Preparation
// errors are logged without disabling the listener. On cancellation it allows five seconds for
// HTTP shutdown, then waits for polling and webhook checks to finish.
func Run(ctx context.Context, opts Options) error {
	s, err := New(opts)
	if err != nil {
		return err
	}

	if s.currentKey() == "" {
		slog.Warn("no API key yet, so the command API refuses everything (box setup installs one)")
	}

	srv := &http.Server{Addr: opts.Listen, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("box server listening", "addr", opts.Listen, "version", opts.Version)

	// Start serving health checks before pulling or starting the proxy, which can take longer
	// than container startup.
	cfg, err := box.LoadConfig()
	if err == nil {
		err = box.Prepare(ctx, cfg, opts.Listen, opts.Proxy)
	}

	if err != nil {
		slog.Error("preparing the host", "err", err)
	}

	var wg sync.WaitGroup
	wg.Go(func() { s.poll.run(ctx) })

	select {
	case err = <-errc:
	case <-ctx.Done():
		stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		err = srv.Shutdown(stop)
	}

	wg.Wait()
	s.hooks.wait()

	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}

	return err
}

// maxBody limits buffered command API request bodies to four MiB.
const maxBody = 4 << 20

// Handler routes health checks, per-site webhooks, and the signed Connect API. Health checks
// require no authentication. Webhooks use their site secret, and command API requests use the
// server key.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	mux.HandleFunc("POST /deploy/{site}", s.hook)

	path, api := boxv1connect.NewBoxServiceHandler(s, connect.WithInterceptors(codes{}), connect.WithReadMaxBytes(maxBody))
	mux.Handle(path, s.signed(api))
	return mux
}

var errorWriter = connect.NewErrorWriter()

// signed buffers a bounded request body for signature verification, then restores it for the
// API handler.
func (s *Server) signed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			errorWriter.Write(w, r, connect.NewError(connect.CodeResourceExhausted, err))
			return
		}

		if err := s.verifier.Verify(r, body); err != nil {
			slog.Warn("refused API request", "path", r.URL.Path, "from", r.Header.Get("X-Forwarded-For"), "err", err)
			errorWriter.Write(w, r, connect.NewError(connect.CodeUnauthenticated, err))
			return
		}

		r.Body = io.NopCloser(bytes.NewReader(body))
		next.ServeHTTP(w, r)
	})
}

// codes converts missing-site and validation errors to Connect status codes for unary and
// streaming handlers.
type codes struct{}

// WrapUnary maps the error returned by a unary handler to a Connect status.
func (codes) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		res, err := next(ctx, req)
		return res, withCode(err)
	}
}

// WrapStreamingClient leaves client calls unchanged because this interceptor handles server
// errors only.
func (codes) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// WrapStreamingHandler maps the error returned by a streaming handler to a Connect status.
func (codes) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error { return withCode(next(ctx, conn)) }
}

func withCode(err error) error {
	var ce *connect.Error
	switch {
	case err == nil, errors.As(err, &ce):
		return err
	case errors.Is(err, box.ErrNoSite):
		return connect.NewError(connect.CodeNotFound, err)
	case box.IsInvalid(err):
		return connect.NewError(connect.CodeInvalidArgument, err)
	}

	return connect.NewError(connect.CodeInternal, err)
}

// work preserves request values while removing cancellation and deadlines. A client disconnect
// must not interrupt a deployment or its recovery attempt.
func work(ctx context.Context) context.Context { return context.WithoutCancel(ctx) }

func stamp(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}

	return timestamppb.New(t)
}

func changes(cs []box.Change) []*boxv1.Change {
	out := make([]*boxv1.Change, len(cs))
	for i, c := range cs {
		out[i] = &boxv1.Change{Service: c.Service, From: c.From, To: c.To}
	}

	return out
}

func siteOf(st box.SiteStatus) *boxv1.Site {
	out := &boxv1.Site{
		Name:       st.Name,
		Domains:    st.Domains,
		DeployedAt: stamp(st.Record.DeployedAt),
		CheckedAt:  stamp(st.Record.CheckedAt),
		Result:     st.Record.Result,
		CheckError: st.Record.CheckError,
	}
	for _, s := range st.Services {
		out.Services = append(out.Services, &boxv1.Service{
			Name:     s.Name,
			Image:    s.Image,
			Policy:   string(s.Policy),
			State:    s.State,
			Running:  s.Running,
			Previous: s.Previous,
			Pending:  s.Pending,
			Rejected: st.Record.Rejected[s.Name],
		})
	}

	return out
}

func checkResult(r box.CheckResult) *boxv1.CheckResult {
	out := &boxv1.CheckResult{Site: r.Site, Deployed: changes(r.Deployed), Waiting: changes(r.Waiting)}
	if r.Err != nil {
		out.Error = r.Err.Error()
	}

	return out
}

// Version returns the build version supplied in Options.
func (s *Server) Version(ctx context.Context, _ *boxv1.VersionRequest) (*boxv1.VersionResponse, error) {
	return &boxv1.VersionResponse{Version: s.opts.Version}, nil
}

// Status reports server settings, polling times, proxy state, and site status. Failures to read
// individual sites are included in the response without discarding other sites.
func (s *Server) Status(ctx context.Context, _ *boxv1.StatusRequest) (*boxv1.StatusResponse, error) {
	cfg, err := box.LoadConfig()
	if err != nil {
		return nil, err
	}

	poll, _ := cfg.Get("poll")
	last, next := s.poll.times()
	out := &boxv1.StatusResponse{
		Version:   s.opts.Version,
		Domain:    cfg.Domain,
		Proxy:     box.ProxyState(ctx),
		Poll:      poll,
		LastCheck: stamp(last),
		NextCheck: stamp(next),
		Notify:    cfg.Notify,
	}

	names, err := box.ListSites()
	if err != nil {
		return nil, err
	}

	for _, name := range names {
		st, err := box.Status(ctx, name)
		if err != nil {
			if out.Errors == nil {
				out.Errors = map[string]string{}
			}

			out.Errors[name] = err.Error()
			continue
		}

		out.Sites = append(out.Sites, siteOf(st))
	}

	return out, nil
}

// GetSite returns the Compose metadata, container state, and image history for one site.
func (s *Server) GetSite(ctx context.Context, req *boxv1.GetSiteRequest) (*boxv1.GetSiteResponse, error) {
	st, err := box.Status(ctx, req.Site)
	if err != nil {
		return nil, err
	}

	return &boxv1.GetSiteResponse{Site: siteOf(st)}, nil
}

// CreateSite saves a new site and its webhook secret, then optionally deploys it. A deployment
// failure leaves the created site files in place.
func (s *Server) CreateSite(ctx context.Context, req *boxv1.CreateSiteRequest) (*boxv1.CreateSiteResponse, error) {
	policy, err := box.ParsePolicy(req.Policy)
	if err != nil {
		return nil, box.Invalid(err)
	}

	site := box.NewSite{
		Name:      req.Name,
		Image:     req.Image,
		Domains:   req.Domains,
		Redirects: req.Redirects,
		Port:      int(req.Port),
		Policy:    policy,
	}
	if err := box.CreateSite(site); err != nil {
		return nil, err
	}

	cfg, err := box.LoadConfig()
	if err != nil {
		return nil, err
	}

	out := &boxv1.CreateSiteResponse{}
	if url, err := cfg.HookURL(site.Name); err == nil {
		secret, err := box.HookSecret(site.Name)
		if err != nil {
			return nil, err
		}

		out.Hook = &boxv1.Hook{Site: site.Name, Url: url, Secret: secret}
	}

	if req.Deploy {
		cs, err := box.Deploy(work(ctx), cfg, site.Name, nil)
		if err != nil {
			return nil, errors.New("created, but the deploy failed: " + err.Error())
		}

		out.Changes = changes(cs)
	}

	return out, nil
}

// RemoveSite stops a site and removes its stored secret and record. Purge also deletes the site
// directory and Compose-managed volumes. Otherwise the directory is retained for recovery.
func (s *Server) RemoveSite(ctx context.Context, req *boxv1.RemoveSiteRequest) (*boxv1.RemoveSiteResponse, error) {
	kept, err := box.Remove(work(ctx), req.Site, req.Purge)
	if err != nil {
		return nil, err
	}

	return &boxv1.RemoveSiteResponse{Kept: kept}, nil
}

// Deploy pulls eligible service images and starts the requested services, attempting image
// recovery if startup fails. Work continues if the client disconnects.
func (s *Server) Deploy(ctx context.Context, req *boxv1.DeployRequest) (*boxv1.DeployResponse, error) {
	cfg, err := box.LoadConfig()
	if err != nil {
		return nil, err
	}

	cs, err := box.Deploy(work(ctx), cfg, req.Site, req.Services)
	if err != nil {
		return nil, err
	}

	return &boxv1.DeployResponse{Changes: changes(cs)}, nil
}

// Check runs image checks for the requested sites, or all sites when none are named. Individual
// check failures are included in the results. An invalid requested site aborts the call.
func (s *Server) Check(ctx context.Context, req *boxv1.CheckRequest) (*boxv1.CheckResponse, error) {
	cfg, err := box.LoadConfig()
	if err != nil {
		return nil, err
	}

	var results []box.CheckResult
	if len(req.Sites) == 0 {
		if results, err = box.CheckAll(work(ctx), cfg); err != nil {
			return nil, err
		}
	}

	for _, name := range req.Sites {
		if _, err := box.LoadSite(name); err != nil {
			return nil, err
		}

		results = append(results, box.Check(work(ctx), cfg, name))
	}

	out := &boxv1.CheckResponse{}
	for _, r := range results {
		out.Results = append(out.Results, checkResult(r))
	}

	return out, nil
}

// Rollback restores a site to its recorded previous images and excludes the replaced images
// from subsequent checks.
func (s *Server) Rollback(ctx context.Context, req *boxv1.RollbackRequest) (*boxv1.RollbackResponse, error) {
	cfg, err := box.LoadConfig()
	if err != nil {
		return nil, err
	}

	cs, err := box.Rollback(work(ctx), cfg, req.Site)
	if err != nil {
		return nil, err
	}

	return &boxv1.RollbackResponse{Changes: changes(cs)}, nil
}

// Logs streams site container logs until completion or client cancellation. A nonpositive Tail
// uses 100 lines per container.
func (s *Server) Logs(ctx context.Context, req *boxv1.LogsRequest, stream *connect.ServerStream[boxv1.LogsResponse]) error {
	tail := int(req.Tail)
	if tail <= 0 {
		tail = 100
	}

	return box.Logs(ctx, logWriter{stream}, req.Site, req.Services, tail, req.Follow)
}

// logWriter forwards each Docker output chunk as one Connect stream message without additional
// buffering.
type logWriter struct {
	stream *connect.ServerStream[boxv1.LogsResponse]
}

// Write sends p as one log message and reports the full byte count only if the send succeeds.
func (w logWriter) Write(p []byte) (int, error) {
	if err := w.stream.Send(&boxv1.LogsResponse{Data: p}); err != nil {
		return 0, err
	}

	return len(p), nil
}

// GetCompose returns the stored site Compose file after validating the site name and
// definition.
func (s *Server) GetCompose(ctx context.Context, req *boxv1.GetComposeRequest) (*boxv1.GetComposeResponse, error) {
	data, err := box.ReadCompose(req.Site)
	if err != nil {
		return nil, err
	}

	return &boxv1.GetComposeResponse{Text: string(data)}, nil
}

// ApplyCompose validates and applies an edited Compose file. A startup failure triggers an
// attempt to restore the old file. Work continues if the client disconnects.
func (s *Server) ApplyCompose(ctx context.Context, req *boxv1.ApplyComposeRequest) (*boxv1.ApplyComposeResponse, error) {
	cfg, err := box.LoadConfig()
	if err != nil {
		return nil, err
	}

	return &boxv1.ApplyComposeResponse{}, box.Apply(work(ctx), cfg, req.Site, []byte(req.Text))
}

// GetConfig returns supported settings in CLI display order, including the effective default
// polling interval.
func (s *Server) GetConfig(ctx context.Context, _ *boxv1.GetConfigRequest) (*boxv1.GetConfigResponse, error) {
	cfg, err := box.LoadConfig()
	if err != nil {
		return nil, err
	}

	out := &boxv1.GetConfigResponse{}
	for _, k := range box.ConfigKeys {
		v, _ := cfg.Get(k)
		out.Settings = append(out.Settings, &boxv1.Setting{Key: k, Value: v})
	}

	return out, nil
}

// SetConfig validates and saves one setting, then updates proxy files or polling state as
// needed. A nonempty notification URL must accept a test message before it is saved. Proxy
// update failures can occur after the setting is saved.
func (s *Server) SetConfig(ctx context.Context, req *boxv1.SetConfigRequest) (*boxv1.SetConfigResponse, error) {
	cfg, err := box.LoadConfig()
	if err != nil {
		return nil, err
	}

	if err := cfg.Set(req.Key, req.Value); err != nil {
		return nil, err
	}

	if req.Key == "notify" && req.Value != "" {
		if err := box.SendNotice(req.Value, "box: notices on", "box will post deploys, failures and waiting updates here.", false); err != nil {
			return nil, box.Invalid(errors.New("the test notice failed, so the setting is unchanged: " + err.Error()))
		}
	}

	if err := cfg.Save(); err != nil {
		return nil, err
	}

	switch req.Key {
	case "domain", "email":
		if err := box.WriteProxy(cfg, s.opts.Listen); err != nil {
			return nil, err
		}
	case "poll":
		s.poll.nudge()
	}

	return &boxv1.SetConfigResponse{}, nil
}

// GetHook returns the site webhook URL and secret, creating a secret if the site has none.
func (s *Server) GetHook(ctx context.Context, req *boxv1.GetHookRequest) (*boxv1.GetHookResponse, error) {
	h, err := hookFor(req.Site, box.EnsureHookSecret)
	if err != nil {
		return nil, err
	}

	return &boxv1.GetHookResponse{Hook: h}, nil
}

// RotateHook saves a new site webhook secret and returns it with the URL. The previous secret
// stops authenticating requests after replacement.
func (s *Server) RotateHook(ctx context.Context, req *boxv1.RotateHookRequest) (*boxv1.RotateHookResponse, error) {
	h, err := hookFor(req.Site, box.RotateHookSecret)
	if err != nil {
		return nil, err
	}

	return &boxv1.RotateHookResponse{Hook: h}, nil
}

func hookFor(name string, secret func(string) (string, error)) (*boxv1.Hook, error) {
	if _, err := box.LoadSite(name); err != nil {
		return nil, err
	}

	cfg, err := box.LoadConfig()
	if err != nil {
		return nil, err
	}

	url, err := cfg.HookURL(name)
	if err != nil {
		return nil, box.Invalid(err)
	}

	sec, err := secret(name)
	if err != nil {
		return nil, err
	}

	return &boxv1.Hook{Site: name, Url: url, Secret: sec}, nil
}

// ListHooks returns webhook URLs for all sites without exposing their secrets.
func (s *Server) ListHooks(ctx context.Context, _ *boxv1.ListHooksRequest) (*boxv1.ListHooksResponse, error) {
	cfg, err := box.LoadConfig()
	if err != nil {
		return nil, err
	}

	names, err := box.ListSites()
	if err != nil {
		return nil, err
	}

	out := &boxv1.ListHooksResponse{}
	for _, n := range names {
		url, err := cfg.HookURL(n)
		if err != nil {
			return nil, box.Invalid(err)
		}

		out.Hooks = append(out.Hooks, &boxv1.Hook{Site: n, Url: url})
	}

	return out, nil
}

// RotateKey validates and saves a replacement API key, then updates the in-memory key used for
// subsequent requests. The handler is authenticated with the current key before rotation.
func (s *Server) RotateKey(ctx context.Context, req *boxv1.RotateKeyRequest) (*boxv1.RotateKeyResponse, error) {
	if err := box.SetAPIKey(req.Key); err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.key = req.Key
	s.mu.Unlock()

	slog.Info("API key replaced")
	return &boxv1.RotateKeyResponse{}, nil
}

// DockerInfo reports daemon and Compose versions, network and proxy state, registry names,
// daemon settings, and disk usage.
func (s *Server) DockerInfo(ctx context.Context, _ *boxv1.DockerInfoRequest) (*boxv1.DockerInfoResponse, error) {
	info, err := box.DockerReport(ctx)
	if err != nil {
		return nil, err
	}

	return &boxv1.DockerInfoResponse{
		Engine:     info.Engine,
		Compose:    info.Compose,
		Network:    info.Network,
		Proxy:      info.Proxy,
		Daemon:     info.Daemon,
		Registries: info.Registries,
		Disk:       info.Disk,
	}, nil
}

// DockerLogin saves registry credentials through the server Docker CLI for later site image
// pulls.
func (s *Server) DockerLogin(ctx context.Context, req *boxv1.DockerLoginRequest) (*boxv1.DockerLoginResponse, error) {
	return &boxv1.DockerLoginResponse{}, box.DockerLogin(ctx, req.Registry, req.Username, req.Password)
}
