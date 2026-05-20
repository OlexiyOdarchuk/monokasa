<script lang="ts">
	// Splits the awful native <input type="datetime-local"> into two
	// friendlier inputs side-by-side. Bound value is an ISO-8601 RFC3339
	// string (UTC) — matches what the API expects. Internally we keep
	// date+time as separate local-tz strings so the browser's date/time
	// pickers behave naturally for the user.
	//
	// Caller usage:
	//   <DateTimePicker bind:value={show.starts_at} id="s" required />

	let {
		value = $bindable(''),
		id = '',
		required = false,
		disabled = false
	}: {
		value?: string;
		id?: string;
		required?: boolean;
		disabled?: boolean;
	} = $props();

	let dateStr = $state(''); // YYYY-MM-DD
	let timeStr = $state(''); // HH:mm

	// One-way sync from external `value`. Without the guard we'd ping-pong
	// against the $effect below that pushes the other way.
	let lastSyncedValue = '';
	$effect(() => {
		if (value === lastSyncedValue) return;
		lastSyncedValue = value;
		if (!value) {
			dateStr = '';
			timeStr = '';
			return;
		}
		const d = new Date(value);
		if (Number.isNaN(d.getTime())) return;
		const pad = (n: number) => n.toString().padStart(2, '0');
		dateStr = `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
		timeStr = `${pad(d.getHours())}:${pad(d.getMinutes())}`;
	});

	// Push back to bindable `value` when either piece changes. Only emits
	// when both fields are filled so partial state doesn't crash the
	// parent's `new Date(...)` parsing.
	$effect(() => {
		if (!dateStr || !timeStr) return;
		const next = new Date(`${dateStr}T${timeStr}`).toISOString();
		if (next !== lastSyncedValue) {
			lastSyncedValue = next;
			value = next;
		}
	});
</script>

<div class="flex gap-2">
	<input
		{id}
		type="date"
		bind:value={dateStr}
		{required}
		{disabled}
		class="dt-input flex-1 rounded-md border border-neutral-800 bg-neutral-900 px-3 py-2 text-neutral-100 focus:border-neutral-600 focus:outline-none disabled:opacity-50"
	/>
	<input
		type="time"
		bind:value={timeStr}
		{required}
		{disabled}
		class="dt-input w-32 rounded-md border border-neutral-800 bg-neutral-900 px-3 py-2 text-neutral-100 focus:border-neutral-600 focus:outline-none disabled:opacity-50"
	/>
</div>

<style>
	/* WebKit (Chrome/Edge/Safari) paints the picker indicator as a dark
	   icon that disappears on a dark background. Inverting the colour
	   matrix gives a legible light icon. Firefox doesn't expose the
	   indicator pseudo-element, but its native picker is already fine. */
	.dt-input::-webkit-calendar-picker-indicator {
		filter: invert(0.85);
		cursor: pointer;
		opacity: 0.7;
	}
	.dt-input::-webkit-calendar-picker-indicator:hover {
		opacity: 1;
	}
	/* Hide the spinner in some browsers — looks ugly inside the dark box. */
	.dt-input::-webkit-inner-spin-button,
	.dt-input::-webkit-outer-spin-button {
		-webkit-appearance: none;
		margin: 0;
	}
</style>
