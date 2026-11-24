// What the confirm pages for @wakeshift/cli's links share: typing the key code
// from the terminal, the errors in words for the person, and
// "Done".
import { useEffect, useState, type ReactNode } from 'react'
import { useLocation } from 'react-router-dom'
import { ApiError } from '../api/client'
import { keyCode, parseLink, publicKeyBytes, takeLink, type CLILink } from '../lib/cliLink'
import { Card, Input, Notice } from './ui'

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

// codeMatches says whether what the person typed is the link's key code
// (spaces around it and case aside, as the server compares).
export const codeMatches = (typed: string, code?: string) => !!code && typed.trim().toLowerCase() === code.toLowerCase()

// TypeKeyCode asks the person to type the key code their terminal (or their
// coding agent's message) shows, under a warning. The page never shows the
// code: a link someone else sent can't be confirmed by reading it off the
// page, only by having the terminal that made it. The server checks the
// code again.
export function TypeKeyCode({ value, onChange, code }: { value: string; onChange: (v: string) => void; code?: string }) {
  const typed = value.trim() !== ''
  const wrong = typed && value.trim().length >= 8 && !codeMatches(value, code)
  return (
    <div className="space-y-3">
      <Notice tone="caution" title="Only continue if you just ran the Wakeshift CLI yourself, on this machine">
        Or your coding agent did, at your request. If someone sent you this link, close this page: continuing gives them a key to your app.
      </Notice>
      <Input
        label="Key code from your terminal"
        placeholder="8 characters, after “Key code:”"
        autoComplete="off"
        autoCapitalize="off"
        autoCorrect="off"
        spellCheck={false}
        maxLength={20}
        value={value}
        onChange={(e) => onChange(e.target.value)}
      />
      {wrong ? (
        <p role="alert" className="text-sm text-danger">
          That isn't this link's key code. Type the one your terminal shows after “Key code:”.
        </p>
      ) : (
        <p className="text-sm text-ink-2">Your terminal, or your coding agent's message, shows it after “Key code:”.</p>
      )}
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
    key_code_required: 'Type the key code your terminal shows.',
    wrong_key_code: "That isn't this link's key code. Type the one your terminal shows, and only if you ran the command yourself.",
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
