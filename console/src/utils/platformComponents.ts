// Per-component config for the Platform page's ResourceControl sliders.
//
// Each entry pins:
//  - where the component's pods live (namespace + label selector for
//    the usage gauge), and
//  - the slider's allowed memory range for that component.
//
// Selectors follow the upstream charts' standard `app.kubernetes.io/name`
// label. If a chart's pods don't carry that label, set `selector` to
// whatever the chart actually uses (some charts only set `app=<name>`).
// The gauge degrades to "no metrics" if the selector misses, so a wrong
// selector is recoverable without breaking the slider's write path.

const Mi = 1024 ** 2
const Gi = 1024 ** 3

export interface PlatformComponentConfig {
  namespace: string
  selector: string
  memoryMin: number
  memoryMax: number
}

// Default stop set is the same as MEMORY_STOPS from utils/resources. The
// ResourceControl primitive filters by [memoryMin, memoryMax] so we don't
// need to override the stops here unless a specific component needs a
// finer-grained range (none do yet).
export const PLATFORM_COMPONENTS: Record<string, PlatformComponentConfig> = {
  prometheus: {
    namespace: 'monitoring',
    selector: 'app.kubernetes.io/name=prometheus',
    memoryMin: 256 * Mi,
    memoryMax: 8 * Gi,
  },
  loki: {
    namespace: 'monitoring',
    selector: 'app.kubernetes.io/name=loki',
    memoryMin: 128 * Mi,
    memoryMax: 2 * Gi,
  },
  grafana: {
    namespace: 'monitoring',
    selector: 'app.kubernetes.io/name=grafana',
    memoryMin: 64 * Mi,
    memoryMax: 512 * Mi,
  },
  // Its peak is a re-list, not its steady state: an API server restart makes
  // it rebuild every store at once, which is what the card's sparkline is
  // worth watching for.
  'kube-state-metrics': {
    namespace: 'monitoring',
    selector: 'app.kubernetes.io/name=kube-state-metrics',
    memoryMin: 64 * Mi,
    memoryMax: 1 * Gi,
  },
  promtail: {
    namespace: 'monitoring',
    selector: 'app.kubernetes.io/name=promtail',
    memoryMin: 64 * Mi,
    memoryMax: 512 * Mi,
  },
  longhorn: {
    namespace: 'longhorn-system',
    selector: 'app=longhorn-manager',
    memoryMin: 128 * Mi,
    memoryMax: 2 * Gi,
  },
  dex: {
    namespace: 'dex',
    selector: 'app.kubernetes.io/name=dex',
    memoryMin: 64 * Mi,
    memoryMax: 512 * Mi,
  },
  zot: {
    namespace: 'zot',
    selector: 'app=zot',
    memoryMin: 128 * Mi,
    memoryMax: 1 * Gi,
  },
  'console-api': {
    namespace: 'kipper-system',
    selector: 'app=console-api',
    memoryMin: 64 * Mi,
    memoryMax: 512 * Mi,
  },
  traefik: {
    namespace: 'traefik',
    selector: 'app.kubernetes.io/name=traefik',
    memoryMin: 64 * Mi,
    memoryMax: 512 * Mi,
  },
  'cert-manager': {
    namespace: 'cert-manager',
    selector: 'app.kubernetes.io/name=cert-manager',
    memoryMin: 64 * Mi,
    memoryMax: 256 * Mi,
  },
  keda: {
    namespace: 'keda',
    selector: 'app=keda-operator',
    memoryMin: 64 * Mi,
    memoryMax: 256 * Mi,
  },
  velero: {
    namespace: 'velero',
    selector: 'app.kubernetes.io/name=velero',
    memoryMin: 128 * Mi,
    memoryMax: 1 * Gi,
  },
}

// Fallback bounds for any component name the backend reports but the
// frontend doesn't have a config for yet. The full memory stop set ends
// up clamped to this range. The gauge still renders; the slider just has
// a wider range than ideal.
export const PLATFORM_DEFAULT_BOUNDS: { memoryMin: number; memoryMax: number } = {
  memoryMin: 64 * Mi,
  memoryMax: 8 * Gi,
}

export function platformConfig(name: string): PlatformComponentConfig | null {
  return PLATFORM_COMPONENTS[name] ?? null
}
