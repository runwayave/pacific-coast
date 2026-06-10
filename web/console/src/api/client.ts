/**
 * Typed API client for the atlantis console BFF.
 *
 * All endpoints return typed responses or throw ApiError on non-2xx.
 * Query factories are compatible with TanStack Query v5 queryOptions().
 */

// ---------------------------------------------------------------------------
// Error type
// ---------------------------------------------------------------------------

export class ApiError extends Error {
  constructor(
    public readonly status: number,
    message: string,
  ) {
    super(message)
    this.name = 'ApiError'
  }
}

// ---------------------------------------------------------------------------
// Wire types
// ---------------------------------------------------------------------------

export interface SetupStatus {
  configured: boolean
}

export interface SetupResult {
  ok: boolean
}

export type ConnectivityProbeStatus = 'ok' | 'err' | 'wait'

export interface ConnectivityProbe {
  label: string
  status: ConnectivityProbeStatus
  meta?: string
}

export interface ConnectivityResponse {
  endpoint: string
  overall: ConnectivityProbeStatus
  probes: ConnectivityProbe[]
}

export interface LoginResult {
  ok: boolean
}

export type UserRole = 'admin' | 'viewer'

export interface MeResult {
  id: string
  email: string
  role: UserRole
  first_name: string
  last_name: string
}

export interface OperatorUser {
  id: number
  email: string
  role: UserRole
  first_name: string
  last_name: string
  created_at: string
}

export interface OperatorsResponse {
  users: OperatorUser[]
}

export interface SubmittedFile {
  path: string
  content: string
}

export interface MergedSchemaResponse {
  version: string
  files: SubmittedFile[]
}

export interface CanonicalIRResponse {
  ir: unknown
  content_hash: string
}

export interface SchemaVersionSummary {
  version: number
  caller: string
  plan_class: string
  event_type: string
  change_count: number
  created_at: string
  ir_hash: string
}

export interface SchemaHistoryResponse {
  versions: SchemaVersionSummary[]
  has_more: boolean
}

export interface SchemaVersionDetail {
  version: number
  caller: string
  plan_class: string
  event_type: string
  diff: DiffPayload
  up_sql: string
  down_sql: string
  ir_snapshot: unknown
  created_at: string
  parent_version?: number
  ir_hash: string
}

export interface DiffChange {
  entity_id: string
  field: string
  detail: string
  kind?: string
}

export interface DiffPayload {
  additive: DiffChange[]
  backfill_required: DiffChange[]
  breaking: DiffChange[]
}

export interface DiffVersionsResponse {
  from_version: number
  to_version: number
  diff: DiffPayload
  from_ir?: unknown
  to_ir?: unknown
}

export interface EntityLineageEntry {
  entity_id: string
  field_name: string
  introduced_by: string
  introduced_at: number
  last_modified_by: string
  last_modified_at: number
  removed_at?: number
}

export interface EntityLineageResponse {
  entries: EntityLineageEntry[]
}

export interface EntityOwnerEntry {
  entity_id: string
  introduced_by: string
  introduced_at: number
  field_count: number
}

export interface EntityOwnersResponse {
  owners: EntityOwnerEntry[]
}

// ---------------------------------------------------------------------------
// Schema editing types
// ---------------------------------------------------------------------------

export type EditOp = 'add' | 'replace' | 'remove'

export interface EditPreviewRequest {
  namespace: string
  entity: string
  field?: string
  op: EditOp
  field_text?: string
}

export interface EditPreviewResponse {
  owner_path: string
  caller: string
  old_content: string
  new_content: string
  plan_class: string
  up_sql: string
  down_sql?: string
  impact?: unknown[]
  breaking?: string[]
  parse_errors?: string[]
  checkpoint_hash: string
}

export interface EditPRRequest extends EditPreviewRequest {
  title?: string
  body?: string
  base_checkpoint_hash?: string
}

export interface EditPRResponse {
  pr_url: string
  number: number
}

// ---------------------------------------------------------------------------
// Caller repo mapping types
// ---------------------------------------------------------------------------

export interface CallerRepo {
  caller: string
  owner: string
  repo: string
  default_branch: string
  schema_path_prefix: string
}

