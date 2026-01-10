# User VCL Deployment Flow

This document describes how user-provided VCL flows through the system from ConfigMap to running Varnish.

## Setup (one-time)

User creates the following resources:

```
├── Gateway (defines the entry point)
├── HTTPRoute(s) (defines routing rules)
├── ConfigMap "my-vcl" (contains user VCL)
└── VarnishConfig (links Gateway to ConfigMap)
```

Example resources:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: my-vcl
data:
  main.vcl: |
    sub vcl_recv {
      # user's custom logic
    }
---
apiVersion: gateway.varnish-software.com/v1alpha1
kind: VarnishConfig
metadata:
  name: my-gateway-config
spec:
  gatewayRef:
    name: my-gateway
  vclConfigMapRef:
    name: my-vcl
    key: main.vcl
```

## Flow: User Updates VCL ConfigMap

```
1. User: kubectl apply -f my-vcl-configmap.yaml
   └── ConfigMap "my-vcl" updated in etcd

2. varnishconfig_controller (watches ConfigMaps referenced by VarnishConfig)
   └── Reconcile triggered
   └── Reads ConfigMap "my-vcl" → gets user VCL string

3. varnishconfig_controller calls into vcl package
   └── Fetches all HTTPRoutes attached to the Gateway
   └── Calls generator.Generate(routes) → generated VCL string
   └── Calls merge.Merge(generatedVCL, userVCL) → concatenated output

4. varnishconfig_controller writes output ConfigMap
   └── ConfigMap "my-gateway-vcl" (owned by Gateway) updated
   └── Contains: main.vcl = concatenated VCL

5. Sidecar (running in Varnish pod, watches mounted ConfigMap volume)
   └── fsnotify detects file change on /var/run/varnish/main.vcl
   └── Reads new VCL content

6. Sidecar calls varnishadm
   └── vcl.load vcl_20240115_143022 /var/run/varnish/main.vcl
   └── If success: vcl.use vcl_20240115_143022
   └── If failure: log error, keep old VCL, expose metric

7. Varnish
   └── Compiles new VCL
   └── Switches to new VCL (zero-downtime, in-flight requests finish on old VCL)
   └── Old VCL discarded after cooldown
```

## Flow: HTTPRoute Changes (routing update)

```
1. User: kubectl apply -f my-httproute.yaml
   └── HTTPRoute updated in etcd

2. httproute_controller
   └── Reconcile triggered
   └── Validates route, sets status conditions
   └── Triggers VCL regeneration for affected Gateway

3. Same as steps 3-7 above
   └── New routing VCL generated
   └── Merged with existing user VCL
   └── Pushed to Varnish
```

## Key Details

### Two ConfigMaps Involved

| ConfigMap | Managed by | Contains |
|-----------|------------|----------|
| `my-vcl` | User | User's VCL |
| `my-gateway-vcl` | Operator | Merged output (generated + user) |

### Sidecar Volume Mount

The sidecar mounts the **output** ConfigMap, not the user's input:

```yaml
volumes:
- name: vcl
  configMap:
    name: my-gateway-vcl  # operator-managed output
```

### Failure Handling

| Failure Point | Behavior |
|---------------|----------|
| Parse error in user VCL | Controller sets VarnishConfig status to error, does not update output ConfigMap |
| Varnish compile error | Sidecar logs error, keeps running old VCL, exposes `varnish_vcl_load_errors_total` metric |

### VCL Merging

VCL supports multiple definitions of the same subroutine (they get concatenated). The merge is simple concatenation:

1. Generated VCL (imports, vcl_init, routing, vcl_backend_fetch calling routing)
2. User VCL (appended after)

If user defines `vcl_backend_fetch`, their code runs after the gateway routing call.
