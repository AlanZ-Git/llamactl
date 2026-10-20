import type { ResourceMetric } from './resources'

export type ControlPatch = Partial<Record<'context' | 'gpu_layers' | 'threads' | 'batch' | 'ubatch' | 'cache_ram', number | null>> & {
  kv_type_k?: 'f16' | 'q8_0' | 'q4_0' | null
  kv_type_v?: 'f16' | 'q8_0' | 'q4_0' | null
  flash_attention?: boolean
}
export interface ControlSnapshot {
  protocol_version: string
  actions: string[]
  operation_budget_seconds: number
  operation_capacity: number
  idempotency_retention_seconds: number
  admission: string
  boot_id: string
  revision: number
  state: string
  running: boolean
  target_model?: string
  actual_model?: string
  ready_token?: string
  active_operation?: string
  process_generation: number
  load_generation: number
  models: string[]
  parameters: Record<string, { type: string; minimum?: number; maximum?: number; nullable: boolean; enum?: string[] }>
  configured_parameters: Record<string, ControlPatch>
  ready_probe: string
  recovery_required: boolean
}
export interface TestEnvelope { boot_id: string; expected_revision: number; idempotency_key: string }
export interface DeploymentTestRequest extends TestEnvelope {
  model: string
  fixture_id: 'synthetic-text-v1'
  patch?: ControlPatch
  measured_generations?: number
  warmup?: boolean
  max_output_tokens?: number
  timeout_seconds?: number
  concurrency?: 1
  seed?: number
}
export interface GenerationObservation {
  kind: 'warmup' | 'initial' | 'subsequent'
  started_at: string
  duration_ms: number
  ttft_ms: number | null
  prompt_tokens: number | null
  completion_tokens: number | null
  backend_prefill_ms: number | null
  backend_decode_ms: number | null
  response_bytes: number
  completed: boolean
  error_code?: string
}
export interface DeploymentMetric {
  unit: string; source: string; scope: string
  mean: number | null; observed_peak: number | null
  observed: number; missing: number; stale: number; coverage: number
  first_at: string | null; last_at: string | null; reason?: string
}
export interface DeploymentResources {
  source: 'shared_resource_cache'
  baseline_at: string | null
  baseline: Record<string, ResourceMetric>
  metrics: Record<string, DeploymentMetric>
  distinct_snapshots: number
  gap?: string
  contention: string
}
export interface DeploymentTest {
  id: string; boot_id: string; instance: string; model: string
  state: 'running' | 'succeeded' | 'partial' | 'failed' | 'cancelled'
  stage: string; error_code?: string; error_stage?: string
  started_at: string; deadline: string; ended_at: string | null; elapsed_ms: number
  revision: number; process_generation: number; load_generation: number
  parameters: ControlPatch; parameter_defaults: string
  identity: {
    application_version: string; application_commit: string; application_build_time: string
    source_modified: boolean | null
    engine: string; engine_version: string; engine_hash: string; model_hash: string; runtime_identity: string
  }
  fixture_id: string; fixture_digest: string
  measured_generations: number; warmup: boolean; max_output_tokens: number; seed: number
  temperature: number; concurrency: 1; attempted_generations: number; completed_measured: number
  load_mode: 'test_owned' | 'reused_ready'; cache_condition: 'unknown'; cache_prompt_requested: boolean
  ready_duration_ms: number | null
  cleanup: string; cleanup_error?: string; cleanup_duration_ms: number
  samples: GenerationObservation[]; resources: DeploymentResources
}
