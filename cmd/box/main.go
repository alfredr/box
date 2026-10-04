// Command box manages Docker Compose sites from a local CLI and runs the server API with the
// serve subcommand.
package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/term"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/alfredr/box/internal/box"
	"github.com/alfredr/box/internal/client"
	"github.com/alfredr/box/internal/engine"
	boxv1 "github.com/alfredr/box/internal/gen/box/v1"
	"github.com/alfredr/box/internal/gen/box/v1/boxv1connect"
	"github.com/alfredr/box/internal/server"
)

// version may be supplied by the linker with -X main.version=<value>. An empty value falls back
// to Go build metadata.
var version = ""

const usage = `box runs sites on a Docker server and is driven from here.

Server (over ssh)
  box setup <user@host> --domain box.example.com [--email E]   install or reinstall the server
  box server upgrade [--image IMG]                             pull the server image and restart it
  box server logs [-f]                                         the server's own logs
  box server docker [--log-size 10m] [--log-files 3] [--live-restore] [--restart]
  box servers                                                  saved servers

Sites
  box new <site> --image IMG --domain D   create a site (box new -h)
  box status [site]                       what's running, what's waiting
  box deploy <site> [service...]          pull and start, rolling back if it fails
  box check [site]                        look for new images now (every site by default)
  box rollback <site> [--to N|ID]         go back to a kept image (the most recent by default)
  box logs <site> [service] [-f] [-n N]   container logs
  box edit <site>                         edit the compose file here, apply it there
  box remove <site> [--purge]             stop a site and set its files aside

Settings
  box config [key [value]]                domain, email, notify, poll (5m, or off)
  box hook url [site] | rotate <site>     webhook addresses and secrets for CI
  box key rotate                          replace the API key
  box docker check | login [registry]     the server's Docker, and registry logins
  box prune                               remove images nothing uses or names
  box version

--server NAME (or BOX_SERVER) picks a saved server when there are several.
`

var errUsage = errors.New("usage")

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	serverName := ""
	if len(args) >= 2 && args[0] == "--server" {
		serverName, args = args[1], args[2:]
	} else if len(args) >= 1 && strings.HasPrefix(args[0], "--server=") {
		serverName, args = strings.TrimPrefix(args[0], "--server="), args[1:]
	}

	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := dispatch(ctx, serverName, args[0], args[1:])
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errUsage):
		return 2
	default:
		fmt.Fprintln(os.Stderr, "box:", client.Message(err))
		return 1
	}
}

func dispatch(ctx context.Context, serverName, cmd string, args []string) error {
	switch cmd {
	case "serve":
		return cmdServe(ctx)
	case "health":
		return cmdHealth()
	case "setup":
		return cmdSetup(ctx, serverName, args)
	case "servers":
		return cmdServers()
	case "version", "--version":
		return cmdVersion(ctx, serverName)
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	}

	commands := map[string]func(context.Context, *conn, []string) error{
		"server":   cmdServer,
		"new":      cmdNew,
		"status":   cmdStatus,
		"deploy":   cmdDeploy,
		"check":    cmdCheck,
		"rollback": cmdRollback,
		"logs":     cmdLogs,
		"edit":     cmdEdit,
		"remove":   cmdRemove,
		"config":   cmdConfig,
		"hook":     cmdHook,
		"key":      cmdKey,
		"docker":   cmdDocker,
		"prune":    cmdPrune,
	}
	f, ok := commands[cmd]
	if !ok {
		fmt.Fprintf(os.Stderr, "box: no command %q\n\n%s", cmd, usage)
		return errUsage
	}

	c, err := dial(serverName)
	if err != nil {
		return err
	}

	return f(ctx, c, args)
}

// conn holds the selected profile, its signed API client, and the profile collection needed to
// persist key changes.
type conn struct {
	name     string
	profiles client.Profiles
	profile  client.Profile
	api      boxv1connect.BoxServiceClient
}

func dial(name string) (*conn, error) {
	profiles, err := client.LoadProfiles()
	if err != nil {
		return nil, err
	}

	name, p, err := profiles.Pick(name)
	if err != nil {
		return nil, err
	}

	return &conn{name: name, profiles: profiles, profile: p, api: client.New(p)}, nil
}

