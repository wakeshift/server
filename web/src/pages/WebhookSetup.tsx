import { useCallback, useEffect, useRef, useState, type FormEvent, type ReactNode } from 'react'
import { Button, Input, Logo, useTitle } from '../components/ui'

// The setup page for a Grok Bot's Automations webhook. Public: no
// sign-in, no analytics, no cookies sent. The token is in the link's
// fragment (#t=…), so it never reaches a request log, a proxy or a
// link-preview fetch; it goes to the server only in a POST body. The page
// keeps no state of its own: once wake-ups are on it can be closed, and
// the user stops them by asking the bot (or deleting the routine).

type Described = {
  app: string
  bot_tail: string
  expires_at: string
  state: 'open' | 'awaiting_proof' | 'on'
  error?: string
}

class SetupError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
  ) {
    super(code)
  }
}

async function post<T>(path: string, body: unknown): Promise<T> {
  const res = await fetch(path, {
    method: 'POST',
    credentials: 'omit',
    referrerPolicy: 'no-referrer',
    headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
    body: JSON.stringify(body),
  })
  const out = await res.json().catch(() => null)
  if (!res.ok) throw new SetupError(res.status, out?.error?.code ?? 'http_error')
  return out as T
}

const fieldErrors: Record<string, string> = {
  invalid_url:
    "This isn't an Automations webhook URL. It starts with https://api2.cursor.sh/automations/webhook/. Copy it again from the routine's webhook trigger.",
  invalid_key: "This doesn't look like the webhook's key. Copy it again from the routine's webhook trigger.",
  webhook_unauthorized: "The webhook didn't accept this key. Copy the key again from the routine's webhook trigger.",
  webhook_not_found: "This webhook doesn't exist. Check that the routine still exists and that its webhook trigger is on, then copy the URL again.",
  webhook_unanswered: "The webhook didn't answer. Try again in a minute.",
  awaiting_proof: 'Wakeshift already sent your bot a check. Wait for it to answer.',
  rate_limited: 'Too many tries from here. Try again later.',
  too_many_tries: 'This link has been tried too often. Try again later.',
  too_many_webhooks: "You've set up as many webhooks as you can today. Try again tomorrow.",
  // What the last attempt ended with (describe's error).
  unauthorized: "The webhook didn't accept this key. Copy the key again from the routine's webhook trigger.",
  not_found: "This webhook doesn't exist. Check that the routine still exists and that its webhook trigger is on, then copy the URL again.",
  unanswered: "The webhook didn't answer. Try again in a minute.",
  key_missing:
    "Your bot's Wakeshift key is missing, which happens when its VM is reset, so it can't confirm this webhook. Ask your bot to set up wake-ups again: it makes a new key and gives you a new link.",
  unconfirmed:
    'Your bot didn\'t confirm this webhook. Check that you copied the webhook of the routine "Wakeshift wake-ups" in the bot that sent you here, then try again.',
}

const linkGone = "This link no longer works: it was used, it expired, or it isn't valid. Ask your bot for a new one."

function fragment(): string {
  return new URLSearchParams(window.location.hash.slice(1)).get('t') ?? ''
}

function time(iso: string) {
  return new Date(iso).toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' })
}

