// What the organization's pages share: the organization from the URL and
// the tab's intro.
import type { ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import { useParams } from 'react-router-dom'
import { ApiError } from '../../api/client'
import { getOrg, type Org } from '../../api/orgs'
import { Empty, ErrorText, Loading, TextLink, useTitle } from '../../components/ui'

export function useOrgFromURL() {
  const { org: id = '' } = useParams()
  const org = useQuery({ queryKey: ['org', id], queryFn: () => getOrg(id) })
  return { id, org }
}

// OrgPage loads the organization named in the URL and shows its page (a tab
// of OrgModal, which shows the organization's name and the tab's title), or
// says it isn't yours (one you're not in reads as one that doesn't exist).
export function OrgPage({ title, intro, actions, children }: { title: string; intro?: ReactNode; actions?: ReactNode; children: (org: Org) => ReactNode }) {
  const { org } = useOrgFromURL()
  useTitle(title, org.data?.name)
  if (org.isPending) return <Loading />
  if (org.isError) {
    if (org.error instanceof ApiError && org.error.status === 404) {
      return (
        <Empty title="No such organization">
          You're not in an organization with that ID. <TextLink to="/">See your apps</TextLink>.
        </Empty>
      )
    }
    return <ErrorText error={org.error} />
  }
  return (
    <div className="space-y-6">
      {(intro || actions) && (
        <div className="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
          {intro && <div className="max-w-xl text-sm leading-relaxed text-ink-2">{intro}</div>}
          {actions && <div className="flex shrink-0 gap-2">{actions}</div>}
        </div>
      )}
      {children(org.data)}
    </div>
  )
}

// actorLabel is who did something, as a log shows it.
export function actorLabel(actor: string, email?: string) {
  if (email) return email
  if (actor === 'hub') return 'Wakeshift'
  return actor
}