// docker opens an SSH-forwarded Docker connection for server maintenance. The caller must
// invoke the returned cleanup function exactly once.
func (c *conn) docker(ctx context.Context) (*engine.Client, func(), error) {
	if c.profile.SSH == "" {
		return nil, nil, fmt.Errorf("server %s has no ssh login saved (box setup saves it)", c.name)
	}

	return client.Host{Target: c.profile.SSH}.Docker(ctx)
}

// parse accepts flags before or after positional arguments. It returns positional arguments in
// their original order.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var rest []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, errUsage
		}

		args = fs.Args()
		if len(args) == 0 {
			return rest, nil
		}

		rest, args = append(rest, args[0]), args[1:]
	}
}

func flags(name, synopsis string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: box %s\n", synopsis)
		fs.PrintDefaults()
	}

	return fs
}

func wrongArgs(fs *flag.FlagSet) error {
	fs.Usage()
	return errUsage
}

// list collects repeated flag values without splitting values on commas.
type list []string

// String joins values with commas for flag display.
func (l *list) String() string { return strings.Join(*l, ",") }

// Set appends one occurrence of the flag value.
func (l *list) Set(v string) error { *l = append(*l, v); return nil }

func one(fs *flag.FlagSet, args []string) (string, error) {
	rest, err := parse(fs, args)
	if err != nil {
		return "", err
	}

	if len(rest) != 1 {
		return "", wrongArgs(fs)
	}

	return rest[0], nil
}

func listenAddr() string {
	if l := os.Getenv("BOX_LISTEN"); l != "" {
		return l
	}

	return client.ServerListen
}

func cmdServe(ctx context.Context) error {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	opts := server.Options{Listen: listenAddr(), Proxy: os.Getenv("BOX_PROXY") != "off", Version: versionString()}
	return server.Run(ctx, opts)
}

// cmdHealth checks only whether the HTTP health endpoint answers. It does not verify Docker,
// proxy startup, or TLS readiness.
func cmdHealth() error {
	hc := &http.Client{Timeout: 3 * time.Second}
	resp, err := hc.Get("http://" + listenAddr() + "/healthz")
	if err != nil {
		return err
	}

	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New(resp.Status)
	}

	return nil
}

func cmdSetup(ctx context.Context, serverName string, args []string) error {
	fs := flags("setup", "setup <user@host> --domain box.example.com [flags]")
	domain := fs.String("domain", "", "where the server is reached, for this client and for webhooks (needs a DNS record)")
	email := fs.String("email", "", "ACME account email for certificates")
	image := fs.String("image", client.DefaultImage, "server image")
	login := fs.String("login", "", "registry to log in to for a private server image, such as ghcr.io")
	install := fs.Bool("install-docker", false, "install Docker without asking if it's missing")
	logSize := fs.String("log-size", "10m", "rotate container logs at this size")
	logFiles := fs.Int("log-files", 3, "rotated log files kept per container")
	live := fs.Bool("live-restore", true, "keep containers running while Docker restarts")
	keepDaemon := fs.Bool("keep-docker-config", false, "leave the server's /etc/docker/daemon.json alone")
	target, err := one(fs, args)
	if err != nil {
		return err
	}

	if *domain == "" {
		return wrongArgs(fs)
	}

	profiles, err := client.LoadProfiles()
	if err != nil {
		return err
	}

	name := serverName
	if name == "" {
		name = *domain
	}

	opts := client.SetupOptions{
		Target:        target,
		InstallDocker: *install,
		Confirm:       confirm,
		InstallOptions: client.InstallOptions{
			Server: client.DefaultServer(*image),
			Domain: *domain,
			Email:  *email,
			Key:    profiles.Servers[name].Key,
			Pull:   true,
			Log:    func(s string) { fmt.Println(s) },
		},
	}
	if !*keepDaemon {
		opts.Daemon = box.DaemonSettings{LogSize: *logSize, LogFiles: *logFiles, LiveRestore: live}
	}

	if *login != "" {
		if opts.Auth, err = askLogin(*login); err != nil {
			return err
		}
	}

	p, err := client.Setup(ctx, opts)
	if err != nil {
		return err
	}

	profiles.Put(name, p)
	if err := profiles.Save(); err != nil {
		return fmt.Errorf("the server is up, but saving its key failed: %w", err)
	}

	fmt.Printf("saved server %s in %s\n\n", name, client.ProfilesPath())

	check, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	v, err := client.New(p).Version(check, &boxv1.VersionRequest{})
	if err != nil {
		fmt.Printf("The server is running, but %s doesn't answer yet (%s).\n", p.URL, client.Message(err))
		fmt.Printf("Point a DNS record for %s at the server. Once Caddy has its certificate, box status works.\n", *domain)
		return nil
	}

	fmt.Printf("%s answers (server %s). Next: box new <site> --image IMG --domain D --deploy\n", p.URL, v.Version)
	return nil
}

