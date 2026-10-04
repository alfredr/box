# box

box runs websites on a single Linux server. Each site is a Docker Compose project, with Caddy handling domain routing and HTTPS. You manage sites from your computer with the `box` command.

The server pulls images from a registry. Build and publish your images before deploying them.

## Install the client

With Go 1.26.7 or later, run this from the repository root:

```sh
go install ./cmd/box
```

The binary is installed in `GOBIN`, or in `GOPATH/bin` if `GOBIN` is unset. Add that directory to your `PATH`.

## Set up a server

You need a Linux host and an SSH account that can access `/var/run/docker.sock`. Use root or a user in the `docker` group. Installing or restarting Docker also requires root or `sudo`, and the host must use systemd for those operations.

Point a DNS record such as `box.example.com` at the host. Allow incoming traffic on 80/tcp, 443/tcp and 443/udp, then run:

```sh
box setup root@203.0.113.7 --domain box.example.com --email you@example.com
```

Setup uses your system's `ssh`, so your SSH configuration, agent and jump hosts apply. It installs the server container, prepares Caddy and the shared `web` network, and saves a client profile with the server URL, API key and SSH login. The default server image is `ghcr.io/alfredr/box:latest`. Add `--login ghcr.io` if the image is private.

Docker Engine 25 or later is required. If setup cannot reach Docker, it offers to run the bundled installer. Check socket permissions and whether Docker is running before accepting the offer on an existing installation. `--install-docker` skips the prompt.

The [installer](internal/client/scripts/install-docker.sh) uses Docker's apt repositories on Debian and Ubuntu systems, Docker's dnf repositories on Fedora, RHEL, Rocky and Alma, and Arch's `docker` package on Arch. Docker then updates through the package manager. Compose is included in the box server image.

Setup also configures Docker log rotation with `max-size=10m` and `max-file=3`, enables live restore, and restarts Docker if these settings change. The log settings apply to new containers. Use `--keep-docker-config` to leave `/etc/docker/daemon.json` unchanged.

Once Caddy has obtained a certificate, check that the server is reachable:

```sh
box status
```

Setup waits for the box container's health check. That check does not confirm that Caddy or the certificate is ready.

## Create and edit sites

Point the site's domains at the server, then create it from a published image:

```sh
box new blog --image ghcr.io/alfredr/blog:latest --domain example.com --redirect www.example.com --deploy
```

This creates an `app` service and starts it immediately. Without `--deploy`, the default `auto` policy lets a later image check start the site. Use `--update manual` to require an explicit first deployment.

Caddy forwards requests to port 80 in the container by default; use `--port` if the application listens elsewhere. You can repeat `--domain` and `--redirect`. Redirects go to the first `--domain`.

To add a database, worker, volume or environment variable, edit the site's Compose file:

```sh
box edit blog
```

The client opens a local copy using `$VISUAL`, `$EDITOR` or `vi`, in that order of preference. The server validates the file before applying it. If the site fails to start, box attempts to restore and apply the old file.

Each service needs a literal `image:` value. Image references containing `${...}` are not supported. When an edit leaves an existing service's image reference unchanged, box resets the local reference to the container's current image before applying the file.

## Updates and rollback

box checks for new images every five minutes by default. Each service's policy determines what happens when its image changes:

| Policy | Behavior |
| --- | --- |
| `auto` | Pull and deploy the new image. |
| `manual` | Pull the new image and report that it is waiting for deployment. |
| `pinned` | Skip image checks and the pull step of `box deploy`. |

Set policies in the Compose file:

```yaml
x-box:
  update:
    app: auto
    worker: manual
    db: pinned
```

`box new` sets `app` to `auto` unless you pass `--update`. Services omitted from `x-box.update` use `manual`.

A check can start a new site after all non-pinned image pulls succeed. The site must have no containers, no recorded successful deployment, and at least one `auto` service with a local image. This first deployment starts the whole Compose project, including manual and pinned services. It can be triggered by polling, a webhook, or `box check`.

After the first deployment, checks update existing containers only. Use `box deploy` to create missing containers. A stopped container still counts as existing and can be started by an automatic update.

The `pinned` policy does not make an image tag immutable. Compose can still start or recreate the service using that tag.

| Command | Purpose |
| --- | --- |
| `box status [site]` | Show service state, pending updates, retained images and storage estimates. |
| `box check [site]` | Check for updates now. With no site, check all sites. |
| `box deploy <site> [service...]` | Pull and start the named services, or all services if none are named. Pinned services skip the pull step. |
| `box rollback <site> [--to N\|ID]` | Restore a retained image for each matching service. The default is the most recent earlier image. |
| `box logs <site> [service] -f` | Follow container logs. Omit `-f` to print recent logs and exit. |

A deployment waits up to 180 seconds for services to start and pass any health checks. If it fails, box attempts to restart them on their earlier images. There is no earlier image to restore on a first deployment. Rollback does not restore the Compose file or volume data.

