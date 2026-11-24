// The console's components, in the marketing site's style:
// paper background, cards with a hairline rule, serif headings, ink pill
// buttons, mono kickers. Pages use these rather than their own classes.
import { isValidElement, useEffect, useId, useRef, useState, type ButtonHTMLAttributes, type InputHTMLAttributes, type ReactNode } from 'react'
import { Link, useLocation, type LinkProps } from 'react-router-dom'
import { Check, Copy, X } from '@phosphor-icons/react'

export function Logo({ size = 'md' }: { size?: 'sm' | 'md' }) {
  return (
    <span className={`flex items-center gap-2.5 font-serif font-semibold tracking-[-0.01em] text-ink ${size === 'sm' ? 'text-[15px]' : 'text-[19px]'}`}>
      <svg viewBox="53 92 295 207" height={size === 'sm' ? 17 : 22} aria-hidden>
        <path d="M200.487 168.326C253.628 168.327 298.187 205.062 310.159 254.523C310.776 257.072 308.113 259.086 305.744 257.961C274.721 243.221 239.126 234.841 201.28 234.841C162.713 234.841 126.485 243.544 95.0481 258.81C92.6885 259.956 90.008 257.963 90.6043 255.409C102.256 205.501 147.03 168.326 200.487 168.326Z" fill="currentColor" />
        <path d="M314.931 206.317C315.323 206.916 315.921 207.366 316.62 207.552C317.318 207.737 318.057 207.646 318.68 207.295C319.302 206.944 319.762 206.359 319.964 205.664C320.166 204.97 320.09 204.226 319.78 203.581C319.78 203.581 319.78 203.581 319.78 203.581C317.676 199.223 315.319 194.971 312.725 190.854C289.641 153.504 245.578 128.783 200.474 129.229C200.474 129.229 200.474 129.229 200.474 129.229C155.37 128.783 111.306 153.504 88.2223 190.854C85.6278 194.971 83.2716 199.223 81.1674 203.581C80.8572 204.226 80.7809 204.97 80.9833 205.664C81.1855 206.359 81.6454 206.944 82.2676 207.295C82.8898 207.646 83.6287 207.737 84.3276 207.552C85.0266 207.366 85.624 206.916 86.0162 206.317C86.0162 206.317 86.0162 206.317 86.0162 206.317C88.5625 202.434 91.2926 198.695 94.1906 195.11C120.226 162.693 160.184 143.84 200.474 144.076C200.474 144.076 200.474 144.076 200.474 144.076C240.763 143.84 280.721 162.693 306.757 195.11C309.655 198.695 312.385 202.434 314.931 206.317Z" fill="currentColor" />
        <path d="M339.449 193.346C339.809 193.969 340.388 194.448 341.081 194.666C341.773 194.884 342.52 194.824 343.16 194.497C343.801 194.171 344.288 193.602 344.519 192.914C344.75 192.226 344.702 191.476 344.41 190.818C344.41 190.818 344.41 190.818 344.41 190.818C341.978 185.365 339.246 180.033 336.224 174.854C309.506 127.956 256.253 95.0378 200.501 95.5767C200.435 95.5766 200.369 95.5765 200.302 95.5765C144.619 94.9737 91.3825 127.747 64.5917 174.53C61.5578 179.702 58.8135 185.028 56.3701 190.475C56.0762 191.133 56.0269 191.882 56.2565 192.571C56.4858 193.26 56.9715 193.83 57.6113 194.158C58.2511 194.486 58.9974 194.547 59.6904 194.331C60.3837 194.115 60.9633 193.637 61.3252 193.014C61.3252 193.014 61.3252 193.014 61.3252 193.014C64.2319 188.019 67.3737 183.184 70.7357 178.529C100.754 136.471 149.92 110.025 200.302 110.423C200.363 110.423 200.423 110.424 200.483 110.424C250.926 110.089 300.111 136.664 330.071 178.839C333.422 183.502 336.553 188.344 339.449 193.346Z" fill="currentColor" />
        <path d="M203.56 251.865C254.582 251.865 302.282 268.363 342.916 289.221C345.992 290.8 344.589 295.299 341.152 294.93C298.647 290.36 247.205 285.648 203.56 285.648C149.481 285.648 106.697 288.902 59.5535 294.934C56.1258 295.373 54.6249 290.943 57.677 289.322C99.6907 267.011 149.781 251.865 203.56 251.865Z" fill="currentColor" />
      </svg>
      Wakeshift
    </span>
  )
}

export function Kicker({ children, className = '' }: { children: ReactNode; className?: string }) {
  return <div className={`font-mono text-xs font-semibold uppercase tracking-[0.12em] text-accent-text ${className}`}>{children}</div>
}

