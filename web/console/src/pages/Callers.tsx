import { useEffect, useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { Check, Copy, Download, Key, Link2, Plus, Trash2, X } from 'lucide-react'
import { api, ApiError, queries, type CallerInfo, type IssueCertResponse } from '@/api/client'
import { useIsAdmin } from '@/hooks/useAuth'
import { PageShell } from '@/components/PageShell'
import { HoverInfo } from '@/components/HoverInfo'
import { SudoConfirmDialog } from './Settings'

function fmtDateShort(iso?: string) {
  if (!iso) return '—'
  const d = new Date(iso)
  if (isNaN(d.getTime())) return '—'
  return d.toISOString().slice(0, 10)
}

const CALLER_NAME_RE = /^[a-z0-9][a-z0-9-]{0,62}[a-z0-9]$|^[a-z0-9]$/

// Until the BFF exposes per-caller cert expiry, the meter renders an
// "unknown" state — same chrome, no fill. The day classes still work
// when expiry data arrives (additive change).
function certClass(days?: number) {
  if (days == null) return ''
  return days < 14 ? 'is-exp' : days < 60 ? 'is-soon' : ''
}
function certLabel(days?: number) {
  if (days == null) return 'unknown'
  return days < 14 ? 'expiring' : days < 60 ? 'renew soon' : 'valid'
}

// ── Page ──────────────────────────────────────────────────────────────────
export function Callers() {
  const qc = useQueryClient()
  const isAdmin = useIsAdmin()
  const { data, isLoading, error } = useQuery(queries.callers())
  const { data: instance } = useQuery(queries.instance())
  const [addOpen, setAddOpen] = useState(false)
  const [revoking, setRevoking] = useState<string | null>(null)
  const [certBundle, setCertBundle] = useState<{ name: string; bundle: IssueCertResponse } | null>(null)
  const [issuingCaller, setIssuingCaller] = useState<string | null>(null)
  const [aliasEditing, setAliasEditing] = useState<string | null>(null)
  const [toast, setToast] = useState<string | null>(null)

  // The design's pages.css gates .callergrid and .callertable behind a
  // :root[data-callers="cards"] attribute. Cards is the canonical view
  // here; setting the attribute reveals the grid layout and hides the
  // table fallback.
  useEffect(() => {
    document.documentElement.setAttribute('data-callers', 'cards')
    return () => { document.documentElement.removeAttribute('data-callers') }
  }, [])

  const showToast = (msg: string) => {
    setToast(msg)
    setTimeout(() => setToast(null), 2400)
  }

  const issueM = useMutation({
    mutationFn: (name: string) => api.callers.issueCert(name),
    onSuccess: (bundle, name) => {
      setIssuingCaller(null)
      setCertBundle({ name, bundle })
    },
    onError: () => setIssuingCaller(null),
  })

  const revokeM = useMutation({
    mutationFn: (name: string) => api.callers.revoke(name),
    onSuccess: (_, name) => {
      setRevoking(null)
      qc.invalidateQueries({ queryKey: ['callers'] })
      showToast(`Caller revoked: ${name}`)
    },
  })

  const registerM = useMutation({
    mutationFn: ({ name, canMutate }: { name: string; canMutate: boolean }) =>
      api.callers.register(name, canMutate),
    onSuccess: (_, vars) => {
      qc.invalidateQueries({ queryKey: ['callers'] })
      setAddOpen(false)
      showToast(`Registered caller: ${vars.name}`)
    },
  })

  return (
    <PageShell
      title="Callers"
      sub="registered identities · mTLS"
      action={isAdmin && (
        <button className="btn btn--brass" onClick={() => setAddOpen(true)}>
          <Plus size={14} />
          <span>Add caller</span>
        </button>
      )}
    >
        <div className="page__bodyinner">
          {error && (
            <div className="banner banner--error" style={{ marginBottom: 16 }}>
              {(error as Error).message}
            </div>
          )}

          {isLoading ? (
            <div
              className="callergrid"
              style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(300px, 1fr))', gap: 14 }}
            >
              {[0, 1, 2, 3, 4, 5].map(i => (
                <div key={i} className="sk" style={{ height: 184 }} />
              ))}
            </div>
          ) : !data?.callers?.length ? (
            <div className="empty">
              <div className="empty__title">No callers registered</div>
              <div className="empty__sub">
                Run <span className="mono">tide apply</span> from a caller repo, or click
                <span className="brass" style={{ marginLeft: 4 }}>Add caller</span> above to register one.
              </div>
            </div>
          ) : (
            <div className="callergrid">
              {data.callers.map(c => (
                <CallerCard
                  key={c.caller}
                  caller={c}
                  canAdmin={isAdmin}
                  onIssue={() => { setIssuingCaller(c.caller); issueM.mutate(c.caller) }}
                  isIssuing={issuingCaller === c.caller}
                  onRevoke={() => setRevoking(c.caller)}
                  onManageAliases={() => setAliasEditing(c.caller)}
                />
              ))}
            </div>
          )}
        </div>

      {addOpen && (
        <AddDialog
          onCancel={() => setAddOpen(false)}
          onRegister={(name, canMutate) => registerM.mutate({ name, canMutate })}
          isPending={registerM.isPending}
          errorMsg={registerM.error ? (registerM.error as Error).message : null}
        />
      )}

      {revoking && (
        <RevokeDialog
          name={revoking}
          isPending={revokeM.isPending}
          onCancel={() => setRevoking(null)}
          onConfirm={() => revokeM.mutate(revoking)}
        />
      )}

      {certBundle && (
        <CertDialog
          name={certBundle.name}
          bundle={certBundle.bundle}
          endpoint={instance?.endpoint}
          onClose={() => setCertBundle(null)}
          showToast={showToast}
        />
      )}

      {aliasEditing && (
        <AliasesDialog
          caller={aliasEditing}
          onClose={(saved) => {
            setAliasEditing(null)
            if (saved) showToast(`Aliases updated: ${aliasEditing}`)
          }}
        />
      )}

      {toast && (
        <div className="toast-wrap">
          <div className="toast">
            <span className="dot" />
            <span>{toast}</span>
          </div>
        </div>
      )}
    </PageShell>
  )
}