func askLogin(registry string) (*engine.Auth, error) {
	if registry == "ghcr.io" {
		fmt.Fprintln(os.Stderr, "For ghcr.io: your GitHub username, and a token with read:packages as the password.")
	}

	fmt.Fprintf(os.Stderr, "%s username: ", registry)
	line, _ := stdin.ReadString('\n')
	user := strings.TrimSpace(line)

	fmt.Fprint(os.Stderr, "Password: ")
	pw, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return nil, err
	}

	return &engine.Auth{Username: user, Password: string(pw), ServerAddress: registry}, nil
}

func cmdServers() error {
	profiles, err := client.LoadProfiles()
	if err != nil {
		return err
	}

	if len(profiles.Servers) == 0 {
		fmt.Println("No servers yet: box setup root@your-server --domain box.example.com")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tURL\tSSH\t")
	for _, n := range profiles.Names() {
		p := profiles.Servers[n]
		mark := ""
		if n == profiles.Default {
			mark = "(default)"
		}

		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", n, p.URL, p.SSH, mark)
	}

	return tw.Flush()
}

func cmdServer(ctx context.Context, c *conn, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, "usage: box server upgrade [--image IMG] | logs [-f] | docker [flags]\n")
		return errUsage
	}

	switch args[0] {
	case "upgrade":
		fs := flags("server upgrade", "server upgrade [--image IMG] [--login REGISTRY]")
		image := fs.String("image", "", "switch to this server image")
		login := fs.String("login", "", "registry to log in to for a private server image")
		if rest, err := parse(fs, args[1:]); err != nil || len(rest) > 0 {
			return wrongArgs(fs)
		}

		var auth *engine.Auth
		if *login != "" {
			var err error
			if auth, err = askLogin(*login); err != nil {
				return err
			}
		}

		eng, closeFn, err := c.docker(ctx)
		if err != nil {
			return err
		}

		defer closeFn()

		if err := client.Upgrade(ctx, eng, *image, auth); err != nil {
			return err
		}

		v, err := c.api.Version(ctx, &boxv1.VersionRequest{})
		if err != nil {
			return err
		}

		fmt.Printf("server %s\n", v.Version)
		return nil
	case "logs":
		fs := flags("server logs", "server logs [-f] [-n N]")
		follow := fs.Bool("f", false, "keep following")
		tail := fs.Int("n", 200, "lines to show from the end")
		if rest, err := parse(fs, args[1:]); err != nil || len(rest) > 0 {
			return wrongArgs(fs)
		}

		eng, closeFn, err := c.docker(ctx)
		if err != nil {
			return err
		}

		defer closeFn()

		return eng.Logs(ctx, client.ServerName, *tail, *follow, os.Stdout)
	case "docker":
		return serverDocker(ctx, c, args[1:])
	}

	return fmt.Errorf("no server command %q: upgrade, logs or docker", args[0])
}