export interface CallerReposResponse {
  repos: CallerRepo[]
}

// ---------------------------------------------------------------------------
// Callers types
// ---------------------------------------------------------------------------

export interface CallerInfo {
  caller: string
  file_count: number
  last_applied_at?: string
  schema_version?: number
  registered: boolean
  can_mutate: boolean
  cert_expires_at?: string // RFC3339; absent when no cert has been issued through the console
}

export interface RegisterCallerResponse {
  caller: string
  can_mutate: boolean
}

export interface GetCallersResponse {
  callers: CallerInfo[]
}

export interface CallerAliasesResponse {
  caller: string
  aliases: string[]
}

export interface RevokeCallerResponse {
  files_removed: number
}

export interface IssueCertResponse {
  cert_pem: string
  key_pem: string
  ca_pem: string
  expires_at: string
}

// ---------------------------------------------------------------------------
// Jobs types
// ---------------------------------------------------------------------------

export interface JobStatus {
  job_id: string
  job_name: string
  status: string
  payload: unknown
  attempts: number
  max_attempts: number
  last_error?: string
  created_at: string
  updated_at: string
  run_after?: string
}

export interface GetJobStatusResponse {
  found: boolean
  job?: JobStatus
}

export interface ListDeadJobsResponse {
  jobs: JobStatus[]
}

export interface RetryDeadJobResponse {
  job_id: string
}

// ---------------------------------------------------------------------------
// Audit log types
// ---------------------------------------------------------------------------

export interface AuditEntry {
  id: number
  user_id: number
  user_email: string
  action: string
  detail?: unknown
  created_at: string
}

export interface AuditLogResponse {
  entries: AuditEntry[]
}

export interface HealthCheck {
  name: string
  status: 'healthy' | 'unhealthy' | 'unknown'
  message?: string
  last_check?: string
}

export type LogLevel = 'debug' | 'info' | 'warn' | 'error'

export interface LogRecord {
  seq: number
  time: string
  level: LogLevel
  msg: string
  attrs: Record<string, string>
}

export interface LogsResponse {
  records: LogRecord[]
  last_seq: number
}

export interface HealthResponse {
  atlantis: {
    status: 'healthy' | 'unhealthy' | 'degraded'
    checks: HealthCheck[]
    readyz_code?: number
    healthz_code?: number
    started_at?: string     // RFC3339 — atlantis process boot
    server_version?: string // linker-stamped main.version
    schema_version?: number // latest applied schema version
    metrics_series?: number // non-comment lines in /metrics
  }
}

// ── Worker dispatcher (PR 3) ────────────────────────────────────────

export interface WorkerSessionSummary {
  session_id: string
  caller: string
  queue: string
  pod_id?: string
  sdk_version?: string
  connected_at: string       // RFC3339
  last_heartbeat_at: string  // RFC3339
  max_in_flight: number
  inflight_count: number
  dispatched: number
  completed: number
  failed: number
  revoked: number
  drained?: boolean
}

export interface WorkerInflightRow {
  job_id: number
  job_name: string
  dispatched_at: string
  ack_received: boolean
}

export interface WorkerSessionEvent {
  at: string
  kind: string
  job_id?: number
  job_name?: string
  note?: string
}

export interface WorkerSessionDetail extends WorkerSessionSummary {
  job_names: string[]
  inflight: WorkerInflightRow[]
  events: WorkerSessionEvent[]
}

export interface ListWorkersResponse {
  sessions: WorkerSessionSummary[]
}

export interface GetWorkerResponse {
  session: WorkerSessionDetail
}

// ---------------------------------------------------------------------------
// Core fetch helper
// ---------------------------------------------------------------------------

async function apiFetch<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    credentials: 'include',
    headers: {
      'Content-Type': 'application/json',
      ...(init?.headers ?? {}),
    },
    ...init,
  })

  if (!res.ok) {
    let msg = `HTTP ${res.status}`
    try {
      const body = await res.json() as { error?: string; message?: string }
      msg = body.error ?? body.message ?? msg
    } catch {
      // ignore JSON parse failure
    }
    throw new ApiError(res.status, msg)
  }

  // 204 No Content
  if (res.status === 204) return undefined as unknown as T

  return res.json() as Promise<T>
}

