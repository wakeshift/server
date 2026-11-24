import { useCallback, useSyncExternalStore } from 'react'
import { useQuery } from '@tanstack/react-query'
import { listOrgs, type Org } from '../api/orgs'

// Which organization is current is kept in the browser, not the session
// (whose cookie names a workspace): the one chosen last, else your first.
const key = 'wakeshift.org'
const listeners = new Set<() => void>()

function read(): string | null {
  try {
    return localStorage.getItem(key)
  } catch {
    return null
  }
}

function subscribe(fn: () => void) {
  listeners.add(fn)
  const storage = (e: StorageEvent) => e.key === key && fn()
  window.addEventListener('storage', storage)
  return () => {
    listeners.delete(fn)
    window.removeEventListener('storage', storage)
  }
}

export function chooseOrg(id: string) {
  try {
    localStorage.setItem(key, id)
  } catch {
    // Private mode: the choice lasts this page only.
  }
  listeners.forEach((fn) => fn())
}

export const useOrgs = () => useQuery({ queryKey: ['orgs'], queryFn: listOrgs })

// useCurrentOrg is the organization the console is showing, and a way to
// switch.
export function useCurrentOrg(): { orgs: ReturnType<typeof useOrgs>; current?: Org; choose: (id: string) => void } {
  const orgs = useOrgs()
  const stored = useSyncExternalStore(subscribe, read)
  const current = orgs.data?.find((o) => o.id === stored) ?? orgs.data?.[0]
  const choose = useCallback((id: string) => chooseOrg(id), [])
  return { orgs, current, choose }
}