Checks skip images rejected by a failed deployment or rollback. Run `box deploy` to retry one explicitly.

Use `box status <site>` to list each service's retained images, newest first. `box rollback <site> --to 2` selects the second entry independently for each service. An image ID prefix of at least four characters can select a specific image. The prefix must be unique within each matching service's history. An ambiguous prefix rejects the whole rollback before any service is changed. Prefix an entirely numeric image ID with `sha256:` to distinguish it from an index. Services without a matching entry are skipped.

Rollback moves the replaced image into history, subject to the same retention limits. This allows a later rollback to select it again.

To remove a site, run `box remove <site>`. This stops the site, keeps its volumes, and moves its directory to `/srv/box/removed/`. Add `--purge` to delete the directory and volumes managed by Compose. Both modes attempt to remove the site's retention tags and the image references in its Compose file. Saved files and volumes do not preserve the site's local images.

## Retain images and reclaim space

By default, box retains three earlier images per service, in addition to the current image. Set server defaults with:

```sh
box config keep 5
box config budget 2GB
```

`keep` accepts 0 through 100. Zero disables history. `budget` accepts a byte count or a size such as `500MB`, `2GB`, or `2GiB`; `off` disables the byte limit. Decimal units use powers of 1000, and binary units use powers of 1024. The default budget is `off`.

Override either setting for individual services in the Compose file:

```yaml
x-box:
  keep:
    app: 5
    db: 0
  budget:
    app: 2GB
    db: off
```

Omitted values inherit the server setting. Both limits apply: box drops the oldest history entries first until the count and estimated size fit. The current image is always retained, even if it exceeds the budget. Rejected images are outside these retention limits.

Limits are applied after successful deployments, rollbacks and edits. Changing a server setting does not immediately trim existing history. Run `box prune` to apply the current limits to all sites and prune unused dangling images.

The budget counts each distinct retained image at its full size from Docker image inspection. Shared layers count toward each image, so box may trim history before its physical disk usage reaches the budget. Server totals can also count images shared across services more than once. Budgets do not cover volumes, build cache, container writes, or temporary storage used during pulls. Capacity warnings reserve room for an incoming image as large as the current one.

Free space is measured on a read-only mount of the daemon's image storage directory. Setup detects the path for classic filesystem drivers. For the containerd image store, pass the host path explicitly:

```sh
box setup root@203.0.113.7 --domain box.example.com --image-store /var/lib/containerd
```