func serverDocker(ctx context.Context, c *conn, args []string) error {
	fs := flags("server docker", "server docker [--log-size 10m] [--log-files 3] [--live-restore] [--restart]")
	size := fs.String("log-size", "", "rotate container logs at this size, like 10m")
	files := fs.Int("log-files", 0, "rotated log files to keep per container")
	live := fs.Bool("live-restore", false, "keep containers running while Docker restarts (--live-restore=false turns it off)")
	restart := fs.Bool("restart", false, "restart Docker so the changes take effect")
	if rest, err := parse(fs, args); err != nil || len(rest) > 0 {
		return wrongArgs(fs)
	}

	s := box.DaemonSettings{LogSize: *size, LogFiles: *files}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "live-restore" {
			s.LiveRestore = live
		}
	})

	if s == (box.DaemonSettings{}) {
		return wrongArgs(fs)
	}

	eng, closeFn, err := c.docker(ctx)
	if err != nil {
		return err
	}

	defer closeFn()

	changed, err := client.ConfigureDaemon(ctx, eng, s)
	switch {
	case err != nil:
		return err
	case !changed:
		fmt.Println("daemon.json already says that.")
		return nil
	case !*restart:
		fmt.Println("daemon.json updated. It takes effect when Docker restarts (add --restart).")
		return nil
	}

	if err := (client.Host{Target: c.profile.SSH}).RestartDocker(ctx); err != nil {
		return err
	}

	fmt.Println("daemon.json updated and Docker restarted. Log settings apply to containers created from now on.")
	return nil
}

func cmdNew(ctx context.Context, c *conn, args []string) error {
	fs := flags("new", "new <site> --image IMG --domain D [flags]")
	image := fs.String("image", "", "image to run, like ghcr.io/you/app:latest")
	var domains, redirects list
	fs.Var(&domains, "domain", "a domain the site serves; repeat for more (the first is its main address)")
	fs.Var(&redirects, "redirect", "a domain that redirects to the first --domain, like www.example.com; repeatable")
	port := fs.Int("port", 80, "port the container listens on")
	update := fs.String("update", "auto", "what a check does with a new image: auto, manual or pinned")
	deploy := fs.Bool("deploy", false, "start the site now")
	name, err := one(fs, args)
	if err != nil {
		return err
	}

	if *image == "" || len(domains) == 0 {
		return wrongArgs(fs)
	}

	policy, err := box.ParsePolicy(*update)
	if err != nil {
		return err
	}

	// Validate template inputs locally before sending a request that creates server files.
	site := box.NewSite{Name: name, Image: *image, Domains: domains, Redirects: redirects, Port: *port, Policy: policy}
	if _, err := site.Compose(); err != nil {
		return err
	}

	out, err := c.api.CreateSite(ctx, &boxv1.CreateSiteRequest{
		Name:      name,
		Image:     *image,
		Domains:   domains,
		Redirects: redirects,
		Port:      int32(*port),
		Policy:    string(policy),
		Deploy:    *deploy,
	})
	if err != nil {
		return err
	}

	fmt.Printf("created %s\n", name)
	switch {
	case *deploy:
		printChanges(name, out.Changes)
	case policy == box.Auto:
		fmt.Printf("It starts when its first image is found (a webhook, the next poll, or box check %s), or now with box deploy %s.\n", name, name)
	default:
		fmt.Printf("Start it with: box deploy %s\n", name)
	}

	fmt.Printf("\nDNS: point A (and AAAA) records for %s at the server.\n", strings.Join(append(domains, redirects...), ", "))
	if out.Hook != nil {
		fmt.Println()
		printHook(out.Hook)
	}

	printWarnings(out.Warnings)
	return nil
}

func printWarnings(warnings []string) {
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
}

func printChanges(name string, changes []*boxv1.Change) {
	if len(changes) == 0 {
		fmt.Printf("%s: up to date\n", name)
		return
	}

	for _, ch := range changes {
		fmt.Printf("%s: %s\n", name, change(ch))
	}
}

func change(c *boxv1.Change) string {
	return box.Change{Service: c.Service, From: c.From, To: c.To}.String()
}

func cmdStatus(ctx context.Context, c *conn, args []string) error {
	fs := flags("status", "status [site]")
	rest, err := parse(fs, args)
	if err != nil {
		return err
	}

	switch len(rest) {
	case 0:
		return hostStatus(ctx, c)
	case 1:
		return siteStatus(ctx, c, rest[0])
	}

	return wrongArgs(fs)
}

