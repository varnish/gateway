# Helm Chart Publishing

## Version Management

Chart versions are managed by the `.version` file at the repository root, which is the single source of truth for all component versions. `go tool bump` rewrites `charts/varnish-gateway/Chart.yaml` on every bump: `version` is strict SemVer without a `v` prefix (a Helm requirement) and `appVersion` carries the same value.

Docker image tags in ghcr.io carry the `v` prefix (e.g. `gateway-operator:v0.22.0`). The chart's image helpers in `templates/_helpers.tpl` normalize the default tag derived from `appVersion` to that form, so rendering or installing from a repo checkout produces pullable images. At package time, CI additionally passes `helm package --version`/`--app-version` so the published chart embeds the exact release version.

## Automated Publishing

Charts are automatically published to `ghcr.io/varnish/charts/varnish-gateway` on version tags via GitHub Actions.

**To publish a new version:**

```bash
go tool bump -patch  # or -minor/-major
git commit -am "bump version to $(cat .version)"
git tag $(cat .version)
git push origin main --tags
```

GitHub Actions will build Docker images and publish the Helm chart with the version from `.version`.

## Manual Publishing

```bash
echo $GITHUB_TOKEN | helm registry login ghcr.io -u USERNAME --password-stdin
make helm-push
```

## Installation

Users install from the OCI registry:

```bash
helm install varnish-gateway \
  oci://ghcr.io/varnish/charts/varnish-gateway \
  --version 0.7.2
```
