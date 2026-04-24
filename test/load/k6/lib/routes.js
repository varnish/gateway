// Route table shared between k6 and the fixture. The small default set
// below matches test/load/fixtures/routes.yaml. For large-fixture runs
// (test/load/fixtures/gen), mount the generated routes.json at
// /k6/routes.json via ConfigMap and k6 will pick it up automatically.

const DEFAULT_ROUTES = [
  { host: 'a.load.local',     path: '/',    expService: 'echo-a' },
  { host: 'a.load.local',     path: '/v2',  expService: 'echo-a' },
  { host: 'b.load.local',     path: '/',    expService: 'echo-b' },
  { host: 'b.load.local',     path: '/api', expService: 'echo-b' },
  { host: 'mixed.load.local', path: '/a',   expService: 'echo-a' },
  { host: 'mixed.load.local', path: '/b',   expService: 'echo-b' },
];

function loadGenerated() {
  // k6's open() is init-only and throws if the path doesn't exist.
  // Attempt only when the path is configured; fall back silently.
  const path = __ENV.ROUTES_JSON_PATH;
  if (!path) return null;
  try {
    const body = open(path);
    const data = JSON.parse(body);
    if (Array.isArray(data.routes) && data.routes.length > 0) {
      return data.routes;
    }
  } catch (_) {
    // fall through
  }
  return null;
}

export const ROUTES = loadGenerated() || DEFAULT_ROUTES;

export function pickRoute() {
  return ROUTES[Math.floor(Math.random() * ROUTES.length)];
}
