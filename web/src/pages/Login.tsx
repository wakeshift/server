import { useEffect, useState, type FormEvent, type ReactNode } from 'react'
import { useMutation, useQuery } from '@tanstack/react-query'
import { useSearchParams } from 'react-router-dom'
import { useSession } from '../hooks/useSession'
import { checkInvite, holdWaitlistKey, saveWaitlistNote, type WaitlistEntry } from '../api/client'
import { Button, ErrorText, Input, Notice, Standalone, useTitle } from '../components/ui'
import { useSite, wakeshiftTerms } from '../lib/site'
import { keepLink, keptLink, keptNewAppLink } from '../lib/cliLink'
import { devTokenParam, keepDevToken, storedDevToken } from '../lib/devToken'

const errors: Record<string, string> = {
  bad_invite: 'That invite link is used up or invalid. You can still sign up without it.',
  account_exists: 'That email already belongs to an account that signs in another way. Use that method.',
  failed: "Signing in didn't work. Try again.",
  unavailable: "Couldn't reach the sign-in provider. Try again in a moment.",
}

// The longest note on a waitlist entry (accounts.MaxNote).
const maxNote = 500

function Provider({ href, onClick, children }: { href: string; onClick: () => void; children: ReactNode }) {
  return (
    <a
      href={href}
      onClick={onClick}
      className="flex w-full items-center justify-center gap-2 rounded-full border border-rule bg-card px-5 py-2.5 text-sm font-semibold text-ink transition-colors hover:border-ink-3"
    >
      {children}
    </a>
  )
}