func hostStatus(ctx context.Context, c *conn) error {
	st, err := c.api.Status(ctx, &boxv1.StatusRequest{})
	if err != nil {
		return err
	}

	poll := st.Poll
	if st.NextCheck != nil {
		poll = "every " + poll + ", next " + st.NextCheck.AsTime().Local().Format("15:04")
	}

	notify := st.Notify
	if notify == "" {
		notify = "off (box config notify https://ntfy.sh/your-topic)"
	}

	fmt.Printf("server   %s, %s (%s)\n", c.name, c.profile.URL, st.Version)
	fmt.Printf("proxy    %s\n", st.Proxy)
	fmt.Printf("polling  %s\n", poll)
	fmt.Printf("notify   %s\n", notify)
	fmt.Printf("images   %s kept, up to %s with the current limits, %s free\n\n",
		box.FormatSize(st.KeptBytes), box.FormatSize(st.WorstBytes), box.FormatSize(st.FreeBytes))
	printWarnings(st.Warnings)

	if len(st.Sites)+len(st.Errors) == 0 {
		fmt.Println("No sites yet: box new <site> --image IMG --domain D")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SITE\tSERVICE\tUPDATE\tSTATE\tIMAGE\tDEPLOYED\tNOTE")
	for _, site := range st.Sites {
		for _, s := range site.Services {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", site.Name, s.Name, s.Policy, s.State, short(s.Running),
				when(site.DeployedAt), note(s))
		}
	}

	for name, e := range st.Errors {
		fmt.Fprintf(tw, "%s\t\t\t\t\t\t%s\n", name, e)
	}

	return tw.Flush()
}

func siteStatus(ctx context.Context, c *conn, name string) error {
	out, err := c.api.GetSite(ctx, &boxv1.GetSiteRequest{Site: name})
	if err != nil {
		return err
	}

	st := out.Site
	fmt.Printf("site      %s\n", name)
	fmt.Printf("domains   %s\n", strings.Join(st.Domains, ", "))
	fmt.Printf("deployed  %s", when(st.DeployedAt))
	if st.Result != "" {
		fmt.Printf(" (%s)", st.Result)
	}

	fmt.Printf("\nchecked   %s", when(st.CheckedAt))
	if st.CheckError != "" {
		fmt.Printf(" (failing: %s)", st.CheckError)
	}

	fmt.Print("\n\n")
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SERVICE\tIMAGE REF\tUPDATE\tSTATE\tRUNNING\tKEPT\tNOTE")
	for _, s := range st.Services {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.Name, s.Image, s.Policy, s.State, short(s.Running),
			kept(s), note(s))
	}

	if err := tw.Flush(); err != nil {
		return err
	}

	for _, s := range st.Services {
		if len(s.History) == 0 {
			continue
		}

		fmt.Printf("\n%s history (box rollback %s --to N or ID):\n", s.Name, name)
		for i, k := range s.History {
			fmt.Printf("  %-3d %s  deployed %s\n", i+1, box.ShortID(k.Id), when(k.DeployedAt))
		}
	}

	return nil
}

func kept(s *boxv1.Service) string {
	limit := "no budget"
	if s.Budget > 0 {
		limit = "budget " + box.FormatSize(s.Budget)
	}

	return fmt.Sprintf("%d of %d, %s (%s)", len(s.History), s.Keep, box.FormatSize(s.KeptBytes), limit)
}

func short(id string) string {
	if id == "" {
		return "-"
	}

	return box.ShortID(id)
}

func when(t *timestamppb.Timestamp) string {
	if t == nil {
		return "never"
	}

	return t.AsTime().Local().Format("2006-01-02 15:04")
}

func note(s *boxv1.Service) string {
	switch {
	case s.Pending != "":
		return "new image " + box.ShortID(s.Pending) + " waiting: box deploy"
	case s.Rejected != "":
		return "holding back " + box.ShortID(s.Rejected)
	}

	return ""
}

func cmdDeploy(ctx context.Context, c *conn, args []string) error {
	fs := flags("deploy", "deploy <site> [service...]")
	rest, err := parse(fs, args)
	if err != nil {
		return err
	}

	if len(rest) == 0 {
		return wrongArgs(fs)
	}

	out, err := c.api.Deploy(ctx, &boxv1.DeployRequest{Site: rest[0], Services: rest[1:]})
	if err != nil {
		return err
	}

	printChanges(rest[0], out.Changes)
	return nil
}

