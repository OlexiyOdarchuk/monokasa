// Thin fetch wrapper for the /api/admin/* endpoints. Cookies travel
// implicitly (same-origin), so there's no token plumbing to do here.
//
// On a 401 from any endpoint we hard-navigate to /admin/login. That's
// either an expired session or the user never had one — either way the
// SPA can't recover, the login page can.
//
// Body shape for errors matches what the Go side returns:
//   { "error": "<machine_code>", "detail": "<human>" }
// Wrapped as ApiError so callers can `instanceof` check and render
// detail to the user.

export class ApiError extends Error {
	readonly status: number;
	readonly code: string;
	readonly detail: string;

	constructor(status: number, code: string, detail: string) {
		super(detail || code);
		this.status = status;
		this.code = code;
		this.detail = detail;
	}
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
	const init: RequestInit = {
		method,
		credentials: 'same-origin',
		headers: {
			Accept: 'application/json'
		}
	};
	if (body !== undefined) {
		init.body = JSON.stringify(body);
		(init.headers as Record<string, string>)['Content-Type'] = 'application/json';
	}

	const r = await fetch(path, init);

	if (r.status === 401) {
		// No session → bounce to login. Use full reload so any in-memory
		// state is wiped clean.
		window.location.href = '/admin/login';
		throw new ApiError(401, 'unauthorized', 'session expired');
	}

	if (r.status === 204) {
		// No body — fine, return null-ish. Callers that expect a value
		// shouldn't use 204 endpoints; type system enforces this for new code.
		return undefined as T;
	}

	const contentType = r.headers.get('content-type') ?? '';
	const isJson = contentType.includes('application/json');

	if (!r.ok) {
		if (isJson) {
			const err = (await r.json()) as { error?: string; detail?: string };
			throw new ApiError(r.status, err.error ?? 'unknown', err.detail ?? '');
		}
		const text = await r.text();
		throw new ApiError(r.status, 'http_' + r.status, text || r.statusText);
	}

	if (!isJson) {
		// Endpoints we use always return JSON unless 204 — surface this
		// as a hard error so a wrong path doesn't silently corrupt state.
		throw new ApiError(r.status, 'unexpected_content_type', contentType);
	}
	return (await r.json()) as T;
}

export const api = {
	get: <T>(path: string) => request<T>('GET', path),
	post: <T>(path: string, body?: unknown) => request<T>('POST', path, body),
	put: <T>(path: string, body?: unknown) => request<T>('PUT', path, body),
	patch: <T>(path: string, body?: unknown) => request<T>('PATCH', path, body),
	del: <T>(path: string) => request<T>('DELETE', path),
	req: <T>(method: string, path: string, body?: unknown) => request<T>(method, path, body)
};

// ---- typed response shapes (mirror internal/admin/admin.go) ----

export interface Me {
	id: number;
	email: string;
	name: string;
}

export interface Stats {
	total: number;
	sold: number;
	held: number;
	free: number;
	revenue_kopecks: number;
}

export type ShowKind = 'seated' | 'ga';

export interface Show {
	id: number;
	slug: string;
	title: string;
	venue: string;
	starts_at: string; // RFC3339
	description: string;
	poster_url: string;
	created_at: string;
	archived_at?: string | null;
	kind: ShowKind;
	ga_capacity: number;
	stats?: Stats | null;
}

export interface Seat {
	id: number;
	show_id: number;
	row: number;
	col: number;
	x: number;
	y: number;
	label: string;
	category: string;
	price_kopecks: number;
	sellable: boolean;
}

export interface Reservation {
	id: number;
	code: string;
	buyer_name: string;
	tg_user_id: number;
	created_at: string;
	expires_at: string;
	confirmed_at?: string | null;
	cancelled_at?: string | null;
	// Set when admin manually marked the order as refunded in monobank.
	// Independent of cancelled_at — refund-mark is pure bookkeeping.
	refunded_at?: string | null;
	status: 'paid' | 'held' | 'expired' | 'cancelled';
}

export interface SeatBrief {
	id: number;
	row: number;
	col: number;
	label: string;
	category: string;
	price_kopecks: number;
}

export interface Guest {
	reservation: Reservation;
	seat: SeatBrief;
}

// --- buyer "Мої квитки" (magic-link auth) ---

export interface BuyerTicketItem {
	reservation_id: number;
	row: number;
	col: number;
	label?: string;
	attendee_name?: string;
	price_kopecks: number;
	cancelled_at?: string | null;
	refunded_at?: string | null;
	qr_payload?: string;
	used_at?: string | null;
}

export interface BuyerTicketShow {
	slug: string;
	title: string;
	venue: string;
	starts_at: string;
	poster_url?: string;
}

export interface BuyerTicket {
	order_id: number;
	order_code: string;
	order_status: 'paid' | 'held' | 'expired' | 'cancelled';
	total_kopecks: number;
	created_at: string;
	show: BuyerTicketShow;
	items: BuyerTicketItem[];
}

export interface DailySales {
	date: string; // YYYY-MM-DD
	tickets: number;
	revenue_kopecks: number;
}

export interface PerShowStats {
	id: number;
	slug: string;
	title: string;
	total: number;
	sold: number;
	held: number;
	free: number;
	revenue_kopecks: number;
}

export interface Analytics {
	from: string;
	to: string;
	days: number;
	daily_sales: DailySales[];
	total_tickets: number;
	total_revenue_kopecks: number;
	orders_created: number;
	orders_paid: number;
	conversion_percent: number;
	per_show: PerShowStats[];
}

