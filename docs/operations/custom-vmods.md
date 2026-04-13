# Custom VMODs

VMODs are shared objects loaded into varnishd at startup. Varnish Gateway ships with the ghost VMOD (routing) plus the standard VMODs that come with varnishd — see the [Varnish VMOD documentation](https://varnish-cache.org/docs/trunk/reference/vmod.html) for the current set.

There are two ways to add your own: **bake them into a custom image** (recommended) or **stage them in via an init container**.

## Option 1: Custom gateway image

A one-liner Dockerfile on top of the stock image — VMODs are `.so` files under `/usr/lib/varnish/vmods/`:

```dockerfile
FROM ghcr.io/varnish/gateway-chaperone:vX.Y.Z
COPY libvmod_custom.so /usr/lib/varnish/vmods/
```

Pin the base image version. The VMOD ABI is version-specific — a module built against Varnish 7.0 will refuse to load on 8.0 — so when you [upgrade the gateway](upgrades.md), rebuild the custom image against the new base tag and roll both together.

### Applying the image

Two places can point at a custom image:

- **Per GatewayClass** — `GatewayClassParameters.spec.image`. Scoped to Gateways using that class.

  ```yaml
  apiVersion: gateway.varnish-software.com/v1alpha1
  kind: GatewayClassParameters
  metadata:
    name: custom-varnish
  spec:
    image: my-registry/varnish-gateway-custom:v1.2.3
  ```

- **Operator-wide default** — the `GATEWAY_IMAGE` env var on the operator Deployment. The Helm chart sets this from `chaperone.image.repository` + `chaperone.image.tag` (tag defaults to `appVersion`).

  ```yaml
  chaperone:
    image:
      repository: my-registry/varnish-gateway-custom
      tag: v1.2.3
  ```

`spec.image` wins over `GATEWAY_IMAGE` for Gateways using that class. Changing either updates the `varnish.io/infra-hash` annotation and triggers a rolling restart. The logging sidecar inherits the custom image unless `logging.image` is set explicitly.

## Option 2: Init container

If building a custom image isn't practical, copy `.so` files into a shared volume at pod startup:

```yaml
apiVersion: gateway.varnish-software.com/v1alpha1
kind: GatewayClassParameters
metadata:
  name: varnish-params
spec:
  varnishdExtraArgs:
    - "-p"
    - "vmod_path=/usr/lib/varnish/vmods:/extra-vmods"
  extraVolumes:
    - name: extra-vmods
      emptyDir: {}
  extraVolumeMounts:
    - name: extra-vmods
      mountPath: /extra-vmods
  extraInitContainers:
    - name: install-vmods
      image: my-registry/my-vmods:latest
      command: ["cp", "/vmods/libvmod_custom.so", "/dst/"]
      volumeMounts:
        - name: extra-vmods
          mountPath: /dst
```

The `emptyDir` is shared between the init container and varnishd; `vmod_path` extends varnishd's module search so it finds the staged files.

### Caveats

- **ABI pinning still applies** — the VMOD must match the varnishd version in the gateway image.
- **Pin the init container tag** (not `:latest`); pushing over a mutable tag won't take effect until the next pod restart from an unrelated cause.
- **Pull failures block pod startup** indefinitely.
- **Harder to debug** — a load error surfaces in varnishd's logs while the root cause (stale init image, wrong path) lives elsewhere.
