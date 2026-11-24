// What the confirm pages for @wakeshift/cli's links share: the key code
// to compare with the terminal, the errors in words for the person, and
// "Done".
import { useEffect, useState, type ReactNode } from 'react'
import { useLocation } from 'react-router-dom'
import { ApiError } from '../api/client'
import { keyCode, parseLink, publicKeyBytes, takeLink, type CLILink } from '../lib/cliLink'
import { Card, Notice } from './ui'

// useCLILink is the CLI link this page was opened with (kept across
// signing in), or a new one pasted into the address bar while it's open.
export function useCLILink(): CLILink | null {
  const { hash } = useLocation()
  const [link, setLink] = useState(takeLink)
  const [seen, setSeen] = useState(hash)
  if (hash !== seen) {
    setSeen(hash)
    const l = parseLink(hash)
    if (l) setLink(l)
  }
  return link
}

// useKeyCode is the link's key code, or invalid for a public key that can't
// be one (cut short, mistyped).
export function useKeyCode(pk: string): { code?: string; invalid: boolean } {
  const bytes = publicKeyBytes(pk)
  const [code, setCode] = useState<string>()
  useEffect(() => {
    let live = true
    if (bytes) keyCode(bytes).then((c) => live && setCode(c))
    return () => {
      live = false
    }
  }, [pk]) // bytes is derived from pk
  return { code, invalid: bytes === null }
}

// KeyCode shows the code for the person to check against their terminal, or
// against their coding agent's message when the agent ran the command.
export function KeyCode({ code }: { code?: string }) {
  return (
    <div className="rounded-lg border border-accent/40 bg-accent-soft/40 px-4 py-3">
      <p className="text-sm text-ink-2">
        Key code: <code className="ml-1 rounded bg-card px-1.5 py-0.5 font-mono text-base font-semibold tracking-wide text-ink">{code ?? '…'}</code>
      </p>
      <p className="mt-1 text-sm text-ink-2">Check that it matches the key code in your terminal or your coding agent's message. Only continue if you or your agent just ran the command.</p>
    </div>
  )
}

export const invalidLink = (
  <Notice tone="caution" title="This link's key isn't valid">
    It may have been cut short. Copy the whole link from your terminal, or run the command again.
  </Notice>
)

// confirmError says what went wrong with registering the key, for the
// person, by the API's error code.
export function confirmError(err: Error | null): ReactNode {
  if (!err) return null
  const code = err instanceof ApiError ? err.code : ''
  const message: Record<string, ReactNode> = {
    key_used: "This key is registered already, so it can't be added again. Run the command again in your terminal to make a new key.",
    invalid_key: "This link's key isn't valid. Copy the whole link from your terminal, or run the command again.",
    forbidden: "Your role in this organization doesn't allow that. Ask an owner or admin, or pick another organization.",
    exists: 'That app ID is taken. Pick another, then create the app.',
  }
  return (
    <p role="alert" className="mt-2 text-sm text-danger">
      {message[code] ?? err.message}
    </p>
  )
}

// Done is the page after the key is registered: the CLI, polling, carries
// on by itself.
export function Done({ title, children }: { title: string; children?: ReactNode }) {
  return (
    <Card className="border-sage/40">
      <p className="font-serif text-2xl text-ink">Done: go back to your terminal</p>
      <p className="mt-2 text-sm leading-relaxed text-ink-2">{title}</p>
      {children && <div className="mt-4">{children}</div>}
    </Card>
  )
}