func cmdCheck(ctx context.Context, c *conn, args []string) error {
	fs := flags("check", "check [site...]")
	sites, err := parse(fs, args)
	if err != nil {
		return err
	}

	out, err := c.api.Check(ctx, &boxv1.CheckRequest{Sites: sites})
	if err != nil {
		return err
	}

	failed := 0
	for _, r := range out.Results {
		switch {
		case r.Error != "":
			failed++
			fmt.Printf("%s: %s\n", r.Site, r.Error)
		case len(r.Deployed)+len(r.Waiting) == 0:
			fmt.Printf("%s: up to date\n", r.Site)
		}

		for _, ch := range r.Deployed {
			fmt.Printf("%s: deployed %s\n", r.Site, change(ch))
		}

		for _, ch := range r.Waiting {
			fmt.Printf("%s: waiting %s (box deploy %s)\n", r.Site, change(ch), r.Site)
		}
	}

	if failed > 0 {
		return fmt.Errorf("%d of %d checks failed", failed, len(out.Results))
	}

	return nil
}

func cmdRollback(ctx context.Context, c *conn, args []string) error {
	fs := flags("rollback", "rollback <site> [--to N|ID]")
	to := fs.String("to", "", "the kept image to go back to: its number in box status <site>, or an image ID prefix (default 1, the most recent)")
	name, err := one(fs, args)
	if err != nil {
		return err
	}

	out, err := c.api.Rollback(ctx, &boxv1.RollbackRequest{Site: name, To: *to})
	if err != nil {
		return err
	}

	printChanges(name, out.Changes)
	fmt.Printf("Checks won't auto-deploy the image you left. box deploy %s goes forward again.\n", name)
	return nil
}

func cmdLogs(ctx context.Context, c *conn, args []string) error {
	fs := flags("logs", "logs <site> [service...] [-f] [-n N]")
	follow := fs.Bool("f", false, "keep following")
	tail := fs.Int("n", 100, "lines to show from the end of each log")
	rest, err := parse(fs, args)
	if err != nil {
		return err
	}

	if len(rest) == 0 {
		return wrongArgs(fs)
	}

	stream, err := c.api.Logs(ctx, &boxv1.LogsRequest{Site: rest[0], Services: rest[1:], Tail: int32(*tail), Follow: *follow})
	if err != nil {
		return err
	}

	defer stream.Close()

	for stream.Receive() {
		os.Stdout.Write(stream.Msg().Data)
	}

	if ctx.Err() != nil {
		return nil
	}

	return stream.Err()
}

func cmdEdit(ctx context.Context, c *conn, args []string) error {
	name, err := one(flags("edit", "edit <site>"), args)
	if err != nil {
		return err
	}

	orig, err := c.api.GetCompose(ctx, &boxv1.GetComposeRequest{Site: name})
	if err != nil {
		return err
	}

	dir, err := os.MkdirTemp("", "box-edit-")
	if err != nil {
		return err
	}

	defer os.RemoveAll(dir)

	draft := filepath.Join(dir, name+".compose.yml")
	if err := os.WriteFile(draft, []byte(orig.Text), 0o600); err != nil {
		return err
	}

	for {
		if err := editor(draft); err != nil {
			return err
		}

		edited, err := os.ReadFile(draft)
		if err != nil {
			return err
		}

		if bytes.Equal(edited, []byte(orig.Text)) {
			fmt.Println("No changes.")
			return nil
		}

		// Check the fields box reads locally. The server also runs full Compose validation
		// before applying the file.
		_, err = box.ParseSite(name, edited)
		var applied *boxv1.ApplyComposeResponse
		if err == nil {
			applied, err = c.api.ApplyCompose(ctx, &boxv1.ApplyComposeRequest{Site: name, Text: string(edited)})
		}

		if err == nil {
			fmt.Printf("%s: applied\n", name)
			printWarnings(applied.Warnings)
			return nil
		}

		if !box.IsInvalid(err) && connect.CodeOf(err) != connect.CodeInvalidArgument {
			return err
		}

		fmt.Fprintln(os.Stderr, client.Message(err))
		if !confirm("Edit again?") {
			return errors.New("left the site as it was")
		}
	}
}