// ── Single card (design's .callercard) ──────────────────────────────────
function CallerCard({
  caller,
  canAdmin,
  onIssue,
  isIssuing,
  onRevoke,
  onManageAliases,
}: {
  caller: CallerInfo
  canAdmin: boolean
  onIssue: () => void
  isIssuing: boolean
  onRevoke: () => void
  onManageAliases: () => void
}) {
  // cert_expires_at is populated whenever a cert is issued through the
  // console — see `RecordCallerCertExpiry` on the server. Callers that
  // were minted out-of-band (e.g. `make self-host-caller-cert`) have an
  // absent value and the meter renders in its "unknown" state.
  const certDays = (() => {
    if (!caller.cert_expires_at) return undefined
    const exp = new Date(caller.cert_expires_at).getTime()
    if (isNaN(exp)) return undefined
    return Math.max(0, Math.round((exp - Date.now()) / 86_400_000))
  })()
  const certTotal = 365
  const pct = certDays != null
    ? Math.max(0, Math.min(100, Math.round((certDays / certTotal) * 100)))
    : 0

  const versionStr = caller.schema_version
    ? `v${String(caller.schema_version).padStart(4, '0')}`
    : '—'

  return (
    <div className="callercard" data-caller={caller.caller}>
      <div className="callercard__top">
        <span className="dot dot--brass" />
        <span className="callercard__name">{caller.caller}</span>
        {!caller.can_mutate && (
          <span className="badge badge--plain">read-only</span>
        )}
        {caller.registered === false && (
          <span className="badge badge--plain">implicit</span>
        )}
        <span className="spacer" style={{ flex: 1 }} />
        {canAdmin && (
          <>
            <HoverInfo
              side="bottom"
              inline
              content={
                <>
                  <p>Configure identity aliases — let this cert satisfy <code className="mono">visible_to</code> for other names.</p>
                  <p className="hi-foot">Operator-controlled. Auth still uses the cert CN; aliases only widen what visible_to predicates match.</p>
                </>
              }
            >
              <button
                className="btn btn--sm btn--ghost btn--icon"
                onClick={onManageAliases}
                aria-label="Manage aliases"
              >
                <Link2 size={13} />
              </button>
            </HoverInfo>
            <HoverInfo
              side="bottom"
              inline
              content={
                <>
                  <p>Mint a fresh client cert and private key. The bundle downloads once.</p>
                  <p className="hi-foot">Supersedes the previous cert — atlantis stops authenticating it.</p>
                </>
              }
            >
              <button
                className="btn btn--sm btn--ghost btn--icon"
                onClick={onIssue}
                disabled={isIssuing}
                aria-label="Re-issue cert"
              >
                {isIssuing ? <span className="spin" /> : <Key size={13} />}
              </button>
            </HoverInfo>
            <HoverInfo
              side="bottom"
              inline
              content={
                <>
                  <p>Drops this caller's identity and any files they registered.</p>
                  <p className="hi-foot">Every cert for this caller stops authenticating — reads and writes both fail.</p>
                </>
              }
            >
              <button
                className="btn btn--sm btn--danger btn--icon"
                onClick={onRevoke}
                aria-label="Revoke caller"
              >
                <Trash2 size={13} />
              </button>
            </HoverInfo>
          </>
        )}
      </div>

      <div className="callercard__grid">
        <div className="cstat">
          <span className="cstat__l">files</span>
          <span className="cstat__v">{caller.file_count || '—'}</span>
        </div>
        <div className="cstat">
          <span className="cstat__l">schema</span>
          <span className="cstat__v">{versionStr}</span>
        </div>
        <div className="cstat">
          <span className="cstat__l">last applied</span>
          <span className="cstat__v">{fmtDateShort(caller.last_applied_at)}</span>
        </div>
        <div className="cstat">
          <span className="cstat__l">can apply</span>
          <span className="cstat__v">{caller.can_mutate ? 'yes' : 'no'}</span>
        </div>
      </div>

      <div className={`cert-meter ${certClass(certDays)}`}>
        <div className="row" style={{ justifyContent: 'space-between' }}>
          <span className="cstat__l" style={{ whiteSpace: 'nowrap' }}>
            cert · {certLabel(certDays)}
          </span>
          <span
            className="mono num"
            style={{ fontSize: 11.5, color: 'var(--ink-2)', whiteSpace: 'nowrap' }}
          >
            {certDays != null ? `${certDays}d left` : '—'}
          </span>
        </div>
        <div className="cert-meter__bar">
          <div className="cert-meter__fill" style={{ width: `${pct}%` }} />
        </div>
      </div>
    </div>
  )
}

