/** "0.4s", "12s", "2m 05s", "1h 03m". Empty for values that are not durations. */
export function formatDuration(milliseconds: number): string {
	if (!Number.isFinite(milliseconds) || milliseconds < 0) return '';
	if (milliseconds < 950) return `${(Math.max(milliseconds, 100) / 1000).toFixed(1)}s`;
	const seconds = Math.round(milliseconds / 1000);
	if (seconds < 60) return `${seconds}s`;
	const minutes = Math.floor(seconds / 60);
	if (minutes < 60) return `${minutes}m ${String(seconds % 60).padStart(2, '0')}s`;
	return `${Math.floor(minutes / 60)}h ${String(minutes % 60).padStart(2, '0')}m`;
}

/** Compact, localized counts such as 12.4K. */
export function formatCount(value: number, locale?: string): string {
	return new Intl.NumberFormat(locale, { notation: 'compact', maximumFractionDigits: 1 }).format(
		value
	);
}

const RELATIVE_UNITS: [Intl.RelativeTimeFormatUnit, number][] = [
	['year', 31_536_000],
	['month', 2_592_000],
	['week', 604_800],
	['day', 86_400],
	['hour', 3_600],
	['minute', 60]
];

/** "5 minutes ago", "yesterday", "now". */
export function formatRelativeTime(value: string, now: number, locale?: string): string {
	const timestamp = Date.parse(value);
	if (Number.isNaN(timestamp)) return '';
	const seconds = Math.round((timestamp - now) / 1000);
	const formatter = new Intl.RelativeTimeFormat(locale, { numeric: 'auto' });
	for (const [unit, size] of RELATIVE_UNITS) {
		if (Math.abs(seconds) >= size) return formatter.format(Math.round(seconds / size), unit);
	}
	return formatter.format(0, 'second');
}