// editor uses VISUAL, then EDITOR, then vi. The editor setting is interpreted as a shell
// command so it can include arguments. The file path is passed as a quoted positional argument.
func editor(path string) error {
	ed := os.Getenv("VISUAL")
	if ed == "" {
		ed = os.Getenv("EDITOR")
	}

	if ed == "" {
		ed = "vi"
	}

	cmd := exec.Command("sh", "-c", ed+` "$1"`, "sh", path)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

var stdin = bufio.NewReader(os.Stdin)

func confirm(question string) bool {
	fmt.Fprintf(os.Stderr, "%s [y/N] ", question)
	line, _ := stdin.ReadString('\n')
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

func cmdRemove(ctx context.Context, c *conn, args []string) error {
	fs := flags("remove", "remove <site> [--purge] [--yes]")
	purge := fs.Bool("purge", false, "also delete the site's volumes and compose file")
	yes := fs.Bool("yes", false, "don't ask")
	name, err := one(fs, args)
	if err != nil {
		return err
	}

	if *purge && !*yes && !confirm("Delete "+name+" with its volumes and compose file?") {
		return errors.New("left " + name + " alone")
	}

	out, err := c.api.RemoveSite(ctx, &boxv1.RemoveSiteRequest{Site: name, Purge: *purge})
	if err != nil {
		return err
	}

	if *purge {
		fmt.Printf("%s: removed with its volumes\n", name)
	} else {
		fmt.Printf("%s: stopped. Its compose file is in %s on the server and its volumes are kept (--purge deletes them).\n", name, out.Kept)
	}

	return nil
}

func cmdConfig(ctx context.Context, c *conn, args []string) error {
	fs := flags("config", "config [key [value]]")
	rest, err := parse(fs, args)
	if err != nil {
		return err
	}

	switch len(rest) {
	case 0, 1:
		out, err := c.api.GetConfig(ctx, &boxv1.GetConfigRequest{})
		if err != nil {
			return err
		}

		for _, s := range out.Settings {
			switch {
			case len(rest) == 0:
				fmt.Printf("%-7s %s\n", s.Key, s.Value)
			case s.Key == rest[0]:
				fmt.Println(s.Value)
				return nil
			}
		}

		if len(rest) == 1 {
			return fmt.Errorf("unknown setting %q (one of %v)", rest[0], box.ConfigKeys)
		}

		return nil
	case 2:
	default:
		return wrongArgs(fs)
	}

	set, err := c.api.SetConfig(ctx, &boxv1.SetConfigRequest{Key: rest[0], Value: rest[1]})
	if err != nil {
		return err
	}

	defer printWarnings(set.Warnings)

	switch rest[0] {
	case "domain", "email":
		fmt.Println("Saved. The proxy picks it up within 30 seconds.")
		if rest[0] == "domain" {
			fmt.Printf("This client still uses %s. Rerun box setup with --domain to move it (it needs a DNS record).\n", c.profile.URL)
		}
	case "notify":
		if rest[1] != "" {
			fmt.Println("Saved, and sent a test notice.")
		}
	default:
		fmt.Println("Saved.")
	}

	return nil
}

func cmdHook(ctx context.Context, c *conn, args []string) error {
	fs := flags("hook", "hook url [site] | rotate <site>")
	rest, err := parse(fs, args)
	if err != nil {
		return err
	}

	switch {
	case len(rest) == 1 && rest[0] == "url":
		out, err := c.api.ListHooks(ctx, &boxv1.ListHooksRequest{})
		if err != nil {
			return err
		}

		for _, h := range out.Hooks {
			fmt.Println(h.Url)
		}

		return nil
	case len(rest) == 2 && rest[0] == "url":
		out, err := c.api.GetHook(ctx, &boxv1.GetHookRequest{Site: rest[1]})
		if err != nil {
			return err
		}

		printHook(out.Hook)
		return nil
	case len(rest) == 2 && rest[0] == "rotate":
		out, err := c.api.RotateHook(ctx, &boxv1.RotateHookRequest{Site: rest[1]})
		if err != nil {
			return err
		}

		fmt.Print("New secret. The old one no longer works; give CI or GitHub this one.\n\n")
		printHook(out.Hook)
		return nil
	}

	return wrongArgs(fs)
}

func printHook(h *boxv1.Hook) {
	fmt.Printf(`Webhook  %s
Secret   %s

From CI, after pushing the image (store the secret as BOX_HOOK_SECRET):
  curl -fsS -X POST -H "Authorization: Bearer $BOX_HOOK_SECRET" %s

As a GitHub webhook: payload URL above, content type application/json, the secret above,
and the "Registry packages" event.
`, h.Url, h.Secret, h.Url)
}

func cmdKey(ctx context.Context, c *conn, args []string) error {
	if len(args) != 1 || args[0] != "rotate" {
		fmt.Fprint(os.Stderr, "usage: box key rotate\n")
		return errUsage
	}

	key := box.NewSecret()
	if _, err := c.api.RotateKey(ctx, &boxv1.RotateKeyRequest{Key: key}); err != nil {
		return err
	}

	p := c.profile
	p.Key = key
	c.profiles.Put(c.name, p)
	if err := c.profiles.Save(); err != nil {
		return fmt.Errorf("the server has the new key %s, but saving it here failed: %w", key, err)
	}

	fmt.Printf("New key saved for %s. Any other copy of the old key no longer works.\n", c.name)
	return nil
}

func cmdDocker(ctx context.Context, c *conn, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, "usage: box docker check | login [registry]\n")
		return errUsage
	}

	switch args[0] {
	case "check":
		return dockerCheck(ctx, c)
	case "login":
		return dockerLogin(ctx, c, args[1:])
	}

	return fmt.Errorf("no docker command %q: check or login (box server docker changes daemon settings)", args[0])
}