// ── Add caller modal (.modal pattern from console.css) ────────────────────
function AddDialog({
  onCancel,
  onRegister,
  isPending,
  errorMsg,
}: {
  onCancel: () => void
  onRegister: (name: string, canMutate: boolean) => void
  isPending: boolean
  errorMsg: string | null
}) {
  const [name, setName] = useState('')
  const [canMutate, setCanMutate] = useState(true)
  const valid = !name || CALLER_NAME_RE.test(name.trim())

  return (
    <div className="overlay is-open" onMouseDown={e => { if (e.target === e.currentTarget) onCancel() }}>
      <div className="modal" role="dialog" aria-modal>
        <div className="modal__head">
          <div className="modal__title">Register caller</div>
          <div className="modal__sub">A caller owns a namespace and connects over mTLS.</div>
        </div>
        <div className="modal__body">
          <div className="field">
            <label className="field__label">Caller name</label>
            <input
              className={`input mono ${!valid ? '' : ''}`}
              placeholder="service-name"
              value={name}
              onChange={e => setName(e.target.value.toLowerCase())}
              autoFocus
            />
            {!valid && (
              <span className="coral" style={{ fontSize: 11.5, marginTop: 4 }}>
                Lowercase letters, digits, interior hyphens only.
              </span>
            )}
          </div>

          <label className="checkbox">
            <input type="checkbox" checked={canMutate} onChange={e => setCanMutate(e.target.checked)} />
            <span className="checkbox__box"><Check size={11} /></span>
            <span>
              Can apply schema
              <span className="faint" style={{ fontSize: 11.5, marginLeft: 4 }}>
                — grants <span className="mono">tide apply</span>
              </span>
            </span>
          </label>

          {errorMsg && (
            <div className="banner banner--error" style={{ marginTop: 2 }}>
              <span className="banner__icon" />
              <span>{errorMsg}</span>
            </div>
          )}
        </div>
        <div className="modal__foot">
          <button className="btn" onClick={onCancel}>Cancel</button>
          <button
            className="btn btn--brass"
            disabled={!name || !valid || isPending}
            onClick={() => onRegister(name.trim(), canMutate)}
          >
            <Plus size={14} />
            <span>{isPending ? 'Registering…' : 'Register'}</span>
          </button>
        </div>
      </div>
    </div>
  )
}