export interface Organizer {
	name: string;
	bio: string;
	contact_email: string;
	phone: string;
	website_url: string;
	telegram_url: string;
	instagram_url: string;
	facebook_url: string;
	logo_url: string;
	updated_at?: string;
}

export interface AuditEntry {
	id: number;
	actor_user_id: number;
	actor_email: string;
	action: string;
	target: string;
	details?: unknown;
	created_at: string;
}

export interface CreateShowInput {
	title: string;
	venue: string;
	starts_at: string;
	// For seated shows: rows×cols grid. For GA shows: ignored — set
	// ga_capacity instead. The backend validates which group is required
	// based on `kind`.
	rows: number;
	cols: number;
	price_kopecks: number;
	kind?: ShowKind;
	ga_capacity?: number;
}

export interface UpdateShowInput {
	title?: string;
	venue?: string;
	starts_at?: string;
	description?: string;
	poster_url?: string;
}

// ---- public-side (anonymous buyer) shapes ----

export interface PublicShowSummary {
	slug: string;
	title: string;
	venue: string;
	starts_at: string;
	description: string;
	poster_url: string;
	seats_free: number;
	seats_total: number;
	kind: ShowKind;
}


export interface PublicSeat {
	id: number;
	row: number;
	col: number;
	x: number;
	y: number;
	label: string;
	category: string;
	price_kopecks: number;
	sellable: boolean;
	taken: boolean;
}

export interface PublicCategory {
	name: string;
	color: string;
	price_kopecks: number;
}

export interface PublicShow {
	slug: string;
	title: string;
	venue: string;
	starts_at: string;
	description: string;
	poster_url: string;
	seats: PublicSeat[];
	categories: PublicCategory[];
	kind: ShowKind;
	// GA-only fields. Absent / zero for seated shows.
	ga_capacity?: number;
	ga_price_kopecks?: number;
	ga_free?: number;
}

export interface SeatCategory {
	id: number;
	name: string;
	color: string;
	price_kopecks: number;
	sort_order: number;
}

export interface CreateReservationInput {
	slug: string;
	seat_id: number;
	buyer_name: string;
	buyer_email: string;
}

export interface ReservationResponse {
	code: string;
	expires_at: string;
	pay_url: string;
	seat: PublicSeat;
	buyer_name: string;
	buyer_email: string;
	// Present when the server has BOT_USERNAME configured. The frontend
	// shows a "Connect Telegram" button that deep-links into the bot's
	// /start handler, attaching this reservation to that chat.
	tg_deep_link?: string;
}

export type ReservationStatus = 'held' | 'paid' | 'expired' | 'cancelled';

export interface ReservationStatusResponse {
	status: ReservationStatus;
}

// --- multi-seat order endpoint (POST /api/public/orders) ---
//
// One order ties together N reservations under a single 8-char payment
// code. The seat map polls /reservations/{code}/status with the order
// code; status flips once monobank confirms the total transfer.

export interface CreateOrderInput {
	slug: string;
	// For seated shows: list of seat ids. For GA shows: leave empty and
	// pass `quantity` instead. Backend rejects requests that mix the two.
	seat_ids: number[];
	buyer_name: string;
	buyer_email: string;
	// Optional per-ticket attendee names. If present, must align 1:1 with
	// seat_ids — empty strings inside the slice fall back to buyer_name
	// at render time. Omit (or pass an empty slice) to use buyer_name for
	// every ticket in the order. Not supported for GA orders.
	attendee_names?: string[];
	// GA-only: how many tickets to allocate. Server picks from the pool.
	quantity?: number;
}

export interface OrderItem {
	seat: PublicSeat;
}

export interface CreateOrderResponse {
	code: string;
	expires_at: string;
	pay_url: string;
	total_kopecks: number;
	items: OrderItem[];
	buyer_name: string;
	buyer_email: string;
	tg_deep_link?: string;
}

// publicApi mirrors api.* but does NOT auto-redirect to /admin/login on
// 401 — the public buyer flow shouldn't even reach a 401, and bouncing
// an anonymous visitor to an admin page would be confusing. Treats all
// 4xx/5xx as plain ApiError instances.
//
// Uses same-origin credentials so the buyer's magic-link session cookie
// (monokasa_buyer) ride along to /api/public/my* — without it the
// frontend gets 401 on whoami and re-renders the login form even
// though the cookie was just set. Same-origin is safe: cookies only
// go to our own domain.
export const publicApi = {
	async get<T>(path: string): Promise<T> {
		const r = await fetch(path, {
			credentials: 'same-origin',
			headers: { Accept: 'application/json' }
		});
		return handlePublicResponse<T>(r);
	},
	async post<T>(path: string, body: unknown): Promise<T> {
		const r = await fetch(path, {
			method: 'POST',
			credentials: 'same-origin',
			headers: {
				Accept: 'application/json',
				'Content-Type': 'application/json'
			},
			body: JSON.stringify(body)
		});
		return handlePublicResponse<T>(r);
	}
};

async function handlePublicResponse<T>(r: Response): Promise<T> {
	if (r.status === 204) return undefined as T;
	const isJson = (r.headers.get('content-type') ?? '').includes('application/json');
	if (!r.ok) {
		if (isJson) {
			const err = (await r.json()) as { error?: string; detail?: string };
			throw new ApiError(r.status, err.error ?? 'unknown', err.detail ?? '');
		}
		throw new ApiError(r.status, 'http_' + r.status, await r.text());
	}
	return (await r.json()) as T;
}
