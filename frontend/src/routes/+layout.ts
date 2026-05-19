// CSR-only. SSR is disabled because adapter-static prerenders at build
// time and we want all interactive routes (admin, event pages) to fetch
// from the live Go backend at runtime, not against a frozen snapshot.
// Re-enable per-route with `export const prerender = true` for any page
// where we can afford a build-time render (e.g., marketing landing).
export const ssr = false;
export const prerender = false;