// ---------------------------------------------------------------------------
// Auth / setup
// ---------------------------------------------------------------------------

export const api = {
  setup: {
    status: (): Promise<SetupStatus> =>
      apiFetch<SetupStatus>('/api/setup/status'),

    connectivity: (): Promise<ConnectivityResponse> =>
      apiFetch<ConnectivityResponse>('/api/setup/connectivity'),

    configure: (
      firstName: string,
      lastName: string,
      email: string,
      password: string,
    ): Promise<SetupResult> =>
      apiFetch<SetupResult>('/api/setup', {
        method: 'POST',
        body: JSON.stringify({
          first_name: firstName,
          last_name: lastName,
          email,
          password,
        }),
      }),
  },

  auth: {
    login: (email: string, password: string): Promise<LoginResult> =>
      apiFetch<LoginResult>('/api/auth/login', {
        method: 'POST',
        body: JSON.stringify({ email, password }),
      }),

    logout: (): Promise<void> =>
      apiFetch<void>('/api/auth/logout', { method: 'POST' }),

    me: (): Promise<MeResult> =>
      apiFetch<MeResult>('/api/auth/me'),

    changePassword: (currentPassword: string, newPassword: string): Promise<{ ok: boolean }> =>
      apiFetch('/api/auth/password', {
        method: 'POST',
        body: JSON.stringify({ current_password: currentPassword, new_password: newPassword }),
      }),

    sudo: (password: string): Promise<{ ok: boolean; expires_in_seconds: number }> =>
      apiFetch('/api/auth/sudo', {
        method: 'POST',
        body: JSON.stringify({ password }),
      }),

    signOutOthers: (): Promise<{ ok: boolean; sessions_removed: number }> =>
      apiFetch('/api/auth/sign-out-others', { method: 'POST' }),

    signOutAll: (): Promise<{ ok: boolean; sessions_removed: number }> =>
      apiFetch('/api/auth/sign-out-all', { method: 'POST' }),
  },

  instance: {
    get: (): Promise<{ endpoint: string }> =>
      apiFetch<{ endpoint: string }>('/api/instance'),
  },

  schema: {
    merged: (sinceVersion?: string): Promise<MergedSchemaResponse> => {
      const qs = sinceVersion ? `?since=${encodeURIComponent(sinceVersion)}` : ''
      return apiFetch<MergedSchemaResponse>(`/api/schema${qs}`)
    },

    canonical: (): Promise<CanonicalIRResponse> =>
      apiFetch<CanonicalIRResponse>('/api/schema/canonical'),

    rollback: (toVersion: number): Promise<{ new_version: number; up_sql: string }> =>
      apiFetch('/api/schema/rollback', {
        method: 'POST',
        body: JSON.stringify({ to_version: toVersion }),
      }),

    previewRollback: (toVersion: number): Promise<{
      target_version: number
      current_version: number
      up_sql: string
      plan_class: string
      change_count: number
    }> =>
      apiFetch('/api/schema/rollback/preview', {
        method: 'POST',
        body: JSON.stringify({ to_version: toVersion }),
      }),
  },

  history: {
    list: (opts?: { before?: number; caller?: string; limit?: number }): Promise<SchemaHistoryResponse> => {
      const params = new URLSearchParams()
      if (opts?.before) params.set('before', String(opts.before))
      if (opts?.caller) params.set('caller', opts.caller)
      if (opts?.limit) params.set('limit', String(opts.limit))
      const qs = params.toString() ? `?${params.toString()}` : ''
      return apiFetch<SchemaHistoryResponse>(`/api/history${qs}`)
    },

    version: (version: number): Promise<SchemaVersionDetail> =>
      apiFetch<SchemaVersionDetail>(`/api/history/${version}`),

    diff: (from: number, to: number): Promise<DiffVersionsResponse> =>
      apiFetch<DiffVersionsResponse>(`/api/diff?from=${from}&to=${to}`),
  },

  lineage: {
    entity: (entityId: string): Promise<EntityLineageResponse> =>
      apiFetch<EntityLineageResponse>(`/api/lineage/${encodeURIComponent(entityId)}`),
  },

  owners: {
    all: (): Promise<EntityOwnersResponse> =>
      apiFetch<EntityOwnersResponse>('/api/owners'),

    entity: (entityId: string): Promise<EntityOwnersResponse> =>
      apiFetch<EntityOwnersResponse>(`/api/owners/${encodeURIComponent(entityId)}`),
  },

  health: {
    get: (): Promise<HealthResponse> =>
      apiFetch<HealthResponse>('/api/health'),
  },

  logs: {
    since: (since: number): Promise<LogsResponse> =>
      apiFetch<LogsResponse>(`/api/logs?since=${since}`),
  },

  workers: {
    list: (): Promise<ListWorkersResponse> =>
      apiFetch<ListWorkersResponse>('/api/admin/workers'),

    get: (sessionID: string): Promise<GetWorkerResponse> =>
      apiFetch<GetWorkerResponse>(`/api/admin/workers/${encodeURIComponent(sessionID)}`),

    drain: (sessionID: string): Promise<Record<string, never>> =>
      apiFetch(`/api/admin/workers/${encodeURIComponent(sessionID)}/drain`, { method: 'POST' }),

    evict: (sessionID: string): Promise<Record<string, never>> =>
      apiFetch(`/api/admin/workers/${encodeURIComponent(sessionID)}/evict`, { method: 'POST' }),
  },

  schemaEdit: {
    preview: (req: EditPreviewRequest): Promise<EditPreviewResponse> =>
      apiFetch<EditPreviewResponse>('/api/schema/edit/preview', {
        method: 'POST',
        body: JSON.stringify(req),
      }),

    openPR: (req: EditPRRequest): Promise<EditPRResponse> =>
      apiFetch<EditPRResponse>('/api/schema/edit/pr', {
        method: 'POST',
        body: JSON.stringify(req),
      }),
  },

  callerRepos: {
    list: (): Promise<CallerReposResponse> =>
      apiFetch<CallerReposResponse>('/api/callers/repos'),

    upsert: (caller: string, data: Omit<CallerRepo, 'caller'>): Promise<{ ok: boolean }> =>
      apiFetch<{ ok: boolean }>(`/api/callers/repos/${encodeURIComponent(caller)}`, {
        method: 'PUT',
        body: JSON.stringify(data),
      }),
  },

  callers: {
    list: (): Promise<GetCallersResponse> =>
      apiFetch<GetCallersResponse>('/api/callers'),

    register: (caller: string, canMutate: boolean): Promise<RegisterCallerResponse> =>
      apiFetch<RegisterCallerResponse>('/api/callers', {
        method: 'POST',
        body: JSON.stringify({ caller, can_mutate: canMutate }),
      }),

    revoke: (caller: string): Promise<RevokeCallerResponse> =>
      apiFetch<RevokeCallerResponse>(`/api/callers/${encodeURIComponent(caller)}`, {
        method: 'DELETE',
      }),

    issueCert: (caller: string): Promise<IssueCertResponse> =>
      apiFetch<IssueCertResponse>(`/api/callers/${encodeURIComponent(caller)}/cert/issue`, {
        method: 'POST',
      }),

    revokeAll: (): Promise<{ ok: boolean; revoked: number; failures: string[] }> =>
      apiFetch('/api/callers/revoke-all', { method: 'POST' }),

    aliases: (caller: string): Promise<CallerAliasesResponse> =>
      apiFetch<CallerAliasesResponse>(`/api/callers/${encodeURIComponent(caller)}/aliases`),

    setAliases: (caller: string, aliases: string[]): Promise<CallerAliasesResponse> =>
      apiFetch<CallerAliasesResponse>(`/api/callers/${encodeURIComponent(caller)}/aliases`, {
        method: 'PUT',
        body: JSON.stringify({ aliases }),
      }),
  },

  users: {
    list: (): Promise<OperatorsResponse> =>
      apiFetch<OperatorsResponse>('/api/users'),

    create: (
      firstName: string,
      lastName: string,
      email: string,
      password: string,
      role: UserRole,
    ): Promise<{ id: number; email: string; role: UserRole; first_name: string; last_name: string }> =>
      apiFetch('/api/users', {
        method: 'POST',
        body: JSON.stringify({
          first_name: firstName,
          last_name: lastName,
          email,
          password,
          role,
        }),
      }),

    setRole: (id: number, role: UserRole): Promise<{ ok: boolean }> =>
      apiFetch(`/api/users/${id}/role`, {
        method: 'PUT',
        body: JSON.stringify({ role }),
      }),

    delete: (id: number): Promise<{ ok: boolean }> =>
      apiFetch(`/api/users/${id}`, { method: 'DELETE' }),
  },

  jobs: {
    get: (id: string): Promise<GetJobStatusResponse> =>
      apiFetch<GetJobStatusResponse>(`/api/jobs/${encodeURIComponent(id)}`),

    listDead: (opts?: { limit?: number; job_name?: string }): Promise<ListDeadJobsResponse> => {
      const params = new URLSearchParams()
      if (opts?.limit) params.set('limit', String(opts.limit))
      if (opts?.job_name) params.set('job_name', opts.job_name)
      const qs = params.toString() ? `?${params.toString()}` : ''
      return apiFetch<ListDeadJobsResponse>(`/api/jobs/dead${qs}`)
    },

    retry: (id: string): Promise<RetryDeadJobResponse> =>
      apiFetch<RetryDeadJobResponse>(`/api/jobs/${encodeURIComponent(id)}/retry`, {
        method: 'POST',
      }),
  },

  audit: {
    list: (limit = 100): Promise<AuditLogResponse> =>
      apiFetch<AuditLogResponse>(`/api/audit?limit=${limit}`),
  },

  // ─────────────────────────────────────────────────────────────────
  // Sandbox — browser hits /api/sandbox/*; the BFF rewrites the path
  // and proxies to the in-process runtime at /v1/sandbox/*. Every
  // response carries t_server_us so the workbench can report server-
  // measured time without round-trip noise.
  // ─────────────────────────────────────────────────────────────────
  sandbox: {
    list: (): Promise<SandboxListResponse> =>
      apiFetch<SandboxListResponse>('/api/sandbox'),

    boot: (req: SandboxBootRequest): Promise<SandboxBootResponse> =>
      apiFetch<SandboxBootResponse>('/api/sandbox', {
        method: 'POST',
        body: JSON.stringify(req),
      }),

    destroy: (pubID: string): Promise<void> =>
      apiFetch<void>(`/api/sandbox/${encodeURIComponent(pubID)}`, { method: 'DELETE' }),

    exec: (pubID: string, sql: string, args: unknown[] = []): Promise<SandboxExecResponse> =>
      apiFetch<SandboxExecResponse>(`/api/sandbox/${encodeURIComponent(pubID)}/sql/exec`, {
        method: 'POST',
        body: JSON.stringify({ sql, args }),
      }),

    query: (pubID: string, sql: string, args: unknown[] = []): Promise<SandboxQueryResponse> =>
      apiFetch<SandboxQueryResponse>(`/api/sandbox/${encodeURIComponent(pubID)}/sql/query`, {
        method: 'POST',
        body: JSON.stringify({ sql, args }),
      }),

    catalog: (pubID: string): Promise<SandboxCatalogResponse> =>
      apiFetch<SandboxCatalogResponse>(
        `/api/sandbox/${encodeURIComponent(pubID)}/inspect/catalog`,
      ),

    describe: (pubID: string, qualified: string): Promise<SandboxDescribeResponse> =>
      apiFetch<SandboxDescribeResponse>(
        `/api/sandbox/${encodeURIComponent(pubID)}/inspect/describe?q=${encodeURIComponent(qualified)}`,
      ),

    sample: (pubID: string, qualified: string, n = 10): Promise<SandboxRowsResponse> =>
      apiFetch<SandboxRowsResponse>(
        `/api/sandbox/${encodeURIComponent(pubID)}/inspect/sample?q=${encodeURIComponent(qualified)}&n=${n}`,
      ),

    find: (pubID: string, req: SandboxFindRequest): Promise<SandboxRowsResponse> =>
      apiFetch<SandboxRowsResponse>(`/api/sandbox/${encodeURIComponent(pubID)}/inspect/find`, {
        method: 'POST',
        body: JSON.stringify(req),
      }),

    mark: (pubID: string): Promise<SandboxMarkResponse> =>
      apiFetch<SandboxMarkResponse>(`/api/sandbox/${encodeURIComponent(pubID)}/mark`, {
        method: 'POST',
        body: '{}',
      }),

    restore: (pubID: string, markID: string): Promise<void> =>
      apiFetch<void>(`/api/sandbox/${encodeURIComponent(pubID)}/restore`, {
        method: 'POST',
        body: JSON.stringify({ mark_id: markID }),
      }),

    bulk: (pubID: string, req: SandboxBulkRequest): Promise<SandboxBulkResponse> =>
      apiFetch<SandboxBulkResponse>(`/api/sandbox/${encodeURIComponent(pubID)}/fixtures/bulk`, {
        method: 'POST',
        body: JSON.stringify(req),
      }),

    fork: (pubID: string, n: number): Promise<SandboxForkResponse> =>
      apiFetch<SandboxForkResponse>(`/api/sandbox/${encodeURIComponent(pubID)}/fork`, {
        method: 'POST',
        body: JSON.stringify({ n }),
      }),

    diff: (pubID: string, beforeMarkID: string, afterMarkID: string): Promise<SandboxDiffResponse> =>
      apiFetch<SandboxDiffResponse>(`/api/sandbox/${encodeURIComponent(pubID)}/inspect/diff`, {
        method: 'POST',
        body: JSON.stringify({ before_mark_id: beforeMarkID, after_mark_id: afterMarkID }),
      }),
  },
}

