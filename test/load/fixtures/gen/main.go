// Fixture generator: emits a large HTTPRoute set plus a matching routes.json
// that k6 can consume to know the expected backend for each (host, path)
// pair. Used for C02/C03 at scale — the default 4-route fixture is too
// small to exercise "hundreds of HTTPRoutes" behaviour.
//
// Output layout (into -out dir):
//   routes.yaml    Gateway + HTTPRoutes, kubectl-applyable.
//   routes.json    [{host, path, expService}] — mount into k6 as a ConfigMap.
//
// The default Gateway fixture and the generator share the "load" gateway
// name and the "varnish-load" namespace — the generator intentionally
// omits the Gateway resource by default so it can layer on top of the
// existing fixture without conflict. Pass -gateway to emit one.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type route struct {
	Host       string `json:"host"`
	Path       string `json:"path"`
	ExpService string `json:"expService"`
}

func main() {
	var (
		outDir   = flag.String("out", "test/load/fixtures/generated", "output directory")
		vhosts   = flag.Int("vhosts", 50, "number of vhosts")
		paths    = flag.Int("paths", 10, "paths per vhost (total routes = vhosts * paths)")
		ns       = flag.String("ns", "varnish-load", "namespace")
		gwName   = flag.String("gateway", "load", "Gateway name to parentRef")
		emitGW   = flag.Bool("emit-gateway", false, "also emit the Gateway resource (default: reuse existing)")
		services = flag.String("services", "echo-a,echo-b", "comma-separated backend services (round-robin)")
		hostTmpl = flag.String("host-template", "h%d.load.local", "printf-style hostname template")
		port     = flag.Int("port", 8080, "backend port")
	)
	flag.Parse()

	svcs := strings.Split(*services, ",")
	if len(svcs) == 0 || svcs[0] == "" {
		fmt.Fprintln(os.Stderr, "at least one service required")
		os.Exit(2)
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir: %v\n", err)
		os.Exit(1)
	}

	var yaml strings.Builder
	var table []route

	if *emitGW {
		fmt.Fprintf(&yaml, gatewayTmpl, *gwName, *ns)
	}

	for i := 0; i < *vhosts; i++ {
		host := fmt.Sprintf(*hostTmpl, i)
		for j := 0; j < *paths; j++ {
			path := "/"
			if j > 0 {
				path = fmt.Sprintf("/p%d", j)
			}
			svc := svcs[(i+j)%len(svcs)]
			name := fmt.Sprintf("gen-h%d-p%d", i, j)

			fmt.Fprintf(&yaml, routeTmpl, name, *ns, *gwName, host, path, svc, *port)
			table = append(table, route{Host: host, Path: path, ExpService: svc})
		}
	}

	if err := os.WriteFile(filepath.Join(*outDir, "routes.yaml"), []byte(yaml.String()), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write yaml: %v\n", err)
		os.Exit(1)
	}
	jsonBody, err := json.MarshalIndent(map[string]any{"routes": table}, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal routes.json: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(filepath.Join(*outDir, "routes.json"), jsonBody, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write json: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "wrote %d HTTPRoutes (%d vhosts × %d paths) to %s\n",
		*vhosts**paths, *vhosts, *paths, *outDir)
}

const gatewayTmpl = `---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: %s
  namespace: %s
spec:
  gatewayClassName: varnish
  listeners:
    - name: http
      protocol: HTTP
      port: 80
      allowedRoutes:
        namespaces: { from: Same }
`

const routeTmpl = `---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: %s
  namespace: %s
  labels:
    fixture: generated
spec:
  parentRefs: [{ name: %s }]
  hostnames: ["%s"]
  rules:
    - matches: [{ path: { type: PathPrefix, value: "%s" } }]
      backendRefs:
        - name: %s
          port: %d
`