Use the directory configured for your host. Docker's `data-root` does not set the storage directory for an external containerd daemon; see [Docker's storage configuration](https://docs.docker.com/engine/daemon/). Setup and server upgrades preserve an existing image-storage mount. If no path is known or the mount cannot be read, status reports free space as unavailable and skips disk-capacity comparisons. Image retention and budget warnings still work.

For a manually installed server, mount the image storage directory read-only and set `BOX_IMAGE_STORE` to its path inside the server container.

box tags current images as `box-keep/<site>:<service>`, history images with an added short image ID, and failed-deployment images with `-rejected`. Retention cleanup removes tags without forcing deletion of images in use. Older `-previous` tags are imported into history and remain managed until their images are restored or trimmed.

Automatic cleanup runs at server startup, after deploy, rollback, edit and remove commands, and after checks that deploy images or record new manual updates. It prunes unused dangling images across the Docker daemon, including images unrelated to box. Startup also attempts to remove older tags from the proxy image repository. Volumes and build cache are not pruned.

Set `BOX_PRUNE=off` in the server container's environment to disable automatic image pruning and startup proxy-image cleanup. Retention limits and site-removal cleanup still apply. Explicit `box prune` still runs.

## Trigger updates from CI

Get the site's webhook URL and secret:

```sh
box hook url blog
```

Store the secret as `BOX_HOOK_SECRET` in CI. After pushing an image, request a check:

```sh
curl -fsS -X POST \
  -H "Authorization: Bearer $BOX_HOOK_SECRET" \
  https://box.example.com/deploy/blog
```

The webhook runs the same check as `box check`, following the site's update policies. A `202 Accepted` response means the check was queued. Use `box status blog` to see the result.

For a GitHub webhook, use the same URL and secret, set the content type to `application/json`, and select the "Registry packages" event. box verifies the `X-Hub-Signature-256` signature.

A webhook secret permits checks for that site only. Run `box hook rotate <site>` to replace it; the old secret stops working immediately. Polling continues independently, so webhooks are optional.

## Configure the server

`box config` lists the settings. Pass a key to read one value, or a key and value to change it:

```sh
box config poll
box config poll 10m
box config notify https://ntfy.sh/your-topic
```

| Key | Accepted value |
| --- | --- |
| `domain` | The server's DNS name for the API and webhooks. |
| `email` | An ACME account email, or an empty string to clear it. |
| `notify` | An HTTP or HTTPS URL for plain-text POST notifications. An empty string disables them. |
| `poll` | A duration of at least `1m`, or `off`. The default is `5m`. |
| `keep` | Earlier images per service, from 0 to 100. The default is 3. |
| `budget` | Estimated image storage per service, such as `2GB`, or `off`. The default is `off`. |

A notification URL is saved only after a test notification succeeds. Changing `domain` updates Caddy but leaves the local client's URL unchanged. To change both, rerun setup with the same profile name and the new domain.

## Maintain the server

| Command | Purpose |
| --- | --- |
| `box docker check` | Inspect Docker and Compose versions, daemon settings, registry logins and disk usage. |
| `box docker login ghcr.io` | Save a registry login for private site images. |
| `box prune` | Trim image history to the current limits and prune unused dangling images. |
| `box server upgrade` | Pull the server image and replace the box container. Use `--image` to change the image and `--login` if authentication is required. |
| `box server logs -f` | Follow the box container's logs. |
| `box server docker --log-size 10m --restart` | Change Docker's log size setting and restart the daemon. |

The `box server` commands use the Docker API over SSH. Site commands use the box API over HTTPS.

Rerunning setup keeps site files and existing settings, except for the domain and any email supplied on the command line. It reuses the key in the selected local profile. Setup without a saved key installs a new key, so other clients must be updated. Supplying `--login` replaces the saved registry configuration with that one login.

## Client profiles and keys

Setup names the profile after the server domain unless you supply `--server NAME`. The first saved profile becomes the default. List profiles with `box servers`, and select one with `box --server NAME <command>` or `BOX_SERVER`.

Profiles are saved in:

- `~/Library/Application Support/box/servers.json` on macOS.
- `$XDG_CONFIG_HOME/box/servers.json` on Linux, or `~/.config/box/servers.json` when `XDG_CONFIG_HOME` is unset.

Set `BOX_CONFIG` to use a different file. Profiles contain API keys and are written with mode `0600`. Anyone with an API key can control that server's Docker daemon.

Setup installs a randomly generated 256-bit key over SSH. API requests use HMAC-SHA256 signatures covering the method, request URI, timestamp, nonce and body hash. The server rejects requests more than five minutes from its clock and repeated nonces while they remain valid. The nonce cache is in memory and is cleared on restart.

`box key rotate` sends a new key over HTTPS in a request signed with the current key, then updates the local profile. Other copies of the old key stop working.

## Server files and API

The box server runs in a container with the host's Docker socket mounted. It listens on `127.0.0.1:9311`, behind Caddy. The [Connect](https://connectrpc.com) API is defined in [proto/box/v1/box.proto](proto/box/v1/box.proto).

| Path | Contents |
| --- | --- |
| `/srv/box/box.json` | Server settings. |
| `/srv/box/sites/<name>/compose.yml` | A site's Compose file. |
| `/srv/box/proxy/` | Caddy's Compose file and base Caddyfile. |
| `/srv/box/secrets/` | The API key and webhook secrets, written with mode `0600`. |
| `/srv/box/state/<name>.json` | Deployment records and image IDs. |
| `/srv/box/docker/` | Registry credentials for the server's Docker CLI. |
| `/srv/box/removed/` | Site directories kept after removal. |

`/srv/box` is mounted at the same path inside the server container because the host's Docker daemon resolves the bind mount paths in site Compose files.

## Development

Run the unit tests and vet checks from the repository root:

```sh
go test ./...
go vet ./...
```

After changing the API definition, regenerate the code:

```sh
go generate .
```

This runs Buf lint and writes generated code to `internal/gen`. Buf and the generators are module tools whose versions are recorded in `go.mod`.

The end-to-end tests need local Docker on Linux or macOS, a Unix Docker socket, and a temporary directory Docker can bind mount:

```sh
go test -tags e2e ./e2e
```

`TestServer` builds and installs the server, then tests deployment, checks, rollback, edits, failure recovery, webhooks, reinstallation, removal and key rotation through the client. Set `BOX_E2E_IMAGE` to use an existing server image.

Set `BOX_E2E_INSTALL=1` to also run `TestInstallScript`. It installs Docker packages in Debian 12 and 13, Ubuntu 24.04, Fedora and Rocky Linux 9 containers, with `systemctl` stubbed out.

The tests leave the host daemon's settings unchanged, disable automatic pruning, and attempt to remove their temporary resources. Cached base images remain. They do not test SSH forwarding, installation or daemon restarts on a real host, the host-network proxy, or certificate issuance.

[The image workflow](.github/workflows/image.yml) runs vet and unit tests before publishing Linux amd64 and arm64 images on pushes to `main` or tags matching `v*`. It publishes `latest` and commit-SHA tags under `ghcr.io/${{ github.repository }}`, which is `ghcr.io/alfredr/box` for this repository.
