# Helm Chart Publishing

## Version Management

Chart versions are managed by the `.version` file at the repository root. The `Chart.yaml` file contains a placeholder version (0.0.0-dev) that is overridden at package time using `helm package --version` and `--app-version` flags.

This ensures the `.version` file is the single source of truth for all component versions.

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