// ─────────────────────────────────────────────────────────────────
// Sandbox wire types
// ─────────────────────────────────────────────────────────────────

export interface SandboxBootRequest {
  backend?: 'sim' | 'embedded'
  determinism?: 'off' | 'strict'
  seed?: number
}

export interface SandboxBootResponse {
  pub_id: string
  backend: string
  boot_ms: number
  schema_version?: string
  entity_count: number
  t_server_us?: number
}

export interface SandboxListEntry {
  pub_id: string
  backend: string
  schema_version?: string
  created_at: string
  last_active: string
  boot_ms: number
}

export interface SandboxListResponse {
  sandboxes: SandboxListEntry[]
}

export interface SandboxExecResponse {
  rows_affected: number
  t_server_us?: number
}

export interface SandboxQueryResponse {
  rows: Array<Record<string, unknown>>
  t_server_us?: number
}

export interface SandboxColumnInfo {
  name: string
  kind: string
  nullable: boolean
}

export interface SandboxCatalogResponse {
  entities: string[]
  t_server_us?: number
}

export interface SandboxDescribeResponse {
  schema: string
  name: string
  qualified: string
  columns: SandboxColumnInfo[]
  primary_key: string[]
  row_count: number
  soft_delete_field?: string
  touch_on_update_field?: string
  partition_field?: string
  time_field?: string
  identity_col?: string
  t_server_us?: number
}