func dockerCheck(ctx context.Context, c *conn) error {
	info, err := c.api.DockerInfo(ctx, &boxv1.DockerInfoRequest{})
	if err != nil {
		return err
	}

	network := "missing (the server creates it when it starts)"
	if info.Network {
		network = "present"
	}

	logins := "none (box docker login ghcr.io, for private images)"
	if len(info.Registries) > 0 {
		logins = strings.Join(info.Registries, ", ")
	}

	fmt.Printf("engine        %s\n", info.Engine)
	fmt.Printf("compose       %s\n", info.Compose)
	fmt.Printf("network %-5s %s\n", box.Network, network)
	fmt.Printf("proxy         %s\n", info.Proxy)
	for _, line := range info.Daemon {
		fmt.Println(line)
	}

	fmt.Printf("logins        %s\n\n", logins)
	fmt.Print(info.Disk)
	return nil
}

func dockerLogin(ctx context.Context, c *conn, args []string) error {
	fs := flags("docker login", "docker login [registry]")
	rest, err := parse(fs, args)
	if err != nil || len(rest) > 1 {
		return wrongArgs(fs)
	}

	registry := "docker.io"
	if len(rest) == 1 {
		registry = rest[0]
	}

	auth, err := askLogin(registry)
	if err != nil {
		return err
	}

	req := &boxv1.DockerLoginRequest{Registry: registry, Username: auth.Username, Password: auth.Password}
	if _, err := c.api.DockerLogin(ctx, req); err != nil {
		return err
	}

	fmt.Printf("The server is logged in to %s.\n", registry)
	return nil
}

func cmdPrune(ctx context.Context, c *conn, args []string) error {
	if len(args) > 0 {
		fmt.Fprint(os.Stderr, "usage: box prune\n")
		return errUsage
	}

	out, err := c.api.Prune(ctx, &boxv1.PruneRequest{})
	if err != nil {
		return err
	}

	fmt.Printf("reclaimed %s\n", out.Reclaimed)
	return nil
}

func cmdVersion(ctx context.Context, serverName string) error {
	fmt.Printf("client %s\n", versionString())
	c, err := dial(serverName)
	if err != nil {
		return nil
	}

	v, err := c.api.Version(ctx, &boxv1.VersionRequest{})
	if err != nil {
		fmt.Printf("server %s: %s\n", c.name, client.Message(err))
		return nil
	}

	fmt.Printf("server %s (%s)\n", v.Version, c.name)
	return nil
}

func versionString() string {
	if version != "" {
		return version
	}

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}

	rev, dirty := "", ""
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value[:min(12, len(s.Value))]
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "+dirty"
			}
		}
	}

	if rev == "" {
		return info.Main.Version
	}

	return rev + dirty
}
