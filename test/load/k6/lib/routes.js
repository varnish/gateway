// Route table shared between k6 and (conceptually) the fixture. Keep this
// in sync with test/load/fixtures/routes.yaml — k6 uses it to know which
// service each (host, path) is expected to hit, and the fixture creates
// the HTTPRoutes that make it so.

export const ROUTES = [
  { host: 'a.load.local', path: '/', expService: 'echo-a' },
  { host: 'a.load.local', path: '/v2',   expService: 'echo-a' },
  { host: 'b.load.local', path: '/',     expService: 'echo-b' },
  { host: 'b.load.local', path: '/api',  expService: 'echo-b' },
  { host: 'mixed.load.local', path: '/a', expService: 'echo-a' },
  { host: 'mixed.load.local', path: '/b', expService: 'echo-b' },
];

export function pickRoute() {
  return ROUTES[Math.floor(Math.random() * ROUTES.length)];
}
