// WorkerSession — per-session drill-in.
//
// Path-title with mono treatment, meta strip (Caller / Queue / Pod /
// Connected / Last heartbeat / SDK), four counter cards, in-flight
// table, handles list, recent events log. Drain / Evict live in the
// page-head action slot, both gated by the shared SudoConfirmDialog.

import { useEffect, useState } from 'react'
import { Link, useNavigate, useParams } from '@tanstack/react-router'
import { AlertTriangle, ShieldOff, ChevronLeft, Activity } from 'lucide-react'
import { api, ApiError } from '@/api/client'
import type { WorkerSessionDetail } from '@/api/client'
import { PageShell } from '@/components/PageShell'
import { SudoConfirmDialog } from './Settings'

const POLL_MS = 2000

export function WorkerSession() {
  const { id } = useParams({ from: '/workers/$id' })
  const navigate = useNavigate()

  const [detail, setDetail] = useState<WorkerSessionDetail | null>(null)
  const [loading, setLoading] = useState(true)
  const [notFound, setNotFound] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [tick, setTick] = useState(0)

  const [showDrain, setShowDrain] = useState(false)
  const [showEvict, setShowEvict] = useState(false)
  const [actionPending, setActionPending] = useState(false)
  const [actionError, setActionError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    let interval: ReturnType<typeof setInterval> | null = null

    const fetchOnce = async () => {
      if (document.visibilityState !== 'visible') return
      try {
        const res = await api.workers.get(id)
        if (cancelled) return
        // Normalize array fields: the server marshals empty Go slices as
        // JSON null, which would crash the `.length` / `.map` calls below
        // once a session has zero in-flight rows (or no events yet).
        setDetail({
          ...res.session,
          job_names: res.session.job_names ?? [],
          inflight: res.session.inflight ?? [],
          events: res.session.events ?? [],
        })
        setNotFound(false)
        setError(null)
      } catch (e) {
        if (cancelled) return
        if (e instanceof ApiError && e.status === 404) {
          setNotFound(true)
        } else {
          setError(e instanceof ApiError ? e.message : 'failed to load session')
        }
      } finally {
        if (!cancelled) setLoading(false)
      }
    }

    fetchOnce()
    interval = setInterval(fetchOnce, POLL_MS)
    const onVis = () => {
      if (document.visibilityState === 'visible') fetchOnce()
    }
    document.addEventListener('visibilitychange', onVis)
    return () => {
      cancelled = true
      if (interval) clearInterval(interval)
      document.removeEventListener('visibilitychange', onVis)
    }
  }, [id, tick])

  // Per-second tick for relative timestamps.
  const [, setRel] = useState(0)
  useEffect(() => {
    const id = setInterval(() => setRel(n => n + 1), 1000)
    return () => clearInterval(id)
  }, [])

  const drain = async (password: string) => {
    setActionPending(true)
    setActionError(null)
    try {
      await api.auth.sudo(password)
      await api.workers.drain(id)
      setShowDrain(false)
      setTick(t => t + 1)
    } catch (e) {
      setActionError(e instanceof ApiError ? e.message : 'drain failed')
    } finally {
      setActionPending(false)
    }
  }

  const evict = async (password: string) => {
    setActionPending(true)
    setActionError(null)
    try {
      await api.auth.sudo(password)
      await api.workers.evict(id)
      navigate({ to: '/workers' })
    } catch (e) {
      setActionError(e instanceof ApiError ? e.message : 'evict failed')
      setActionPending(false)
    }
  }

  if (notFound) {
    return (
      <PageShell title="Worker session" sub="not found">
        <div className="page__bodyinner">
          <div className="card">
            <div className="empty">
              <div className="empty__icon">
                <Activity size={18} />
              </div>
              <div className="empty__title">Session not found</div>
              <div className="empty__sub">
                The session disconnected or never registered.
              </div>
              <Link to="/workers" className="btn btn--ghost btn--sm" style={{ gap: 6 }}>
                <ChevronLeft size={12} /> Back to workers
              </Link>
            </div>
          </div>
        </div>
      </PageShell>
    )
  }

  if (loading && !detail) {
    return (
      <PageShell title="Worker session" sub="loading">
        <div className="page__bodyinner">
          <div className="card" style={{ padding: 12 }}>
            <div className="sk" style={{ height: 64, marginBottom: 12 }} />
            <div className="sk" style={{ height: 120 }} />
          </div>
        </div>
      </PageShell>
    )
  }

  if (!detail) {
    return (
      <PageShell title="Worker session" sub="error">
        <div className="page__bodyinner">
          <div className="banner banner--error">{error || 'failed to load'}</div>
        </div>
      </PageShell>
    )
  }

  const stale = isStale(detail.last_heartbeat_at)
  const statusKind: 'connected' | 'stale' | 'drained' = detail.drained
    ? 'drained'
    : stale
      ? 'stale'
      : 'connected'

  const title = (
    <span style={{ display: 'inline-flex', alignItems: 'center', gap: 12 }}>
      <Link to="/workers" className="btn btn--ghost btn--sm btn--icon" title="Back" style={{ width: 28, height: 28 }}>
        <ChevronLeft size={14} />
      </Link>
      <span className="mono" style={{ fontSize: 19, fontWeight: 450, letterSpacing: '-0.01em' }}>
        <span className="faint">{detail.caller}</span>
        <span className="faint" style={{ margin: '0 6px' }}>/</span>
        <span style={{ color: 'var(--ink-0)' }}>{detail.queue}</span>
      </span>
      <StatusPill kind={statusKind} />
    </span>
  )

  const action = (
    <>
      {!detail.drained && (
        <button
          className="btn btn--ghost btn--sm"
          onClick={() => { setActionError(null); setShowDrain(true) }}
          style={{ gap: 6 }}
        >
          <AlertTriangle size={12} />
          Drain
        </button>
      )}
      <button
        className="btn btn--sm"
        onClick={() => { setActionError(null); setShowEvict(true) }}
        style={{ gap: 6, color: 'var(--coral)', borderColor: 'transparent' }}
      >
        <ShieldOff size={12} />
        Evict
      </button>
    </>
  )

  return (
    <PageShell title={title} sub={`session ${detail.session_id}`} action={action}>
      <div className="page__bodyinner">
        {error && <div className="banner banner--error" style={{ marginBottom: 18 }}>{error}</div>}

        {/* Meta strip — mirrors Schema entity-detail's .detail__meta layout. */}
        <div style={{ display: 'flex', gap: 28, flexWrap: 'wrap', marginBottom: 28 }}>
          <Meta label="Caller" value={detail.caller} mono />
          <Meta label="Queue" value={detail.queue} />
          <Meta label="Pod" value={detail.pod_id || '—'} mono faint={!detail.pod_id} />
          <Meta label="SDK" value={detail.sdk_version || '—'} mono faint={!detail.sdk_version} />
          <Meta label="Connected" value={relativeAgo(detail.connected_at)} />
          <Meta
            label="Heartbeat"
            value={relativeAgo(detail.last_heartbeat_at)}
            tone={stale ? 'coral' : detail.drained ? 'muted' : 'sage'}
          />
        </div>

        {/* Counter cards. */}
        <div style={{
          display: 'grid',
          gridTemplateColumns: 'repeat(5, minmax(0, 1fr))',
          gap: 12,
          marginBottom: 32,
        }}>
          <CounterTile label="Dispatched" value={detail.dispatched} />
          <CounterTile label="Completed" value={detail.completed} tone="sage" />
          <CounterTile label="Failed" value={detail.failed} tone={detail.failed > 0 ? 'coral' : undefined} />
          <CounterTile label="Revoked" value={detail.revoked} tone={detail.revoked > 0 ? 'coral' : undefined} />
          <CounterTile
            label="In-flight"
            value={detail.inflight_count}
            sub={`of ${detail.max_in_flight} max`}
          />
        </div>

        <div style={{ display: 'grid', gridTemplateColumns: 'minmax(0, 2fr) minmax(0, 1fr)', gap: 18, marginBottom: 32 }}>
          {/* In-flight jobs */}
          <div>
            <div className="section-label" style={{ marginBottom: 12 }}>IN-FLIGHT</div>
            {detail.inflight.length === 0 ? (
              <div className="card">
                <div className="empty" style={{ padding: '32px 24px' }}>
                  <div className="empty__title">Idle</div>
                  <div className="empty__sub">No jobs currently in-flight on this session.</div>
                </div>
              </div>
            ) : (
              <div className="card" style={{ padding: 6 }}>
                <table className="tbl">
                  <thead>
                    <tr>
                      <th>Job ID</th>
                      <th>Name</th>
                      <th>Dispatched</th>
                      <th style={{ textAlign: 'right' }}>Ack</th>
                    </tr>
                  </thead>
                  <tbody>
                    {detail.inflight.map(r => (
                      <tr key={r.job_id}>
                        <td className="mono num">{r.job_id}</td>
                        <td className="mono">{r.job_name}</td>
                        <td className="num">{relativeAgo(r.dispatched_at)}</td>
                        <td style={{ textAlign: 'right' }}>
                          {r.ack_received ? (
                            <span className="sage" style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }}>
                              <span className="dot" style={{ background: 'var(--sage)' }} />
                              acked
                            </span>
                          ) : (
                            <span className="muted" style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }}>
                              <span className="dot" style={{ background: 'var(--ink-3)' }} />
                              pending
                            </span>
                          )}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </div>

          {/* Handles */}
          <div>
            <div className="section-label" style={{ marginBottom: 12 }}>HANDLES</div>
            <div className="card">
              <div className="card__body" style={{ padding: 0 }}>
                {detail.job_names.length === 0 ? (
                  <div className="empty" style={{ padding: '24px 12px' }}>
                    <div className="empty__sub">No declared job names.</div>
                  </div>
                ) : (
                  <ul style={{ listStyle: 'none' }}>
                    {detail.job_names.map((n, i) => (
                      <li
                        key={n}
                        className="mono"
                        style={{
                          padding: '10px 16px',
                          fontSize: 'var(--font-data)',
                          color: 'var(--ink-1)',
                          borderBottom: i === detail.job_names.length - 1 ? 'none' : '1px solid var(--line-soft)',
                        }}
                      >
                        {n}
                      </li>
                    ))}
                  </ul>
                )}
              </div>
            </div>
          </div>
        </div>

        {/* Events */}
        <div className="section-label" style={{ marginBottom: 12 }}>RECENT EVENTS</div>
        {detail.events.length === 0 ? (
          <div className="card">
            <div className="empty" style={{ padding: '24px' }}>
              <div className="empty__sub">No recorded events yet.</div>
            </div>
          </div>
        ) : (
          <div className="card" style={{ padding: 6 }}>
            <table className="tbl">
              <thead>
                <tr>
                  <th style={{ width: 100 }}>When</th>
                  <th>Event</th>
                  <th>Job</th>
                  <th>Note</th>
                </tr>
              </thead>
              <tbody>
                {[...detail.events].reverse().map((e, i) => (
                  <tr key={i}>
                    <td className="mono num faint" style={{ fontSize: 11.5 }}>{relativeAgo(e.at)}</td>
                    <td>
                      <EventKind kind={e.kind} />
                    </td>
                    <td className="mono" style={{ fontSize: 12 }}>
                      {e.job_id ? (
                        <>
                          <span className="faint">#</span>
                          <span>{e.job_id}</span>
                          {e.job_name && <span className="faint" style={{ marginLeft: 6 }}>{e.job_name}</span>}
                        </>
                      ) : (
                        <span className="faint">—</span>
                      )}
                    </td>
                    <td className="muted" style={{ fontSize: 12 }}>{e.note || ''}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>

      {showDrain && (
        <SudoConfirmDialog
          title="Drain worker"
          icon={<AlertTriangle size={18} className="brass" />}
          body={
            <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
              <p>
                The dispatcher will stop sending new jobs to this session.{' '}
                {detail.inflight_count === 0
                  ? 'The session will disconnect cleanly within a few seconds.'
                  : `The ${detail.inflight_count} in-flight job${detail.inflight_count === 1 ? '' : 's'} will finish first.`}
              </p>
              <p className="muted" style={{ fontSize: 12 }}>
                Safe for rolling out a worker change.
              </p>
            </div>
          }
          confirmLabel="Drain"
          pending={actionPending}
          error={actionError}
          onCancel={() => setShowDrain(false)}
          onConfirm={drain}
        />
      )}

      {showEvict && (
        <SudoConfirmDialog
          title="Evict worker"
          icon={<ShieldOff size={18} className="coral" />}
          body={
            <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
              <p>
                Force-close the stream immediately. {detail.inflight_count > 0 && (
                  <>The <b>{detail.inflight_count} in-flight job{detail.inflight_count === 1 ? '' : 's'}</b> will be returned to the queue and re-attempted by another worker.</>
                )}
              </p>
              <p className="coral" style={{ fontSize: 12 }}>
                Destructive. Use only for stuck or misbehaving workers.
              </p>
            </div>
          }
          requiredText="evict"
          confirmLabel="Evict"
          pending={actionPending}
          error={actionError}
          onCancel={() => setShowEvict(false)}
          onConfirm={evict}
        />
      )}
    </PageShell>
  )
}

function StatusPill({ kind }: { kind: 'connected' | 'stale' | 'drained' }) {
  const map = {
    connected: { dot: 'dot dot--brass', label: 'connected', cls: '' },
    stale: { dot: 'dot dot--coral', label: 'stale heartbeat', cls: 'coral' },
    drained: { dot: 'dot', label: 'draining', cls: 'muted' },
  } as const
  const s = map[kind]
  return (
    <span style={{
      display: 'inline-flex', alignItems: 'center', gap: 7,
      padding: '4px 10px',
      background: 'var(--canvas-1)',
      border: '1px solid var(--line-soft)',
      borderRadius: 12,
      fontSize: 11.5,
    }} className={s.cls}>
      <span
        className={s.dot}
        style={kind === 'drained' ? { background: 'var(--ink-3)' } : undefined}
      />
      {s.label}
    </span>
  )
}

function Meta({
  label, value, mono, faint, tone,
}: { label: string; value: string; mono?: boolean; faint?: boolean; tone?: 'sage' | 'coral' | 'muted' }) {
  const toneCls = tone === 'sage' ? 'sage' : tone === 'coral' ? 'coral' : tone === 'muted' ? 'muted' : ''
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
      <div className="section-label" style={{ fontSize: 10.5, letterSpacing: '0.05em' }}>{label}</div>
      <div
        className={[mono ? 'mono' : '', faint ? 'faint' : '', toneCls].filter(Boolean).join(' ')}
        style={{ fontSize: 13, color: toneCls ? undefined : (faint ? 'var(--ink-3)' : 'var(--ink-1)') }}
      >
        {value}
      </div>
    </div>
  )
}

function CounterTile({
  label, value, sub, tone,
}: { label: string; value: number; sub?: string; tone?: 'sage' | 'coral' }) {
  const color = tone === 'sage' ? 'var(--sage)' : tone === 'coral' ? 'var(--coral)' : 'var(--ink-0)'
  return (
    <div className="card">
      <div className="card__body" style={{ padding: 16 }}>
        <div className="section-label" style={{ marginBottom: 8 }}>{label}</div>
        <div className="num" style={{ fontSize: 24, fontWeight: 500, color, letterSpacing: '-0.01em', lineHeight: 1.05 }}>
          {value.toLocaleString()}
        </div>
        {sub && <div className="faint" style={{ fontSize: 11, marginTop: 4 }}>{sub}</div>}
      </div>
    </div>
  )
}

function EventKind({ kind }: { kind: string }) {
  // Soft colour-coding mirrors the .lvl- pattern but for event kinds.
  const tone: 'sage' | 'coral' | 'brass' | '' =
    kind === 'completed' || kind === 'acked' ? 'sage' :
    kind === 'failed' || kind === 'protocol_violation' || kind === 'authz_rejected' || kind === 'authz_rejected_post_open' || kind === 'evicted' ? 'coral' :
    kind === 'revoked' || kind === 'drain_requested' || kind === 'drained_started' ? 'brass' :
    ''
  const cls = tone ? tone : 'ink-1'
  const bg = tone === 'sage' ? 'var(--sage-tint)' : tone === 'coral' ? 'var(--coral-tint)' : tone === 'brass' ? 'var(--accent-tint)' : 'var(--canvas-3)'
  return (
    <span
      className={'mono ' + cls}
      style={{
        display: 'inline-block',
        padding: '2px 8px',
        background: bg,
        borderRadius: 4,
        fontSize: 11,
        letterSpacing: '0.02em',
      }}
    >
      {kind}
    </span>
  )
}

function isStale(rfc3339: string): boolean {
  const ts = new Date(rfc3339).getTime()
  if (!Number.isFinite(ts)) return false
  return Date.now() - ts > 10_000
}

function relativeAgo(rfc3339: string): string {
  const ts = new Date(rfc3339).getTime()
  if (!Number.isFinite(ts)) return '—'
  const delta = Math.max(0, Math.floor((Date.now() - ts) / 1000))
  if (delta < 60) return `${delta}s ago`
  if (delta < 3600) return `${Math.floor(delta / 60)}m ago`
  if (delta < 86400) return `${Math.floor(delta / 3600)}h ago`
  return `${Math.floor(delta / 86400)}d ago`
}
