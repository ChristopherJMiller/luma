import type { Timestamp } from './events'
import type { AgentSpec } from './link'

/** GET /internal/v1/agents/{id}/spec?tier= (C10), from the live catalog. */
export interface EvalSpec {
  agent: AgentSpec
  helper_model: string
  timezone: string
  webfetch_deny_domains: string[]
}

export interface EvalResult {
  id: string
  passed: boolean
  failures: string[]
  cost_usd: number
}

/** POST /internal/v1/eval-runs. */
export interface EvalRun {
  suite: string
  sdk_version: string
  cli_version: string
  image: string
  started_at: Timestamp
  finished_at: Timestamp
  passed: number
  failed: number
  cost_usd: number
  results: EvalResult[]
}

export interface ReconcileWorkspace {
  /** The Anthropic workspace name: luma-interactive or luma-ci. */
  name: string
  api_usd: number
}

/** POST /internal/v1/usage-reconciliations. */
export interface UsageReconciliation {
  /** YYYY-MM-DD, UTC. */
  day: string
  workspaces: ReconcileWorkspace[]
}

export interface ReconcileDrift {
  name: string
  api_usd: number
  ledger_usd: number
  drift_ratio: number
}

/** The response to a UsageReconciliation. */
export interface UsageReconciliationResult {
  workspaces: ReconcileDrift[]
}
