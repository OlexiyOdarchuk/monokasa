// URL safety helpers for user-supplied values.
//
// Admin-controlled fields like organizer.logo_url, show.poster_url, and
// even the server-built pay_url end up in `src=` / `href=` attributes
// where a malicious or accidentally-misconfigured string with a
// `javascript:` / `data:` scheme would execute on click.
//
// Treat every URL coming from the wire as untrusted at the boundary
// and run it through these helpers before binding to the DOM.

const ABSOLUTE_OK = ['https://', 'http://']; // http for local dev / cloudflared tunnels
const ABSOLUTE_BAD = ['javascript:', 'data:', 'vbscript:', 'file:'];

/**
 * safeHref returns a sanitised URL for `<a href>` or empty when the
 * input can't be trusted. Accepts:
 *   - absolute https:// or http:// URLs
 *   - same-origin paths starting with `/`
 *   - mailto: and tel: links (we render them deliberately on /about)
 *
 * Rejects javascript:, data:, vbscript:, file:, and anything ambiguous.
 * Bare hosts (`example.com/foo`) get https:// prepended.
 */
export function safeHref(url: string | undefined | null): string {
	const u = (url ?? '').trim();
	if (!u) return '';
	const lower = u.toLowerCase();
	if (ABSOLUTE_BAD.some((s) => lower.startsWith(s))) return '';
	if (u.startsWith('/') || u.startsWith('#')) return u;
	if (lower.startsWith('mailto:') || lower.startsWith('tel:')) return u;
	if (ABSOLUTE_OK.some((s) => lower.startsWith(s))) return u;
	// Bare host like `example.com/path` — assume https.
	// Reject anything with a colon BEFORE the first slash (likely an
	// unknown scheme we don't want to allow blindly).
	const firstSlash = u.indexOf('/');
	const firstColon = u.indexOf(':');
	if (firstColon !== -1 && (firstSlash === -1 || firstColon < firstSlash)) {
		return '';
	}
	return 'https://' + u;
}

/**
 * safeImageSrc is safeHref minus mailto:/tel: (no sense as image
 * sources). Same scheme allow-list otherwise.
 */
export function safeImageSrc(url: string | undefined | null): string {
	const u = (url ?? '').trim();
	if (!u) return '';
	const lower = u.toLowerCase();
	if (ABSOLUTE_BAD.some((s) => lower.startsWith(s))) return '';
	if (u.startsWith('/')) return u;
	if (ABSOLUTE_OK.some((s) => lower.startsWith(s))) return u;
	const firstSlash = u.indexOf('/');
	const firstColon = u.indexOf(':');
	if (firstColon !== -1 && (firstSlash === -1 || firstColon < firstSlash)) {
		return '';
	}
	return 'https://' + u;
}