// ── Revoke confirmation ──────────────────────────────────────────────────
function RevokeDialog({
  name,
  isPending,
  onCancel,
  onConfirm,
}: {
  name: string
  isPending: boolean
  onCancel: () => void
  onConfirm: () => void
}) {
  return (
    <div className="overlay is-open" onMouseDown={e => { if (e.target === e.currentTarget) onCancel() }}>
      <div className="modal" role="dialog" aria-modal>
        <div className="modal__head">
          <div className="modal__title">Revoke caller?</div>
          <div className="modal__sub">
            Removes <span className="mono brass">{name}</span> from the identity table and clears
            all of its registered files. Every cert minted for this caller stops
            authenticating immediately — both reads and writes fail.
          </div>
        </div>
        <div className="modal__foot">
          <button className="btn" onClick={onCancel} disabled={isPending}>Cancel</button>
          <button className="btn btn--danger" onClick={onConfirm} disabled={isPending}>
            {isPending ? 'Revoking…' : 'Revoke caller'}
          </button>
        </div>
      </div>
    </div>
  )
}

// ── Cert download modal (design's certDialog) ─────────────────────────────
function CertDialog({
  name,
  bundle,
  endpoint,
  onClose,
  showToast,
}: {
  name: string
  bundle: IssueCertResponse
  endpoint?: string
  onClose: () => void
  showToast: (msg: string) => void
}) {
  const dl = (ext: string, body: string, desc: string) => {
    const blob = new Blob([body], { type: 'application/x-pem-file' })
    const a = document.createElement('a')
    a.href = URL.createObjectURL(blob)
    a.download = `${name}.${ext}`
    a.click()
    URL.revokeObjectURL(a.href)
    showToast(`Downloaded ${name}.${ext}`)
    void desc
  }

  const yaml = `caller: ${name}
endpoint: ${endpoint || '<ATL_ENDPOINT>'}
tls:
  cert: ./${name}.crt
  key:  ./${name}.key
  ca:   ./ca.crt`

  // expires_at comes from the signer; surface the real date rather than a
  // hardcoded TTL — the leaf lifetime is set in cmd/signer/main.go (currently
  // 90d) and may shift over time. The private key only exists in this tab's
  // memory until close; re-issuing rotates the cert and invalidates the prior
  // one on the next dial.
  const exp = new Date(bundle.expires_at)
  const expStr = isNaN(exp.getTime()) ? bundle.expires_at : exp.toISOString().slice(0, 10)

  return (
    <div className="overlay is-open" onMouseDown={e => { if (e.target === e.currentTarget) onClose() }}>
      <div className="modal" role="dialog" aria-modal style={{ width: 520 }}>
        <div className="modal__head">
          <div className="modal__title">Certificate issued — {name}</div>
          <div className="modal__sub">
            Expires {expStr}. Download the three files and store them in your secret
            manager — re-issuing rotates the cert and invalidates this one.
          </div>
        </div>
        <div className="modal__body">
          <div className="probe-test">
            <div className="probe-test__row" style={{ cursor: 'pointer' }} onClick={() => dl('crt', bundle.cert_pem, 'client certificate')}>
              <span className="probe-test__icon brass"><Download size={14} /></span>
              <span className="probe-test__label">{name}.crt</span>
              <span className="faint" style={{ fontSize: 11.5, marginLeft: 8 }}>client certificate</span>
              <span className="probe-test__status muted">download</span>
            </div>
            <div className="probe-test__row" style={{ cursor: 'pointer' }} onClick={() => dl('key', bundle.key_pem, 'private key')}>
              <span className="probe-test__icon brass"><Download size={14} /></span>
              <span className="probe-test__label">{name}.key</span>
              <span className="faint" style={{ fontSize: 11.5, marginLeft: 8 }}>private key</span>
              <span className="probe-test__status muted">download</span>
            </div>
            <div className="probe-test__row" style={{ cursor: 'pointer' }} onClick={() => dl('ca.crt', bundle.ca_pem, 'CA bundle')}>
              <span className="probe-test__icon brass"><Download size={14} /></span>
              <span className="probe-test__label">{name}.ca.crt</span>
              <span className="faint" style={{ fontSize: 11.5, marginLeft: 8 }}>CA bundle</span>
              <span className="probe-test__status muted">download</span>
            </div>
          </div>

          <div>
            <div className="section-label" style={{ marginBottom: 8 }}>tide.yaml</div>
            <pre style={{
              margin: 0, padding: '13px 15px', background: 'var(--canvas-0)',
              border: '1px solid var(--line-soft)', borderRadius: 'var(--radius)',
              fontFamily: 'var(--mono)', fontSize: 12, color: 'var(--ink-1)', lineHeight: 1.7,
              position: 'relative',
            }}>
              {yaml}
              <button
                className="btn btn--sm btn--ghost btn--icon"
                style={{ position: 'absolute', top: 9, right: 9 }}
                onClick={() => { navigator.clipboard.writeText(yaml); showToast('Copied tide.yaml') }}
              >
                <Copy size={13} />
              </button>
            </pre>
          </div>
        </div>
        <div className="modal__foot">
          <button className="btn btn--brass" onClick={onClose}>Done</button>
        </div>
      </div>
    </div>
  )
}

