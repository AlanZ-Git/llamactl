export interface ResourcePolicy {
  allow_ai: boolean
  admission: 'allowed' | 'blocked'
  durable: boolean
  release: 'not_requested' | 'draining' | 'terminating' | 'released' | 'unreleased_unknown' | 'failed'
  revision: number
  boot_id: string
  release_operation?: {
    id: string
    mode: 'graceful' | 'immediate'
    policy_revision: number
    started_at: string
    ended_at?: string
    error_code?: string
  }
}

// Reject imprecise uint64 CAS values rather than send a rounded revision.
export function validateResourcePolicy(value: unknown): ResourcePolicy {
  if (!value || typeof value !== 'object') throw new Error('Invalid owner policy snapshot')
  const p = value as ResourcePolicy
  if (typeof p.allow_ai !== 'boolean' || typeof p.durable !== 'boolean' ||
    typeof p.boot_id !== 'string' || !p.boot_id || !Number.isSafeInteger(p.revision) || p.revision < 0 ||
    p.admission !== (p.allow_ai ? 'allowed' : 'blocked') ||
    !(p.allow_ai ? ['not_requested'] : ['draining', 'terminating', 'released', 'unreleased_unknown', 'failed']).includes(p.release)) {
    throw new Error('Invalid or imprecise owner policy snapshot; refresh with a compatible client')
  }
  return p
}

export interface ResourceMetric {
  value: number | null
  unit: string
  source: string
  scope: string
  available: boolean
  reason?: string
  sampled_at: string | null
  sample_window_ms: number
  stale: boolean
}

export interface ResourceSnapshot {
  sampled_at: string | null
  sample_window_ms: number
  interval_ms: number
  target_interval_ms: number
  stale: boolean
  throttled: boolean
  throttle_reason?: string
  gaming: ResourceMetric
  cpu: { usage: ResourceMetric; logical_count: ResourceMetric; physical_count: ResourceMetric }
  memory: { total: ResourceMetric; available: ResourceMetric; used: ResourceMetric }
  gpu: {
    source: string
    scope: string
    available: boolean
    reason?: string
    sampled_at: string | null
    attempted_at: string | null
    sample_window_ms: number
    stale: boolean
    devices: { index: number; utilization: ResourceMetric; memory_total: ResourceMetric; memory_used: ResourceMetric; memory_free: ResourceMetric }[]
  }
  processes: {
    instance: string
    generation: number
    coverage: string
    reason?: string
    observed_processes: number
    cpu: ResourceMetric
    working_set: ResourceMetric
    private_bytes: ResourceMetric
    gpu_utilization: ResourceMetric
    gpu_memory: ResourceMetric
  }[]
  process_coverage: string
}
