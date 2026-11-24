// Select: the design system's dropdown, a button that opens a listbox, in place of a
// native <select>. Pages still pass <option> children and read
// e.target.value, so callers didn't change.
import { Children, isValidElement, useEffect, useId, useLayoutEffect, useRef, useState, type CSSProperties, type KeyboardEvent, type ReactElement, type ReactNode } from 'react'
import { CaretDown, Check } from '@phosphor-icons/react'
import './select.css'

type Option = { value: string; label: string }

// The text an option shows: its children joined ("orders (personal)").
function textOf(node: ReactNode): string {
  if (node == null || typeof node === 'boolean') return ''
  if (typeof node === 'string' || typeof node === 'number') return String(node)
  if (Array.isArray(node)) return node.map(textOf).join('')
  if (isValidElement<{ children?: ReactNode }>(node)) return textOf(node.props.children)
  return ''
}

function optionsOf(children: ReactNode): Option[] {
  return Children.toArray(children)
    .filter((c): c is ReactElement<{ value?: string | number; children?: ReactNode }> => isValidElement(c))
    .map((c) => {
      const label = textOf(c.props.children)
      return { value: c.props.value != null ? String(c.props.value) : label, label }
    })
}

export function Select({
  label,
  ariaLabel,
  value,
  onChange,
  children,
  disabled,
  placeholder = 'Select…',
}: {
  // label is shown above the field; ariaLabel names a field with no label
  // shown (a role in a row of people).
  label?: string
  ariaLabel?: string
  value: string
  onChange: (e: { target: { value: string } }) => void
  children: ReactNode
  disabled?: boolean
  placeholder?: string
}) {
  const options = optionsOf(children)
  const [open, setOpen] = useState(false)
  const [active, setActive] = useState(0)
  const [menuStyle, setMenuStyle] = useState<CSSProperties>()
  const rootRef = useRef<HTMLDivElement>(null)
  const menuRef = useRef<HTMLUListElement>(null)
  const selected = options.find((o) => o.value === value)
  const baseID = useId()
  const labelID = `${baseID}-label`
  const listboxID = `${baseID}-listbox`
  const optionID = (i: number) => `${baseID}-option-${i}`
  const activeID = open && options[active] ? optionID(active) : undefined

  useEffect(() => {
    if (activeID) document.getElementById(activeID)?.scrollIntoView?.({ block: 'nearest' })
  }, [activeID])

  useEffect(() => {
    if (!open) return
    const onPointer = (e: MouseEvent) => {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) setOpen(false)
    }
    document.addEventListener('mousedown', onPointer)
    return () => document.removeEventListener('mousedown', onPointer)
  }, [open])

  // Keep the menu inside the nearest clipping box (a modal's body).
  useLayoutEffect(() => {
    const menu = menuRef.current
    const root = rootRef.current
    if (!open || !menu || !root) return
    let left = 0
    let right = window.innerWidth
    let top = 0
    let bottom = window.innerHeight
    for (let node = root.parentElement; node; node = node.parentElement) {
      if (getComputedStyle(node).overflowX === 'visible') continue
      const bounds = node.getBoundingClientRect()
      left = Math.max(left, bounds.left)
      right = Math.min(right, bounds.right)
      top = Math.max(top, bounds.top)
      bottom = Math.min(bottom, bounds.bottom)
      break
    }
    const rect = menu.getBoundingClientRect()
    const trigger = root.getBoundingClientRect()
    const style: CSSProperties = {}
    // Not ours (the library's opens down only): up, when it would be cut off
    // below and there's more room above, as on a modal's last row.
    if (rect.bottom > bottom - 8 && trigger.top - top > bottom - trigger.bottom) Object.assign(style, { top: 'auto', bottom: 'calc(100% + 6px)' })
    if (rect.right > right - 8) {
      if (trigger.right - rect.width >= left + 8) Object.assign(style, { left: 'auto', right: 0 })
      else style.maxWidth = Math.max(trigger.width, right - rect.left - 8)
    }
    if (Object.keys(style).length) setMenuStyle(style)
  }, [open])

  const openMenu = () => {
    const i = options.findIndex((o) => o.value === value)
    setActive(i < 0 ? 0 : i)
    setMenuStyle(undefined)
    setOpen(true)
  }
  const choose = (next: string) => {
    if (next !== value) onChange({ target: { value: next } })
    setOpen(false)
  }

  const onKeyDown = (e: KeyboardEvent) => {
    if (disabled) return
    switch (e.key) {
      case 'ArrowDown':
        e.preventDefault()
        if (!open) return openMenu()
        setActive((a) => Math.min(options.length - 1, a + 1))
        break
      case 'ArrowUp':
        e.preventDefault()
        if (!open) return openMenu()
        setActive((a) => Math.max(0, a - 1))
        break
      case 'Enter':
      case ' ':
        e.preventDefault()
        if (!open) openMenu()
        else if (options[active]) choose(options[active].value)
        break
      case 'Home':
        if (!open) return
        e.preventDefault()
        setActive(0)
        break
      case 'End':
        if (!open) return
        e.preventDefault()
        setActive(Math.max(0, options.length - 1))
        break
      case 'Escape':
        // Close the menu, not the modal around it.
        if (open) {
          e.preventDefault()
          e.stopPropagation()
          setOpen(false)
        }
        break
      case 'Tab':
        setOpen(false)
        break
    }
  }

  return (
    <div className="block text-sm">
      {label && (
        <span id={labelID} className="mb-1.5 block font-medium text-ink-2">
          {label}
        </span>
      )}
      <div className="uiSelect" ref={rootRef}>
        <button
          type="button"
          className="uiSelectTrigger"
          aria-haspopup="listbox"
          aria-expanded={open}
          aria-controls={open ? listboxID : undefined}
          aria-activedescendant={activeID}
          aria-labelledby={label ? `${labelID} ${baseID}-value` : undefined}
          aria-label={label ? undefined : ariaLabel}
          disabled={disabled}
          onClick={() => (open ? setOpen(false) : openMenu())}
          onKeyDown={onKeyDown}
        >
          <span id={`${baseID}-value`} className={selected ? 'uiSelectValue' : 'uiSelectPlaceholder'}>
            {selected ? selected.label : placeholder}
          </span>
          <CaretDown className="uiSelectChevron" size={16} aria-hidden />
        </button>
        {open && (
          <ul ref={menuRef} className="uiSelectMenu" role="listbox" aria-labelledby={label ? labelID : undefined} aria-label={label ? undefined : ariaLabel} tabIndex={-1} id={listboxID} style={menuStyle}>
            {options.map((o, i) => (
              <li
                key={o.value}
                id={optionID(i)}
                role="option"
                aria-selected={o.value === value}
                className={`uiSelectOption${i === active ? ' active' : ''}${o.value === value ? ' selected' : ''}`}
                onMouseEnter={() => setActive(i)}
                onMouseDown={(e) => {
                  e.preventDefault()
                  choose(o.value)
                }}
              >
                <span title={o.label}>{o.label}</span>
                {o.value === value && <Check className="uiSelectCheck" size={15} aria-hidden />}
              </li>
            ))}
          </ul>
        )}
      </div>
    </div>
  )
}