export interface SandboxRowsResponse {
  rows: Array<Record<string, unknown>>
  t_server_us?: number
}

export interface SandboxFindPredicate {
  column: string
  op: '=' | '!=' | '<' | '<=' | '>' | '>=' | 'is null' | 'is not null'
  value?: unknown
}

export interface SandboxFindRequest {
  qualified: string
  predicates: SandboxFindPredicate[]
}

export interface SandboxMarkResponse {
  mark_id: string
  t_server_us?: number
}

export interface SandboxBulkRequest {
  qualified: string
  n: number
  seed?: number
  pk_start?: number
}

export interface SandboxBulkResponse {
  inserted: number
  t_server_us?: number
}

export interface SandboxForkResponse {
  ids: string[]
  backend: string
  t_server_us?: number
}

export interface SandboxTableDiff {
  added: number
  removed: number
  modified: number
}

export interface SandboxDiffResponse {
  tables: Record<string, SandboxTableDiff>
  t_server_us?: number
}

// ---------------------------------------------------------------------------
// TanStack Query factories
// ---------------------------------------------------------------------------

export const queries = {
  setupStatus: () => ({
    queryKey: ['setup', 'status'] as const,
    queryFn: () => api.setup.status(),
  }),

  me: () => ({
    queryKey: ['auth', 'me'] as const,
    queryFn: () => api.auth.me(),
    retry: false,
  }),

  schemaMerged: (sinceVersion?: string) => ({
    queryKey: ['schema', 'merged', sinceVersion] as const,
    queryFn: () => api.schema.merged(sinceVersion),
    staleTime: 30_000,
  }),

  schemaCanonical: () => ({
    queryKey: ['schema', 'canonical'] as const,
    queryFn: () => api.schema.canonical(),
    staleTime: 30_000,
  }),

  historyList: (opts?: { before?: number; caller?: string; limit?: number }) => ({
    queryKey: ['history', 'list', opts] as const,
    queryFn: () => api.history.list(opts),
  }),

  historyVersion: (version: number) => ({
    queryKey: ['history', 'version', version] as const,
    queryFn: () => api.history.version(version),
    enabled: version > 0,
  }),

  diff: (from: number, to: number) => ({
    queryKey: ['diff', from, to] as const,
    queryFn: () => api.history.diff(from, to),
    enabled: from > 0 && to > 0,
  }),

  entityLineage: (entityId: string) => ({
    queryKey: ['lineage', entityId] as const,
    queryFn: () => api.lineage.entity(entityId),
    enabled: !!entityId,
  }),

  entityOwners: (entityId?: string) => ({
    queryKey: ['owners', entityId ?? 'all'] as const,
    queryFn: () => entityId ? api.owners.entity(entityId) : api.owners.all(),
  }),

  health: () => ({
    queryKey: ['health'] as const,
    queryFn: () => api.health.get(),
    refetchInterval: 30_000,
  }),

  callerRepos: () => ({
    queryKey: ['callerRepos'] as const,
    queryFn: () => api.callerRepos.list(),
    staleTime: 60_000,
  }),

  callers: () => ({
    queryKey: ['callers'] as const,
    queryFn: () => api.callers.list(),
    staleTime: 30_000,
  }),

  operators: () => ({
    queryKey: ['users', 'operators'] as const,
    queryFn: () => api.users.list(),
    staleTime: 30_000,
  }),

  instance: () => ({
    queryKey: ['instance'] as const,
    queryFn: () => api.instance.get(),
    staleTime: Infinity, // endpoint is process config, doesn't change at runtime
  }),

  deadJobs: (opts?: { limit?: number; job_name?: string }) => ({
    queryKey: ['jobs', 'dead', opts] as const,
    queryFn: () => api.jobs.listDead(opts),
    refetchInterval: 15_000,
  }),

  auditLog: (limit = 100) => ({
    queryKey: ['audit', limit] as const,
    queryFn: () => api.audit.list(limit),
    staleTime: 10_000,
  }),

  sandboxList: () => ({
    queryKey: ['sandbox', 'list'] as const,
    queryFn: () => api.sandbox.list(),
    // Poll cheaply so the list reflects ephemeral lifecycle. List
    // requests do NOT bump server-side TTL (see internal/console/sandbox.go).
    refetchInterval: 5_000,
  }),

  sandboxDescribe: (pubID: string, qualified: string) => ({
    queryKey: ['sandbox', pubID, 'describe', qualified] as const,
    queryFn: () => api.sandbox.describe(pubID, qualified),
    enabled: !!pubID && !!qualified,
  }),

  sandboxCatalog: (pubID: string) => ({
    queryKey: ['sandbox', pubID, 'catalog'] as const,
    queryFn: () => api.sandbox.catalog(pubID),
    enabled: !!pubID,
    // Catalog is built at boot and immutable for the sandbox's lifetime,
    // so cache aggressively — no point re-fetching.
    staleTime: Infinity,
  }),
}
