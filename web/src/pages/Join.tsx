import { useEffect, useState, type FormEvent } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useNavigate, useSearchParams } from 'react-router-dom'
import { ApiError } from '../api/client'
import { acceptInvite, keepForSignIn, previewInvite, type Role } from '../api/orgs'
import { Button, ErrorText, Input, linkClass, Loading, Notice, Standalone, useTitle } from '../components/ui'
import { chooseOrg } from '../hooks/useOrg'
import { useSession } from '../hooks/useSession'
import { keepDevToken, storedDevToken } from '../lib/devToken'
import { useSite, wakeshiftTerms } from '../lib/site'

// The invite's token arrives in the link's fragment (/join#<token>), which
// never reaches a server. It's kept for this tab while the person signs in
// (sessionStorage), and dropped from the address bar.
const stored = 'wakeshift.join'

function takeToken(): string {
  const fromHash = window.location.hash.slice(1)
  if (fromHash) {
    try {
      sessionStorage.setItem(stored, fromHash)
    } catch {
      // Private mode: it lasts this page only.
    }
    window.history.replaceState(null, '', window.location.pathname + window.location.search)
    return fromHash
  }
  try {
    return sessionStorage.getItem(stored) ?? ''
  } catch {
    return ''
  }
}

const signInErrors: Record<string, string> = {
  org_invite_email: "That account's email isn't the address this invite was sent to. Sign in with the account for that address.",
  org_invite_invalid: "This invite isn't valid any more. Ask for a new one.",
  org_invite_inviter: "Whoever invited you can't any more. Ask them for a new invite.",
  org_invite_no_signups:
    'Whoever invited you has no sign-ups left, so this invite can’t make a new account. Ask them to contact Wakeshift, or sign in with an account you already have.',
  org_full: 'This organization is full. Ask whoever invited you.',
  account_exists: 'That email already belongs to an account that signs in another way. Use that method.',
  failed: "Signing in didn't work. Try again.",
  unavailable: "Couldn't reach the sign-in provider. Try again in a moment.",
}

const article = (r: Role) => (r === 'developer' ? 'a' : 'an')

// Join: what an invite is for, and joining (signing in or up first if
// needed). Mail scanners fetch links but don't press buttons, so nothing
// happens until Join is pressed.
export default function Join() {
  useTitle('Join')
  const qc = useQueryClient()
  const navigate = useNavigate()
  const { session, login, logout } = useSession()
  const [token] = useState(takeToken)
  const [params, setParams] = useSearchParams()
  const signInError = params.get('sign_in_error')
  const [email, setEmail] = useState('')
  const [devToken, setDevToken] = useState(storedDevToken)
  const preview = useQuery({ queryKey: ['join-preview', token], queryFn: () => previewInvite(token), enabled: token !== '', retry: false })
  const join = useMutation({
    mutationFn: () => acceptInvite(token),
    onSuccess: async ({ org_id }) => {
      try {
        sessionStorage.removeItem(stored)
      } catch {
        // Nothing kept.
      }
      chooseOrg(org_id)
      await qc.invalidateQueries({ queryKey: ['orgs'] })
      navigate('/')
    },
  })
  const signIn = useMutation({
    mutationFn: async (provider: string) => {
      await keepForSignIn(token)
      window.location.href = `/auth/${provider}/start?${new URLSearchParams({ return_to: '/join' })}`
    },
  })
  useEffect(() => {
    if (join.error instanceof ApiError && join.error.code === 'already_member') navigate('/')
  }, [join.error, navigate])

  const s = session.data
  const methods = s?.sign_in
  const site = useSite()
  const wrong = join.error instanceof ApiError && join.error.code === 'wrong_account'

  return (
    <Standalone wide>
      <h1 className="font-serif text-[28px] font-normal leading-tight tracking-[-0.01em]">Join an organization</h1>
      {!token && <p className="mt-3 text-sm leading-relaxed text-ink-2">Open the link from your invite email to join.</p>}
      {token && preview.isPending && <Loading />}
      {token && preview.isError && (
        <div className="mt-4">
          <Notice tone="caution">
            {preview.error instanceof ApiError && preview.error.status === 404 ? "This invite isn't valid any more. Ask for a new one." : preview.error.message}
          </Notice>
        </div>
      )}
      {preview.data && (
        <>
          <p className="mt-3 text-[15px] leading-relaxed text-ink-2">
            <b className="text-ink">{preview.data.invited_by}</b> invited you to join <b className="text-ink">{preview.data.org_name}</b> as{' '}
            {article(preview.data.role)} {preview.data.role}.
          </p>
          <div className="mt-5 space-y-3">
            {signInError && <Notice tone="caution">{signInErrors[signInError] ?? signInErrors.failed}</Notice>}
            {wrong && (
              <Notice tone="caution" title="Another account">
                {join.error?.message}
              </Notice>
            )}
            {s?.authenticated && !wrong && (
              <>
                <p className="text-sm text-ink-2">
                  Signed in as <b className="text-ink">{s.user?.email}</b>.
                </p>
                <Button
                  className="w-full"
                  disabled={join.isPending}
                  onClick={() => {
                    if (signInError) setParams({}, { replace: true })
                    join.mutate()
                  }}
                >
                  Join {preview.data.org_name}
                </Button>
                {join.error && !(join.error instanceof ApiError && join.error.code === 'already_member') && <ErrorText error={join.error} />}
              </>
            )}
            {s?.authenticated && wrong && (
              <Button className="w-full" variant="secondary" disabled={logout.isPending} onClick={() => logout.mutate(undefined, { onSuccess: () => join.reset() })}>
                Sign out
              </Button>
            )}
            {!s?.authenticated && (
              <>
                <p className="text-sm leading-relaxed text-ink-2">Sign in with the account for the address the invite was sent to. No account yet? This makes one.</p>
                {methods?.google && (
                  <Button className="w-full" variant="secondary" disabled={signIn.isPending} onClick={() => signIn.mutate('google')}>
                    Continue with Google
                  </Button>
                )}
                {methods?.github && (
                  <Button className="w-full" variant="secondary" disabled={signIn.isPending} onClick={() => signIn.mutate('github')}>
                    Continue with GitHub
                  </Button>
                )}
                {methods?.dev && (
                  <form
                    className="space-y-3 border-t border-rule pt-4"
                    onSubmit={(e: FormEvent) => {
                      e.preventDefault()
                      keepDevToken(devToken)
                      login.mutate({ email, token: devToken })
                    }}
                  >
                    <Input label="Email" type="email" value={email} onChange={(e) => setEmail(e.target.value)} />
                    <Input label="Token" type="password" autoComplete="off" value={devToken} onChange={(e) => setDevToken(e.target.value)} />
                    <p className="text-xs text-ink-3">Development server: any email signs in, with the token the server printed when it started.</p>
                    <Button type="submit" className="w-full" disabled={!email || !devToken || login.isPending}>
                      Sign in
                    </Button>
                  </form>
                )}
                <ErrorText error={signIn.error ?? login.error} />
              </>
            )}
          </div>
          {site.terms && (
            <p className="mt-6 border-t border-rule pt-4 text-xs leading-relaxed text-ink-3">
              {wakeshiftTerms(site) ? <>By joining, you agree to Wakeshift's </> : <>By joining, you agree to the </>}
              <a className={linkClass} href={site.terms}>
                Terms of Service
              </a>
              {site.privacy ? (
                <>
                  {' '}
                  and{' '}
                  <a className={linkClass} href={site.privacy}>
                    Privacy Policy
                  </a>
                </>
              ) : null}
              .
            </p>
          )}
        </>
      )}
    </Standalone>
  )
}
