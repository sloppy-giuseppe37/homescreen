const CACHE_NAME = 'homecontrol-v2';
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

self.addEventListener('fetch', (event) => {
  // Only handle navigation requests (HTML pages) for offline fallback
  if (event.request.mode === 'navigate') {
    event.respondWith(
      fetch(event.request).catch(() => caches.match(OFFLINE_URL))
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
