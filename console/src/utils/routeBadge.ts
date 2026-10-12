export interface RouteHealthState {
  ingress_ready: boolean
  tls_ready: boolean
  unpublished?: boolean
}

export interface RouteBadge {
  label: string
  tone: 'good' | 'pending' | 'neutral'
}

/** Keeps route status badges consistent across the Routes page and app Settings. */
export function routeBadge(health: RouteHealthState, liveLabel: string): RouteBadge {
  if (health.unpublished) return { label: 'Nothing published', tone: 'neutral' }
  if (health.tls_ready) return { label: liveLabel, tone: 'good' }
  return { label: health.ingress_ready ? 'TLS pending' : 'Provisioning', tone: 'pending' }
}

export const routeBadgeClass: Record<RouteBadge['tone'], string> = {
  good: 'bg-emerald-50 text-emerald-700 dark:bg-emerald-950 dark:text-emerald-400',
  pending: 'bg-amber-50 text-amber-700 dark:bg-amber-950 dark:text-amber-400',
  neutral: 'bg-slate-100 text-slate-600 dark:bg-slate-800 dark:text-slate-400',
}