export default function WebhookSetup() {
  const [token] = useState(fragment)
  const [info, setInfo] = useState<Described | null>(null)
  const [gone, setGone] = useState(false)
  const [loadFailed, setLoadFailed] = useState(false)
  const [url, setURL] = useState('')
  const [key, setKey] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const [notNow, setNotNow] = useState(false)
  const timer = useRef<number | undefined>(undefined)
  useTitle(info ? `${info.app}: wake your Grok Bot` : 'Wake your Grok Bot')

  const describe = useCallback(async () => {
    if (!token) return
    try {
      const d = await post<Described>('/v1/stream/webhook-setup/describe', { token })
      setInfo(d)
      setLoadFailed(false)
      if (d.state === 'open' && d.error) setError(fieldErrors[d.error] ?? fieldErrors.unconfirmed)
    } catch (e) {
      if (e instanceof SetupError && e.status === 401) setGone(true)
      else if (e instanceof SetupError && e.status === 429) return // polled too often: the next look will do
      else setLoadFailed(true)
    }
  }, [token])

  useEffect(() => {
    describe()
  }, [describe])

  // While the bot answers Wakeshift's check, look again every few seconds:
  // every 6, so the check's 10 minutes stay under describe's limit of 120
  // per address per 10 minutes.
  useEffect(() => {
    if (info?.state !== 'awaiting_proof') return
    timer.current = window.setInterval(describe, 6000)
    return () => window.clearInterval(timer.current)
  }, [info?.state, describe])

  async function save(e: FormEvent) {
    e.preventDefault()
    setError('')
    setBusy(true)
    try {
      await post('/v1/stream/webhook-setup', { token, url: url.trim(), key: key.trim() })
      setKey('')
      setInfo((i) => (i ? { ...i, state: 'awaiting_proof', error: undefined } : i))
    } catch (err) {
      if (err instanceof SetupError && err.status === 401) setGone(true)
      else setError(err instanceof SetupError ? (fieldErrors[err.code] ?? fieldErrors.webhook_unanswered) : fieldErrors.webhook_unanswered)
    } finally {
      setBusy(false)
    }
  }

  let body
  if (info?.state === 'on') {
    body = (
      <>
        <Title>Wake-ups are on</Title>
        <P>
          Your bot confirmed the webhook. New messages from <b className="text-ink">{info.app}</b> now wake it, even while it's idle. You can close this page.
        </P>
        <P>To stop wake-ups, ask your bot to stop watching, or delete the routine "Wakeshift wake-ups" in Grok.</P>
      </>
    )
  } else if (!token || gone) {
    body = (
      <>
        <Title>Wake your Grok Bot</Title>
        <P>{linkGone}</P>
      </>
    )
  } else if (loadFailed) {
    body = (
      <>
        <Title>Wake your Grok Bot</Title>
        <P>Wakeshift didn't answer. Try again in a minute.</P>
      </>
    )
  } else if (!info) {
    body = <p className="text-sm text-ink-3">Loading…</p>
  } else if (notNow) {
    body = (
      <>
        <Title>Wake your Grok Bot</Title>
        <P>Nothing was saved. Your bot can still wait for {info.app}'s messages while you're in the chat.</P>
      </>
    )
  } else if (info.state === 'awaiting_proof') {
    body = (
      <>
        <Title>Waiting for your bot</Title>
        <P>
          The webhook accepted Wakeshift's check. Your bot answers it in a moment, usually within half a minute, and this page shows when wake-ups are on.
        </P>
      </>
    )
  } else {
    body = (
      <>
        <Title>Wake your Grok Bot</Title>
        <P>
          Your Grok Bot asked to be woken when <b className="text-ink">{info.app}</b> sends it something. Wakeshift does that by calling the webhook of the routine
          your bot just created. Paste the routine's webhook URL and key below. Your bot then confirms the webhook is its own, and only its messages go there.
        </P>
        <p className="mt-3 text-xs leading-relaxed text-ink-3">
          App: {info.app} · Bot ending in <code className="font-mono">…{info.bot_tail}</code> · This link works once, until {time(info.expires_at)}.
        </p>
        <P>
          <b className="text-ink">Where to find them:</b> in the bot, open Automations, then the routine "Wakeshift wake-ups", then its webhook trigger. The URL
          starts with <code className="font-mono text-[13px]">https://api2.cursor.sh/automations/webhook/</code>.
        </P>
        <form className="mt-5 space-y-4" onSubmit={save} autoComplete="off">
          <Input label="Webhook URL" value={url} onChange={(e) => setURL(e.target.value)} spellCheck={false} autoComplete="off" required />
          <Input label="Key" type="password" value={key} onChange={(e) => setKey(e.target.value)} autoComplete="new-password" spellCheck={false} required />
          {error && <p className="text-sm text-danger">{error}</p>}
          <p className="text-xs leading-relaxed text-ink-3">
            Wakeshift keeps the key encrypted and uses it only to send this bot's messages to it. Each message starts a run in the bot, which counts toward your
            usage. To stop wake-ups at any time, ask your bot to stop watching, or delete the routine "Wakeshift wake-ups" in Grok.
          </p>
          <div className="flex items-center gap-3">
            <Button type="submit" disabled={busy}>
              Turn on wake-ups
            </Button>
            <Button type="button" variant="secondary" onClick={() => setNotNow(true)}>
              Not now
            </Button>
          </div>
        </form>
      </>
    )
  }

  return (
    <div className="flex min-h-screen flex-col items-center px-4 pb-10 pt-14 sm:pt-20">
      {info && (
        <p className="mb-5 font-serif text-[22px] leading-tight text-ink" data-testid="app-name">
          {info.app}
        </p>
      )}
      <div className="w-full max-w-120 rounded-2xl border border-rule bg-card p-6 shadow-[0_12px_30px_-18px_rgba(60,40,10,0.25)] sm:p-7">{body}</div>
      <div className="mt-6 flex items-center gap-2 text-xs text-ink-3">
        <span>Powered by</span>
        <Logo size="sm" />
      </div>
    </div>
  )
}

function Title({ children }: { children: ReactNode }) {
  return <h1 className="font-serif text-[26px] font-normal leading-tight tracking-[-0.01em]">{children}</h1>
}

function P({ children }: { children: ReactNode }) {
  return <p className="mt-3 text-[15px] leading-relaxed text-ink-2">{children}</p>
}