// textOf is the text in a ReactNode: strings and numbers, also inside
// elements and arrays (<>Keys <code>x</code></> is "Keys x").
export function textOf(n: ReactNode): string {
  if (typeof n === 'string' || typeof n === 'number') return String(n)
  if (Array.isArray(n)) return n.map(textOf).join('')
  if (isValidElement<{ children?: ReactNode }>(n)) return textOf(n.props.children)
  return ''
}

// useTitle sets the tab's title, e.g. "Keys · Review App · Wakeshift", so
// tabs, history and screen readers can tell pages apart. Parts may be JSX:
// their text is used.
export function useTitle(...parts: ReactNode[]) {
  const title = [...parts.map((p) => textOf(p).replace(/\s+/g, ' ').trim()).filter((p) => p !== ''), 'Wakeshift'].join(' · ')
  // Again on every navigation, so the page behind a modal (pages/org/OrgModal)
  // takes the title back when it closes.
  const { key } = useLocation()
  useEffect(() => {
    document.title = title
  }, [title, key])
}

// PageHeader: a kicker, the page's title and a line or two on what it's for.
// context, when the kicker doesn't say it, is what the window's title adds
// after the page's (an app's pages: the app's name).
export function PageHeader({ kicker, context, title, children, actions }: { kicker?: ReactNode; context?: ReactNode; title: ReactNode; children?: ReactNode; actions?: ReactNode }) {
  useTitle(title, context ?? kicker)
  return (
    <header className="flex flex-col gap-4 sm:flex-row sm:items-end sm:justify-between">
      <div>
        {kicker && <Kicker className="mb-3">{kicker}</Kicker>}
        <h1 className="font-serif text-[32px] font-normal leading-[1.1] tracking-[-0.015em] sm:text-[40px]">{title}</h1>
        {children && <div className="mt-3 max-w-2xl text-[15px] leading-relaxed text-ink-2">{children}</div>}
      </div>
      {actions && <div className="flex shrink-0 gap-2">{actions}</div>}
    </header>
  )
}

// Modal: a dialog over the page, closed by its X, Escape or a click outside
// it. The same frame as an organization's modal (pages/org/OrgModal).
export function Modal({ open, title, onClose, children }: { open: boolean; title: ReactNode; onClose: () => void; children: ReactNode }) {
  const dialog = useRef<HTMLDialogElement>(null)
  const titleID = useId()
  useEffect(() => {
    const d = dialog.current
    if (!d) return
    if (open && !d.open) d.showModal()
    if (!open && d.open) d.close()
  }, [open])
  return (
    <dialog
      ref={dialog}
      aria-labelledby={titleID}
      className="m-auto max-h-[85vh] w-[min(40rem,calc(100vw-2rem))] flex-col overflow-hidden rounded-xl border border-rule bg-card p-0 text-ink shadow-[0_24px_60px_-24px_rgba(60,40,10,0.45)] backdrop:bg-ink/30 open:flex"
      onCancel={(e) => {
        e.preventDefault()
        onClose()
      }}
      onClick={(e) => e.target === dialog.current && onClose()}
    >
      {open && (
        <>
          <header className="flex items-center justify-between gap-4 px-6 pt-5">
            <h2 id={titleID} className="font-serif text-xl font-normal leading-tight">{title}</h2>
            <button aria-label="Close" className="-mr-2 rounded-md p-2 text-ink-3 hover:bg-paper-2 hover:text-ink" onClick={onClose}>
              <X size={18} aria-hidden />
            </button>
          </header>
          <div className="min-h-0 flex-1 overflow-y-auto px-6 pb-6 pt-4">{children}</div>
        </>
      )}
    </dialog>
  )
}

export function Card({ title, description, children, actions, className = '' }: { title?: ReactNode; description?: ReactNode; children?: ReactNode; actions?: ReactNode; className?: string }) {
  return (
    <section className={`rounded-xl border border-rule bg-card p-5 sm:p-6 ${className}`}>
      {(title || actions) && (
        <div className="mb-4 flex flex-wrap items-center justify-between gap-3">
          {title && <h2 className="font-serif text-xl font-normal leading-tight">{title}</h2>}
          {actions}
        </div>
      )}
      {description && <p className="-mt-2 mb-4 text-sm leading-relaxed text-ink-2">{description}</p>}
      {children}
    </section>
  )
}

type Variant = 'primary' | 'secondary' | 'tertiary' | 'danger'
type Size = 'sm' | 'md'

