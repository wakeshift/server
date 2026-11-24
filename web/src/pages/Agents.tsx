import { useState, type FormEvent } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { createAgent, listAgents, revokeStreamLinks, streamLink } from '../api/client'
import { Button, Empty, ErrorText, Input, Loading, Modal, Notice, PageHeader, Secret, when } from '../components/ui'
import { Clock, Key } from '@phosphor-icons/react'
import { AgentLogos } from '../components/AgentLogos'

const monitorPrompt = 'Use the Monitor tool to watch this WebSocket and handle each event.'

export default function Agents() {
  const qc = useQueryClient()
  const agents = useQuery({ queryKey: ['agents'], queryFn: listAgents })
  const [id, setID] = useState('')
  const [name, setName] = useState('')
  const [link, setLink] = useState<{ agent: string; url: string; expires: string } | null>(null)
  const [revoked, setRevoked] = useState<string | null>(null)
  // The agent whose links Revoke links is asking to revoke.
  const [confirming, setConfirming] = useState<string | null>(null)
  // The modal: the add form, then (after adding, or from a row's Stream
  // link) the agent's stream link.
  const [step, setStep] = useState<'add' | 'link' | null>(null)

  const getLink = useMutation({
    mutationFn: (agent: string) => streamLink(agent).then((r) => ({ agent, url: r.url, expires: r.expires_at })),
    onSuccess: (l) => {
      setLink(l)
      setRevoked(null)
    },
  })
  const showLink = (agent: string) => {
    setLink(null)
    setStep('link')
    getLink.mutate(agent)
  }
  const create = useMutation({
    mutationFn: (a: { id: string; name: string }) => createAgent(a.id, a.name),
    onSuccess: (_, a) => {
      setID('')
      setName('')
      qc.invalidateQueries({ queryKey: ['agents'] })
      showLink(a.id)
    },
  })
  const close = () => {
    setStep(null)
    setLink(null)
    getLink.reset()
  }
  const revoke = useMutation({
    mutationFn: (agent: string) => revokeStreamLinks(agent).then((r) => ({ agent, n: r.revoked })),
    onSuccess: ({ agent, n }) => {
      if (link?.agent === agent) setLink(null)
      setConfirming(null)
      setRevoked(`Revoked ${n} link${n === 1 ? '' : 's'} for ${agent}; open streams on them have closed.`)
    },
  })

  return (
    <div className="space-y-8">
      <PageHeader title="Agents">
        An agent is a lasting name for one of your agents, usually the program it runs in: <code>claude-code</code>,{' '}
        <code>codex</code>. Its stream link
        delivers the events its subscriptions match. Apps you build don't need these: Wakeshift makes agents in the
        workspaces it keeps for your app's users.
        <AgentLogos className="mt-4" />
        <div className="mt-5">
          <Button onClick={() => setStep('add')}>Add an agent</Button>
        </div>
      </PageHeader>

      {revoked && <Notice>{revoked}</Notice>}

      {agents.isPending && <Loading />}
      {!!agents.data?.length && (
        <div className="space-y-3">
          {agents.data.map((a) => (
            <section key={a.id} className="flex flex-col gap-3 rounded-xl border border-rule bg-card px-5 py-4 sm:flex-row sm:items-center sm:justify-between sm:px-6">
              <div className="min-w-0">
                <h2 className="truncate font-serif text-xl font-normal leading-tight">{a.id}</h2>
                <p className="mt-1 text-sm text-ink-2">
                  {a.display_name && a.display_name !== a.id && <>{a.display_name} · </>}
                  <span className="text-ink-3">Added {when(a.created_at)}</span>
                </p>
              </div>
              <div className="flex shrink-0 flex-wrap gap-2">
                <Button size="sm" variant="secondary" onClick={() => showLink(a.id)} disabled={getLink.isPending}>
                  Stream link
                </Button>
                <Button
                  size="sm"
                  variant="danger"
                  onClick={() => {
                    revoke.reset()
                    setConfirming(a.id)
                  }}
                  disabled={revoke.isPending && revoke.variables === a.id}
                >
                  Revoke links
                </Button>
              </div>
            </section>
          ))}
        </div>
      )}
      <ErrorText error={agents.error} />

      <Modal open={confirming !== null} title={`Revoke ${confirming}'s stream links?`} onClose={() => setConfirming(null)}>
        <p className="text-sm leading-relaxed text-ink-2">
          Every stream link for <span className="font-medium text-ink">{confirming}</span> stops working, and open streams on them close at once. To
          reconnect it, get a new stream link.
        </p>
        <ErrorText error={revoke.error} />
        <div className="mt-5 flex flex-wrap gap-2">
          <Button variant="danger" onClick={() => confirming && revoke.mutate(confirming)} disabled={revoke.isPending}>
            Revoke links
          </Button>
          <Button variant="secondary" onClick={() => setConfirming(null)}>
            Cancel
          </Button>
        </div>
      </Modal>

      <Modal open={step !== null} title={step === 'link' ? `Stream link for ${link?.agent ?? getLink.variables ?? ''}` : 'Add an agent'} onClose={close}>
        {step === 'add' && (
          <>
            <form
              className="flex flex-col gap-4"
              onSubmit={(e: FormEvent) => {
                e.preventDefault()
                create.mutate({ id, name })
              }}
            >
              <Input label="ID" placeholder="claude-code" value={id} onChange={(e) => setID(e.target.value)} />
              <Input label="Display name" placeholder="Claude Code" value={name} onChange={(e) => setName(e.target.value)} />
              <Button type="submit" className="self-start" disabled={!id || create.isPending}>
                Add
              </Button>
            </form>
            <ErrorText error={create.error} />
          </>
        )}
        {step === 'link' && (
          <>
            {getLink.isPending && <Loading />}
            <ErrorText error={getLink.error} />
            {link && (
              <div className="flex flex-col gap-5">
                <ol className="flex flex-col gap-5">
                  <li className="grid grid-cols-[1.25rem_1fr]">
                    <span className="text-sm font-semibold leading-relaxed text-accent-text">1</span>
                    <Secret id="stream-link" value={link.url}>
                      Give this link to the agent.
                    </Secret>
                  </li>
                  <li className="grid grid-cols-[1.25rem_1fr]">
                    <span className="text-sm font-semibold leading-relaxed text-accent-text">2</span>
                    <Secret id="stream-prompt" value={monitorPrompt}>
                      In Claude Code, for example, send it with:
                    </Secret>
                  </li>
                </ol>
                <ul className="space-y-1 text-sm leading-relaxed text-ink-2">
                  <li className="grid grid-cols-[1.25rem_1fr] items-center">
                    <Clock size={15} className="text-ink-3" aria-hidden />
                    <span>Works until {when(link.expires)}, or until you revoke it.</span>
                  </li>
                  <li className="grid grid-cols-[1.25rem_1fr] items-center">
                    <Key size={15} className="text-ink-3" aria-hidden />
                    <span>Anyone with it receives this agent's events.</span>
                  </li>
                </ul>
              </div>
            )}
          </>
        )}
      </Modal>
      {agents.data?.length === 0 && <Empty title="No agents yet">Use Add an agent to add one and try a wake-up by hand.</Empty>}
    </div>
  )
}
