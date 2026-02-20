const CACHE_NAME = 'homecontrol-v5';
const FETCH_TIMEOUT_MS = 5000;
const OFFLINE_URL = '/static/offline.html';

// Assets to pre-cache for offline support
const PRECACHE = [
  OFFLINE_URL,
  '/static/inter.css',
  '/static/lucide.css',
  '/static/fonts/inter-400.ttf',
  '/static/fonts/inter-500.ttf',
  '/static/fonts/inter-600.ttf',
  '/static/fonts/inter-700.ttf',
  '/static/fonts/lucide.woff2',
];

self.addEventListener('install', (event) => {
  event.waitUntil(
    caches.open(CACHE_NAME).then((cache) => cache.addAll(PRECACHE))
  );
  self.skipWaiting();
});

self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches.keys().then((keys) =>
      Promise.all(keys.filter((k) => k !== CACHE_NAME).map((k) => caches.delete(k)))
    )
  );
  self.clients.claim();
});

// Fetch with a timeout - if the server doesn't respond in time, reject.
// This handles firewall-blocked connections that hang instead of failing.
function fetchWithTimeout(request, timeoutMs) {
  const controller = new AbortController();
  const timeoutId = setTimeout(() => controller.abort(), timeoutMs);

  return fetch(request, { signal: controller.signal })
    .then((response) => {
      clearTimeout(timeoutId);
      return response;
    })
    .catch((err) => {
      clearTimeout(timeoutId);
      throw err;
    });
}

self.addEventListener('fetch', (event) => {
  // Only handle navigation requests (HTML pages) for offline fallback
  if (event.request.mode === 'navigate') {
    event.respondWith(
      fetchWithTimeout(event.request, FETCH_TIMEOUT_MS).then((response) => {
        // If the server (or a reverse proxy like Caddy) returns a server
        // error, treat it the same as a network failure — show the offline
        // skeleton page instead of a raw error.
        if (response.status >= 500) {
          return caches.match(OFFLINE_URL) || response;
        }
        return response;
      }).catch(() => caches.match(OFFLINE_URL))
    );
    return;
  }

  // For static assets: try cache first, then network
  if (event.request.url.includes('/static/')) {
    event.respondWith(
      caches.match(event.request).then((cached) => cached || fetch(event.request))
    );
    return;
  }
});