function buttonClass(variant: Variant, size: Size, className = '') {
  const v = {
    primary: 'border-ink bg-ink text-paper hover:bg-black',
    secondary: 'border-rule bg-transparent text-ink hover:border-ink-3',
    tertiary: 'border-transparent bg-transparent text-accent-text hover:bg-paper-2',
    danger: 'border-rule bg-transparent text-danger hover:border-danger/50',
  }[variant]
  const s = size === 'sm' ? 'px-3.5 py-1.5 text-[13px]' : 'px-5 py-2.5 text-sm'
  return `inline-flex items-center justify-center gap-2 whitespace-nowrap rounded-full border font-semibold transition-colors disabled:cursor-default disabled:opacity-45 ${v} ${s} ${className}`
}

export function Button({ variant = 'primary', size = 'md', className = '', ...props }: ButtonHTMLAttributes<HTMLButtonElement> & { variant?: Variant; size?: Size }) {
  return <button className={buttonClass(variant, size, className)} {...props} />
}

// ButtonLink is a link that looks like a Button.
export function ButtonLink({ variant = 'primary', size = 'md', className = '', ...props }: LinkProps & { variant?: Variant; size?: Size }) {
  return <Link className={buttonClass(variant, size, className)} {...props} />
}

export const linkClass = 'text-accent-text underline decoration-accent-text/30 underline-offset-[3px] hover:decoration-accent-text'

export function TextLink(props: LinkProps) {
  return <Link className={linkClass} {...props} />
}

const fieldClass = 'w-full rounded-lg border border-rule bg-card px-3 py-2 text-sm text-ink placeholder:text-ink-3 focus:border-accent focus:outline-hidden'

export function Input({ label, ...props }: InputHTMLAttributes<HTMLInputElement> & { label: string }) {
  return (
    <label className="block text-sm">
      <span className="mb-1.5 block font-medium text-ink-2">{label}</span>
      <input className={fieldClass} {...props} />
    </label>
  )
}

// The design system's dropdown, in its own file with its styles.
export { Select } from './Select'

export function Hint({ children }: { children: ReactNode }) {
  return <p className="text-xs leading-relaxed text-ink-3">{children}</p>
}

export function ErrorText({ error }: { error: Error | null | undefined }) {
  return error ? <p className="mt-2 text-sm text-danger">{error.message}</p> : null
}

export function Loading() {
  return <p className="text-sm text-ink-3">Loading…</p>
}

// Empty is what a list shows with nothing in it: what would be here, and
// how to get it there.
export function Empty({ title, children, action }: { title?: ReactNode; children?: ReactNode; action?: ReactNode }) {
  return (
    <div className="rounded-xl border border-dashed border-ink-3/60 px-5 py-6 text-sm">
      {title && <p className="font-serif text-lg text-ink">{title}</p>}
      {children && <div className={`leading-relaxed text-ink-2 ${title ? 'mt-1' : ''}`}>{children}</div>}
      {action && <div className="mt-4">{action}</div>}
    </div>
  )
}

export function Badge({ tone = 'neutral', children }: { tone?: 'neutral' | 'accent' | 'sage' | 'caution' | 'danger'; children: ReactNode }) {
  // Text colors are darker than the brand's where it's needed for 4.5:1 on
  // the badge's background.
  const t = {
    neutral: 'border border-rule text-ink-3',
    accent: 'bg-accent-soft text-[#A0420E]',
    danger: 'bg-[#FBE4E1] text-danger',
    sage: 'bg-sage-soft text-sage',
    caution: 'bg-caution-bg text-caution',
  }[tone]
  return <span className={`inline-flex items-center rounded-full px-2 py-0.5 font-mono text-[10px] font-semibold uppercase tracking-[0.08em] ${t}`}>{children}</span>
}

export function Notice({ tone = 'info', title, children }: { tone?: 'info' | 'caution'; title?: ReactNode; children?: ReactNode }) {
  const t = tone === 'caution' ? 'border-caution-rule bg-caution-bg text-[#6E4200]' : 'border-rule bg-paper text-ink-2'
  return (
    <div className={`rounded-lg border px-4 py-3 text-sm leading-relaxed ${t}`}>
      {title && <p className={`font-semibold ${tone === 'caution' ? 'text-caution' : 'text-ink'}`}>{title}</p>}
      {children}
    </div>
  )
}

// Rows is a divided list; Row is one item: what it is on the left, what you
// can do to it on the right (below it, on a phone).
export function Rows({ children }: { children: ReactNode }) {
  return <ul className="-my-3 divide-y divide-rule">{children}</ul>
}

