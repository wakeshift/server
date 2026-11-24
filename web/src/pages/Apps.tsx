import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { apiFetch, listApps } from '../api/client'
import { Button, Card, Empty, ErrorText, Loading, Meta, PageHeader, Row, Rows, when } from '../components/ui'

// Apps with access left in this workspace from before apps stopped signing
// people in with Wakeshift. Apps keep their own users now, so nothing new
// is connected here; these are being removed, and can be revoked now.
export default function Apps() {
  const qc = useQueryClient()
  const list = useQuery({ queryKey: ['apps'], queryFn: listApps })
  const revoke = useMutation({
    mutationFn: (id: string) => apiFetch(`/api/v1/grants/${encodeURIComponent(id)}/revoke`, { method: 'POST' }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['apps'] }),
  })
  const used = list.data ?? []

  return (
    <div className="space-y-8">
      <PageHeader title="Connected apps">
        Apps on Wakeshift keep their own users, so you manage them in the app. These still have access here from when
        apps could act in your workspace. Wakeshift is removing it; revoke it now to end it at once.
      </PageHeader>
      {list.isPending && <Loading />}
      <ErrorText error={list.error ?? revoke.error} />

      {list.isSuccess && used.length === 0 && <Empty title="No apps have access to this workspace" />}
      {used.map((app) => (
        <Card key={app.id} title={app.name} description={app.description || undefined}>
          <Rows>
            {app.grants.map((g) => (
              <Row
                key={g.id}
                actions={
                  <Button
                    size="sm"
                    variant="danger"
                    disabled={revoke.isPending && revoke.variables === g.id}
                    onClick={() => {
                      if (window.confirm(`Revoke ${app.name}'s access? What it set up is undone.`)) revoke.mutate(g.id)
                    }}
                  >
                    Revoke
                  </Button>
                }
              >
                {g.kind === 'agent' ? (
                  <>
                    Acts for <span className="font-medium">{g.agent_id}</span>
                  </>
                ) : (
                  'Connected to this workspace'
                )}
                <Meta>Until {when(g.expires_at)}</Meta>
              </Row>
            ))}
          </Rows>
        </Card>
      ))}
    </div>
  )
}