export default function Login() {
  const { session, login } = useSession()
  // A link from @wakeshift/cli waits here for the sign-in, which returns to
  // its page without the fragment.
  useState(keepLink)
  const [email, setEmail] = useState('')
  const methods = session.data?.sign_in
  const waitlist = session.data?.waitlist
  const [params, setParams] = useSearchParams()
  // Development sign-in's token, from the link the server printed
  // (?dev_token=, taken out of the address bar) or entered by hand.
  const [devToken, setDevToken] = useState(() => params.get(devTokenParam) ?? storedDevToken())
  useEffect(() => {
    const fromLink = params.get(devTokenParam)
    if (fromLink === null) return
    keepDevToken(fromLink)
    const rest = new URLSearchParams(params)
    rest.delete(devTokenParam)
    setParams(rest, { replace: true })
  }, [params, setParams])
  const error = params.get('sign_in_error')
  const invite = params.get('invite') ?? ''
  // Where a link straight to the console came from (?ref=quickstart): sign-in
  // saves it on the account it makes (accounts.CleanRef checks it).
  const ref = params.get('ref') ?? ''
  const valid = useQuery({ queryKey: ['invite', invite], queryFn: () => checkInvite(invite), enabled: invite !== '' })
  useTitle(waitlist && !waitlist.invited ? 'Waitlist' : 'Sign in')
  const clearError = () => {
    if (error) setParams({ ...(invite && { invite }), ...(ref && { ref }) }, { replace: true })
  }

  const submit = (e: FormEvent) => {
    e.preventDefault()
    clearError()
    keepDevToken(devToken)
    login.mutate({ email, token: devToken })
  }

  // Back here afterwards (e.g. an agent's authorize page), without the
  // sign-in's own parameters.
  const back = new URLSearchParams(params)
  back.delete('sign_in_error')
  back.delete('invite')
  back.delete('ref')
  back.delete(devTokenParam)
  const returnTo = window.location.pathname + (back.size ? `?${back}` : '')
  const start = (provider: string) => {
    const q = new URLSearchParams({ return_to: returnTo })
    if (invite) q.set('invite', invite)
    if (ref) q.set('ref', ref)
    return `/auth/${provider}/start?${q}`
  }
  const providers = methods?.google || methods?.github
  const site = useSite()
  const buttons = (
    <>
      {methods?.google && (
        <Provider href={start('google')} onClick={clearError}>
          Continue with Google
        </Provider>
      )}
      {methods?.github && (
        <Provider href={start('github')} onClick={clearError}>
          Continue with GitHub
        </Provider>
      )}
    </>
  )

  if (waitlist && !waitlist.invited && !error) {
    return (
      <Standalone>
        <Waitlist entry={waitlist} />
        {providers && (
          <div className="mt-6 space-y-3 border-t border-rule pt-4">
            <p className="text-xs leading-relaxed text-ink-3">Signed in with the wrong account? Use another.</p>
            {buttons}
          </div>
        )}
      </Standalone>
    )
  }

  return (
    <Standalone>
      <h1 className="font-serif text-[28px] font-normal leading-tight tracking-[-0.01em]">
        {waitlist?.invited ? 'Your account is ready' : 'Sign in'}
      </h1>
      <p className="mt-2 text-sm leading-relaxed text-ink-2">
        {waitlist?.invited
          ? 'Sign in with the account you used to join the waitlist to start building.'
          : "Sign in or sign up to build apps that wake agents. You don't need an account to use an app in your agent."}
      </p>

      <div className="mt-5 space-y-3">
        {invite && valid.data?.valid && !error && (
          <Notice title="You're invited">Sign in to make your account.</Notice>
        )}
        {invite && valid.data && !valid.data.valid && !error && <Notice tone="caution">{errors.bad_invite}</Notice>}
        {error && <Notice tone="caution">{errors[error] ?? errors.failed}</Notice>}
        {buttons}
      </div>

      {methods?.dev && (
        <form onSubmit={submit} className={`space-y-3 ${providers ? 'mt-5 border-t border-rule pt-5' : 'mt-1'}`}>
          <Input label="Email" type="email" autoComplete="email" value={email} onChange={(e) => setEmail(e.target.value)} />
          <Input label="Token" type="password" autoComplete="off" value={devToken} onChange={(e) => setDevToken(e.target.value)} />
          <p className="text-xs text-ink-3">Development server: any email signs in, with the token the server printed when it started.</p>
          <ErrorText error={login.error} />
          <Button type="submit" disabled={login.isPending || !email || !devToken} className="w-full">
            Sign in
          </Button>
        </form>
      )}
      {methods && !methods.google && !methods.github && !methods.dev && (
        <p className="mt-5 text-sm text-ink-3">No sign-in method is configured on this server.</p>
      )}
      {(providers || methods?.dev) && site.terms && (
        <p className="mt-5 text-xs leading-relaxed text-ink-3">
          {wakeshiftTerms(site) ? <>By signing in, you agree to Wakeshift's </> : <>By signing in, you agree to the </>}
          <a className="underline underline-offset-2 hover:text-ink" href={site.terms}>Terms of Service</a>
          {site.privacy ? (
            <>
              {' '}and{' '}
              <a className="underline underline-offset-2 hover:text-ink" href={site.privacy}>Privacy Policy</a>
            </>
          ) : null}
          .
        </p>
      )}

      {!invite && !waitlist && providers && (
        <p className="mt-6 border-t border-rule pt-4 text-xs leading-relaxed text-ink-3">
          New here? Continue with Google or GitHub to sign up.
        </p>
      )}
    </Standalone>
  )
}

// Waitlist: a sign-in past the sign-up cap. No account yet; a note on what
// they'll build, kept with their place.
function Waitlist({ entry }: { entry: WaitlistEntry }) {
  const [note, setNote] = useState(entry.note)
  const [fromCLI] = useState(keptLink)
  // From init's link: tell Wakeshift the key is ours, so the terminal stops
  // waiting and says we're on the waitlist.
  const [cliLink] = useState(keptNewAppLink)
  const hold = useMutation({ mutationFn: holdWaitlistKey })
  const holdKey = hold.mutate
  useEffect(() => {
    if (cliLink) holdKey(cliLink.pk)
  }, [cliLink, holdKey])
  const save = useMutation({ mutationFn: saveWaitlistNote })
  const saved = save.isSuccess && save.variables === note
  const submit = (e: FormEvent) => {
    e.preventDefault()
    save.mutate(note)
  }
  return (
    <>
      <h1 className="font-serif text-[28px] font-normal leading-tight tracking-[-0.01em]">You're on the waitlist</h1>
      <p className="mt-2 text-sm leading-relaxed text-ink-2">We'll email you when your account is ready.</p>
      {fromCLI && (
        <div className="mt-4">
          <Notice title="Your app wasn't created">
            {hold.isSuccess ? "Your terminal knows you're on the waitlist. " : ''}Once your account is ready, run the command in your terminal again.
          </Notice>
        </div>
      )}
      <form onSubmit={submit} className="mt-5 space-y-3">
        <label className="block text-sm">
          <span className="mb-1.5 block font-medium text-ink-2">What will you build? (optional)</span>
          <textarea
            className="w-full rounded-lg border border-rule bg-card px-3 py-2 text-sm text-ink placeholder:text-ink-3 focus:border-accent focus:outline-hidden"
            rows={4}
            maxLength={maxNote}
            value={note}
            onChange={(e) => setNote(e.target.value)}
            placeholder="A bot that wakes my agent when a deploy fails"
          />
        </label>
        <div className="flex items-center justify-between gap-3">
          <span className="text-xs text-ink-3">
            {note.length} / {maxNote}
          </span>
          <Button type="submit" size="sm" disabled={save.isPending || note === (save.isSuccess ? save.variables : entry.note)}>
            {saved ? 'Saved' : 'Save'}
          </Button>
        </div>
        <ErrorText error={save.error} />
      </form>
    </>
  )
}