export function Row({ children, actions, muted }: { children: ReactNode; actions?: ReactNode; muted?: boolean }) {
  return (
    <li className={`flex flex-col gap-3 py-3 text-sm sm:flex-row sm:items-center sm:justify-between sm:gap-4 ${muted ? 'text-ink-3' : ''}`}>
      <div className="min-w-0">{children}</div>
      {actions && <div className="flex shrink-0 flex-wrap gap-2">{actions}</div>}
    </li>
  )
}

export function Meta({ children }: { children: ReactNode }) {
  return <div className="mt-0.5 text-xs leading-relaxed text-ink-3">{children}</div>
}

// Facts is a short list of labels and values.
export function Facts({ items }: { items: [string, ReactNode][] }) {
  return (
    <dl className="grid grid-cols-[7.5rem_1fr] gap-x-4 gap-y-2 text-sm sm:grid-cols-[10rem_1fr]">
      {items.map(([k, v]) => (
        <div key={k} className="contents">
          <dt className="text-ink-3">{k}</dt>
          <dd className="min-w-0 wrap-break-word">{v}</dd>
        </div>
      ))}
    </dl>
  )
}

export const thClass = 'pb-2 pr-4 text-left font-mono text-[11px] font-semibold uppercase tracking-widest text-ink-3'

// Secret shows a value to copy once (an app key, a stream link) with what
// it's for, and Copy and Done.
// TabLabel is a tab's label, as wide as it is in bold whether the tab is
// open or not, so the row doesn't shift as tabs are clicked.
export function TabLabel({ label, active }: { label: string; active: boolean }) {
  return (
    <span className="inline-grid">
      <span aria-hidden className="invisible col-start-1 row-start-1 font-semibold">
        {label}
      </span>
      <span className={`col-start-1 row-start-1 text-center ${active ? 'font-semibold' : ''}`}>{label}</span>
    </span>
  )
}

// Secret is a value shown once, with a copy button in its box, and Done
// when the page has somewhere to dismiss it to (onDone).
export function Secret({ id, value, children, onDone }: { id: string; value: string; children?: ReactNode; onDone?: () => void }) {
  const [copied, setCopied] = useState<'yes' | 'select' | null>(null)
  return (
    <div>
      {children && <div className="mb-3 text-sm leading-relaxed text-ink-2">{children}</div>}
      <div className="flex items-start gap-2 rounded-lg border border-rule bg-paper py-1 pl-3 pr-1">
        <code id={id} className="block min-w-0 flex-1 break-all py-2 text-xs text-ink">
          {value}
        </code>
        <button
          type="button"
          className="shrink-0 rounded-md p-2 text-ink-2 transition-colors hover:bg-paper-2 hover:text-ink focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-ink"
          aria-label={copied === 'yes' ? 'Copied' : 'Copy'}
          title={copied === 'yes' ? 'Copied' : 'Copy'}
          onClick={async () => setCopied((await copy(value, document.getElementById(id))) ? 'yes' : 'select')}
        >
          {copied === 'yes' ? <Check size={16} aria-hidden /> : <Copy size={16} aria-hidden />}
        </button>
      </div>
      {(onDone || copied) && (
        <div className="mt-3 flex flex-wrap items-center gap-2">
          {onDone && (
            <Button size="sm" variant="secondary" onClick={onDone}>
              Done
            </Button>
          )}
          {copied === 'yes' && <span className="text-sm text-sage">Copied</span>}
          {copied === 'select' && <span className="text-sm text-ink-3">Selected: press ⌘C to copy</span>}
        </div>
      )}
    </div>
  )
}

// Standalone is a page outside the console (sign-in, joining):
// the logo, then one card in the middle of the paper.
export function Standalone({ children, wide }: { children: ReactNode; wide?: boolean }) {
  return (
    <div className="flex min-h-screen flex-col items-center px-4 pb-10 pt-14 sm:pt-20">
      <div className="mb-6">
        <Logo />
      </div>
      <div className={`w-full rounded-2xl border border-rule bg-card p-6 shadow-[0_12px_30px_-18px_rgba(60,40,10,0.25)] sm:p-7 ${wide ? 'max-w-120' : 'max-w-104'}`}>
        {children}
      </div>
    </div>
  )
}

// copy puts text on the clipboard, falling back to selecting it where the
// Clipboard API isn't available (plain-http origins other than localhost).
export async function copy(text: string, fallback: HTMLElement | null): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text)
    return true
  } catch {
    if (fallback) window.getSelection()?.selectAllChildren(fallback)
    return false
  }
}

export function when(iso: string) {
  return new Date(iso).toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' })
}

export const plural = (n: number, one: string) => `${n} ${one}${n === 1 ? '' : 's'}`