// ── Aliases dialog ──────────────────────────────────────────────────────
//
// Aliases let a registered caller's cert CN satisfy `visible_to`
// predicates declared for other identity names. PostgreSQL-roles /
// AD-SID / DNS-CNAME pattern: schemas reference roles, deploy-time
// config maps physical identities to those roles. Identity rename
// doesn't require schema edits.
//
// Aliases are operator-controlled (sudo-gated on save) and never
// substitute for authentication — they only widen the authz match set.
function AliasesDialog({
  caller,
  onClose,
}: {
  caller: string
  onClose: (saved: boolean) => void
}) {
  const qc = useQueryClient()
  const { data, isLoading, error } = useQuery({
    queryKey: ['caller-aliases', caller],
    queryFn: () => api.callers.aliases(caller),
  })

  const [draft, setDraft] = useState<string[] | null>(null)
  const [newAlias, setNewAlias] = useState('')
  const [showSudo, setShowSudo] = useState(false)
  const [saving, setSaving] = useState(false)
  const [saveError, setSaveError] = useState<string | null>(null)

  // Hydrate draft once data arrives. Subsequent loads (e.g. background
  // refetch) shouldn't clobber unsaved edits.
  useEffect(() => {
    if (data && draft === null) setDraft([...data.aliases])
  }, [data, draft])

  const current = draft ?? data?.aliases ?? []
  const dirty = data ? !arraysEqual(current, data.aliases) : false

  const onAdd = () => {
    const a = newAlias.trim()
    if (!a) return
    if (a === caller) {
      setSaveError(`Alias cannot equal the caller name (${caller}).`)
      return
    }
    if (current.includes(a)) {
      setNewAlias('')
      return
    }
    setDraft([...current, a])
    setNewAlias('')
    setSaveError(null)
  }

  const onRemove = (a: string) => {
    setDraft(current.filter(x => x !== a))
    setSaveError(null)
  }

  const save = async (password: string) => {
    setSaving(true)
    setSaveError(null)
    try {
      await api.auth.sudo(password)
      await api.callers.setAliases(caller, current)
      qc.invalidateQueries({ queryKey: ['caller-aliases', caller] })
      setShowSudo(false)
      onClose(true)
    } catch (e) {
      setSaveError(e instanceof ApiError ? e.message : 'save failed')
    } finally {
      setSaving(false)
    }
  }

  // Render sudo gate alone (not stacked) when the operator hits Save —
  // mirrors the Settings page's "one modal at a time" pattern. Aliases
  // dialog state is kept in this closure so cancel-from-sudo returns
  // the operator to the same editing state, not a fresh load.
  if (showSudo) {
    return (
      <SudoConfirmDialog
        title="Save aliases"
        icon={<Link2 size={18} />}
        body={
          <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
            <p>
              Replacing the alias set for <span className="mono">{caller}</span>.
              {' '}This widens (or narrows) the <code className="mono">visible_to</code> match set
              immediately for all future authz checks.
            </p>
            <p className="muted" style={{ fontSize: 12 }}>
              Cert authentication is unchanged. Aliases only affect what visible_to predicates this
              cert satisfies at job-dispatch authz time.
            </p>
          </div>
        }
        confirmLabel="Save"
        pending={saving}
        error={saveError}
        onCancel={() => setShowSudo(false)}
        onConfirm={save}
      />
    )
  }

  return (
    <div
      className="overlay is-open"
      onMouseDown={e => { if (e.target === e.currentTarget) onClose(false) }}
    >
      <div className="modal" style={{ width: 520 }} role="dialog" aria-modal>
          <div className="modal__head">
            <div className="row" style={{ gap: 10, alignItems: 'center' }}>
              <Link2 size={16} />
              <span className="modal__title">Identity aliases</span>
            </div>
            <div className="modal__sub">
              <span className="mono">{caller}</span> — additional names this cert satisfies for{' '}
              <code className="mono">visible_to</code>
            </div>
          </div>
          <div className="modal__body">
            {isLoading && <div className="muted">Loading…</div>}
            {error && (
              <div className="banner banner--error">
                {(error as Error).message}
              </div>
            )}

            {!isLoading && !error && (
              <>
                <div className="field">
                  <label className="field__label">Current aliases</label>
                  {current.length === 0 ? (
                    <div className="faint" style={{ fontSize: 12, padding: '6px 0' }}>
                      No aliases. Only <span className="mono">visible_to "{caller}"</span> and{' '}
                      <span className="mono">visible_to "*"</span> match this cert today.
                    </div>
                  ) : (
                    <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6 }}>
                      {current.map(a => (
                        <span
                          key={a}
                          className="badge badge--plain mono"
                          style={{
                            display: 'inline-flex',
                            alignItems: 'center',
                            gap: 6,
                            padding: '4px 4px 4px 10px',
                            background: 'var(--canvas-3)',
                            border: '1px solid var(--line-soft)',
                            borderRadius: 14,
                          }}
                        >
                          {a}
                          <button
                            type="button"
                            onClick={() => onRemove(a)}
                            className="btn btn--ghost btn--icon"
                            style={{ width: 18, height: 18, padding: 0, color: 'var(--ink-3)' }}
                            aria-label={`Remove alias ${a}`}
                          >
                            <X size={11} />
                          </button>
                        </span>
                      ))}
                    </div>
                  )}
                </div>

                <div className="field">
                  <label className="field__label">Add alias</label>
                  <div className="row" style={{ gap: 6 }}>
                    <input
                      className="input mono"
                      value={newAlias}
                      onChange={e => setNewAlias(e.target.value)}
                      onKeyDown={e => { if (e.key === 'Enter') onAdd() }}
                      placeholder="e.g. vendor"
                      autoFocus
                      spellCheck={false}
                      autoComplete="off"
                      style={{ flex: 1 }}
                    />
                    <button
                      className="btn btn--ghost btn--sm"
                      onClick={onAdd}
                      disabled={!newAlias.trim()}
                    >
                      <Plus size={12} /> Add
                    </button>
                  </div>
                  <div className="faint" style={{ fontSize: 11, marginTop: 6, lineHeight: 1.5 }}>
                    A schema declaration of <span className="mono">visible_to "X"</span> matches when
                    the cert CN is X or X appears in this alias list. Reserved names (atlantis*,
                    anonymous, *) are rejected.
                  </div>
                </div>

                {saveError && (
                  <div className="banner banner--error">{saveError}</div>
                )}
              </>
            )}
          </div>
          <div className="modal__foot">
            <button
              className="btn btn--ghost"
              onClick={() => onClose(false)}
              disabled={saving}
            >
              Cancel
            </button>
            <button
              className="btn btn--brass"
              onClick={() => { setSaveError(null); setShowSudo(true) }}
              disabled={!dirty || isLoading}
            >
              {dirty ? 'Save changes' : 'No changes'}
            </button>
          </div>
        </div>
      </div>
  )
}

function arraysEqual(a: string[], b: string[]): boolean {
  if (a.length !== b.length) return false
  const as = [...a].sort()
  const bs = [...b].sort()
  for (let i = 0; i < as.length; i++) {
    if (as[i] !== bs[i]) return false
  }
  return true
}
